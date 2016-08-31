// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"

	"github.com/keybase/client/go/logger"
	"golang.org/x/net/context"
)

// blockJournal stores a single ordered list of block operations for a
// single TLF, along with the associated block data, in flat files in
// a directory on disk.
//
// The directory layout looks like:
//
// dir/block_journal/EARLIEST
// dir/block_journal/LATEST
// dir/block_journal/0...000
// dir/block_journal/0...001
// dir/block_journal/0...fff
// dir/blocks/0100/0...01/data
// dir/blocks/0100/0...01/key_server_half
// ...
// dir/blocks/01ff/f...ff/data
// dir/blocks/01ff/f...ff/key_server_half
//
// Each entry in the journal in dir/block_journal contains the
// mutating operation and arguments for a single operation, except for
// block data. (See diskJournal comments for more details about the
// journal.)
//
// The block data is stored separately in dir/blocks. Each block has
// its own subdirectory with its ID as a name.  The block
// subdirectories are splayed over (# of possible hash types) * 256
// subdirectories -- one byte for the hash type (currently only one)
// plus the first byte of the hash data -- using the first four
// characters of the name to keep the number of directories in dir
// itself to a manageable number, similar to git. Each block directory
// has data, which is the raw block data that should hash to the block
// ID, and key_server_half, which contains the raw data for the
// associated key server half.
//
// blockJournal is not goroutine-safe, so any code that uses it must
// guarantee that only one goroutine at a time calls its functions.
type blockJournal struct {
	codec  Codec
	crypto cryptoPure
	dir    string

	log      logger.Logger
	deferLog logger.Logger

	j          diskJournal
	refs       map[BlockID]blockRefMap
	isShutdown bool

	// Tracks the total size of on-disk blocks that will be put to the
	// server (i.e., does not count reference adds).  It is only
	// accurate for users of this journal that properly flush entries;
	// in particular, direct calls to `removeReferences` can cause
	// this count to deviate from the actual disk usage of the
	// journal.
	blockBytes int64
}

type bserverOpName string

const (
	blockPutOp    bserverOpName = "blockPut"
	addRefOp      bserverOpName = "addReference"
	removeRefsOp  bserverOpName = "removeReferences"
	archiveRefsOp bserverOpName = "archiveReferences"
)

// A blockJournalEntry is just the name of the operation and the
// associated block ID and contexts. Fields are exported only for
// serialization.
type blockJournalEntry struct {
	// Must be one of the four ops above.
	Op bserverOpName
	// Must have exactly one entry with one context for blockPutOp
	// and addRefOp.
	Contexts map[BlockID][]BlockContext
}

// Get the single context stored in this entry. Only applicable to
// blockPutOp and addRefOp.
func (e blockJournalEntry) getSingleContext() (
	BlockID, BlockContext, error) {
	switch e.Op {
	case blockPutOp, addRefOp:
		if len(e.Contexts) != 1 {
			return BlockID{}, BlockContext{}, fmt.Errorf(
				"Op %s doesn't have exactly one context: %v",
				e.Op, e.Contexts)
		}
		for id, idContexts := range e.Contexts {
			if len(idContexts) != 1 {
				return BlockID{}, BlockContext{}, fmt.Errorf(
					"Op %s doesn't have exactly one context for id=%s: %v",
					e.Op, id, idContexts)
			}
			return id, idContexts[0], nil
		}
	}

	return BlockID{}, BlockContext{}, fmt.Errorf(
		"getSingleContext() erroneously called on op %s", e.Op)
}

// makeBlockJournal returns a new blockJournal for the given
// directory. Any existing journal entries are read.
func makeBlockJournal(
	ctx context.Context, codec Codec, crypto cryptoPure, dir string,
	log logger.Logger) (*blockJournal, error) {
	journalPath := filepath.Join(dir, "block_journal")
	deferLog := log.CloneWithAddedDepth(1)
	j := makeDiskJournal(
		codec, journalPath, reflect.TypeOf(blockJournalEntry{}))
	journal := &blockJournal{
		codec:    codec,
		crypto:   crypto,
		dir:      dir,
		log:      log,
		deferLog: deferLog,
		j:        j,
	}

	refs, err := journal.readJournal(ctx)
	if err != nil {
		return nil, err
	}

	journal.refs = refs
	return journal, nil
}

// The functions below are for building various non-journal paths.

func (j *blockJournal) blocksPath() string {
	return filepath.Join(j.dir, "blocks")
}

func (j *blockJournal) blockPath(id BlockID) string {
	idStr := id.String()
	return filepath.Join(j.blocksPath(), idStr[:4], idStr[4:])
}

func (j *blockJournal) blockDataPath(id BlockID) string {
	return filepath.Join(j.blockPath(id), "data")
}

func (j *blockJournal) keyServerHalfPath(id BlockID) string {
	return filepath.Join(j.blockPath(id), "key_server_half")
}

// The functions below are for reading and writing journal entries.

func (j *blockJournal) readJournalEntry(ordinal journalOrdinal) (
	blockJournalEntry, error) {
	entry, err := j.j.readJournalEntry(ordinal)
	if err != nil {
		return blockJournalEntry{}, err
	}

	return entry.(blockJournalEntry), nil
}

// readJournal reads the journal and returns a map of all the block
// references in the journal.
func (j *blockJournal) readJournal(ctx context.Context) (
	map[BlockID]blockRefMap, error) {
	refs := make(map[BlockID]blockRefMap)

	first, err := j.j.readEarliestOrdinal()
	if os.IsNotExist(err) {
		return refs, nil
	} else if err != nil {
		return nil, err
	}
	last, err := j.j.readLatestOrdinal()
	if err != nil {
		return nil, err
	}

	j.log.CDebugf(ctx, "Reading journal entries %d to %d", first, last)

	var blockBytes int64
	for i := first; i <= last; i++ {
		e, err := j.readJournalEntry(i)
		if err != nil {
			return nil, err
		}

		// Handle single ops separately.
		switch e.Op {
		case blockPutOp, addRefOp:
			id, context, err := e.getSingleContext()
			if err != nil {
				return nil, err
			}

			blockRefs := refs[id]
			if blockRefs == nil {
				blockRefs = make(blockRefMap)
				refs[id] = blockRefs
			}

			err = blockRefs.put(context, liveBlockRef)
			if err != nil {
				return nil, err
			}

			// Only puts count as bytes, on the assumption that the
			// refs won't have to upload any new bytes.  (This might
			// be wrong if all references to a block were deleted
			// since the addref entry was appended.)
			if e.Op == blockPutOp {
				b, err := j.getDataSize(id)
				// Ignore ENOENT errors, since users like
				// BlockServerDisk can remove block data without
				// deleting the corresponding addRef.
				if err != nil && !os.IsNotExist(err) {
					return nil, err
				}
				blockBytes += b
			}

			continue
		}

		for id, idContexts := range e.Contexts {
			blockRefs := refs[id]

			switch e.Op {
			case removeRefsOp:
				if blockRefs == nil {
					// All refs are already gone,
					// which is not an error.
					continue
				}

				for _, context := range idContexts {
					err := blockRefs.remove(context)
					if err != nil {
						return nil, err
					}
				}

				if len(blockRefs) == 0 {
					delete(refs, id)
				}

			case archiveRefsOp:
				if blockRefs == nil {
					blockRefs = make(blockRefMap)
					refs[id] = blockRefs
				}

				for _, context := range idContexts {
					err := blockRefs.put(
						context, archivedBlockRef)
					if err != nil {
						return nil, err
					}
				}

			default:
				return nil, fmt.Errorf("Unknown op %s", e.Op)
			}
		}
	}
	j.log.CDebugf(ctx, "Found %d block bytes in the journal", blockBytes)
	j.blockBytes = blockBytes
	return refs, nil
}

func (j *blockJournal) writeJournalEntry(
	ordinal journalOrdinal, entry blockJournalEntry) error {
	return j.j.writeJournalEntry(ordinal, entry)
}

func (j *blockJournal) appendJournalEntry(
	op bserverOpName, contexts map[BlockID][]BlockContext) error {
	return j.j.appendJournalEntry(nil, blockJournalEntry{
		Op:       op,
		Contexts: contexts,
	})
}

func (j *blockJournal) length() (uint64, error) {
	return j.j.length()
}

func (j *blockJournal) getRefEntry(
	id BlockID, refNonce BlockRefNonce) (blockRefEntry, error) {
	refs := j.refs[id]
	if refs == nil {
		return blockRefEntry{}, BServerErrorBlockNonExistent{}
	}

	e, ok := refs[refNonce]
	if !ok {
		return blockRefEntry{}, BServerErrorBlockNonExistent{}
	}

	return e, nil
}

func (j *blockJournal) putRefEntry(
	id BlockID, refEntry blockRefEntry) error {
	existingRefEntry, err := j.getRefEntry(
		id, refEntry.Context.GetRefNonce())
	var exists bool
	switch err.(type) {
	case BServerErrorBlockNonExistent:
		exists = false
	case nil:
		exists = true
	default:
		return err
	}

	if exists {
		err = existingRefEntry.checkContext(refEntry.Context)
		if err != nil {
			return err
		}
	}

	if j.refs[id] == nil {
		j.refs[id] = make(blockRefMap)
	}

	return j.refs[id].put(refEntry.Context, refEntry.Status)
}

func (j *blockJournal) getDataSize(id BlockID) (int64, error) {
	fi, err := os.Stat(j.blockDataPath(id))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func (j *blockJournal) getData(id BlockID) (
	[]byte, BlockCryptKeyServerHalf, error) {
	data, err := ioutil.ReadFile(j.blockDataPath(id))
	if os.IsNotExist(err) {
		return nil, BlockCryptKeyServerHalf{},
			BServerErrorBlockNonExistent{}
	} else if err != nil {
		return nil, BlockCryptKeyServerHalf{}, err
	}

	keyServerHalfPath := j.keyServerHalfPath(id)
	buf, err := ioutil.ReadFile(keyServerHalfPath)
	if os.IsNotExist(err) {
		return nil, BlockCryptKeyServerHalf{},
			BServerErrorBlockNonExistent{}
	} else if err != nil {
		return nil, BlockCryptKeyServerHalf{}, err
	}

	// Check integrity.

	dataID, err := j.crypto.MakePermanentBlockID(data)
	if err != nil {
		return nil, BlockCryptKeyServerHalf{}, err
	}

	if id != dataID {
		return nil, BlockCryptKeyServerHalf{}, fmt.Errorf(
			"Block ID mismatch: expected %s, got %s", id, dataID)
	}

	var serverHalf BlockCryptKeyServerHalf
	err = serverHalf.UnmarshalBinary(buf)
	if err != nil {
		return nil, BlockCryptKeyServerHalf{}, err
	}

	return data, serverHalf, nil
}

// All functions below are public functions.

var errBlockJournalShutdown = errors.New("blockJournal is shutdown")

func (j *blockJournal) getDataWithContext(
	id BlockID, context BlockContext) (
	[]byte, BlockCryptKeyServerHalf, error) {
	if j.isShutdown {
		return nil, BlockCryptKeyServerHalf{},
			errBlockJournalShutdown
	}

	refEntry, err := j.getRefEntry(id, context.GetRefNonce())
	if err != nil {
		return nil, BlockCryptKeyServerHalf{}, err
	}

	err = refEntry.checkContext(context)
	if err != nil {
		return nil, BlockCryptKeyServerHalf{}, err
	}

	return j.getData(id)
}

func (j *blockJournal) getAll() (
	map[BlockID]map[BlockRefNonce]blockRefLocalStatus, error) {
	if j.isShutdown {
		return nil, errBlockJournalShutdown
	}

	res := make(map[BlockID]map[BlockRefNonce]blockRefLocalStatus)

	for id, refs := range j.refs {
		if len(refs) == 0 {
			continue
		}

		res[id] = make(map[BlockRefNonce]blockRefLocalStatus)
		for ref, refEntry := range refs {
			res[id][ref] = refEntry.Status
		}
	}

	return res, nil
}

func (j *blockJournal) putData(
	ctx context.Context, id BlockID, context BlockContext, buf []byte,
	serverHalf BlockCryptKeyServerHalf) (err error) {
	j.log.CDebugf(ctx, "Putting %d bytes of data for block %s with context %v",
		len(buf), id, context)
	defer func() {
		if err != nil {
			j.deferLog.CDebugf(ctx,
				"Put for block %s with context %v failed with %v",
				id, context, err)
		}
	}()

	err = validateBlockServerPut(j.crypto, id, context, buf)
	if err != nil {
		return err
	}

	if j.isShutdown {
		return errBlockJournalShutdown
	}

	// Check the data and retrieve the server half, if they exist.
	_, existingServerHalf, err := j.getDataWithContext(id, context)
	var exists bool
	switch err.(type) {
	case BServerErrorBlockNonExistent:
		exists = false
	case nil:
		exists = true
	default:
		return err
	}

	if exists {
		// If the entry already exists, everything should be
		// the same, except for possibly additional
		// references.

		// We checked that both buf and the existing data hash
		// to id, so no need to check that they're both equal.

		if existingServerHalf != serverHalf {
			return fmt.Errorf(
				"key server half mismatch: expected %s, got %s",
				existingServerHalf, serverHalf)
		}
	}

	err = os.MkdirAll(j.blockPath(id), 0700)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(j.blockDataPath(id), buf, 0600)
	if err != nil {
		return err
	}
	j.blockBytes += int64(len(buf))

	// TODO: Add integrity-checking for key server half?

	err = ioutil.WriteFile(
		j.keyServerHalfPath(id), serverHalf.data[:], 0600)
	if err != nil {
		return err
	}

	err = j.putRefEntry(id, blockRefEntry{
		Status:  liveBlockRef,
		Context: context,
	})
	if err != nil {
		return err
	}

	return j.appendJournalEntry(
		blockPutOp, map[BlockID][]BlockContext{id: {context}})
}

func (j *blockJournal) addReference(
	ctx context.Context, id BlockID, context BlockContext) (
	err error) {
	j.log.CDebugf(ctx, "Adding reference for block %s with context %v",
		id, context)
	defer func() {
		if err != nil {
			j.deferLog.CDebugf(ctx,
				"Adding reference for block %s with context %v failed with %v",
				id, context, err)
		}
	}()

	if j.isShutdown {
		return errBlockJournalShutdown
	}

	refs := j.refs[id]
	if refs == nil {
		return BServerErrorBlockNonExistent{fmt.Sprintf("Block ID %s "+
			"doesn't exist and cannot be referenced.", id)}
	}

	// Only add it if there's a non-archived reference.
	hasNonArchivedRef := false
	for _, refEntry := range refs {
		if refEntry.Status == liveBlockRef {
			hasNonArchivedRef = true
			break
		}
	}

	if !hasNonArchivedRef {
		return BServerErrorBlockArchived{fmt.Sprintf("Block ID %s has "+
			"been archived and cannot be referenced.", id)}
	}

	// TODO: Figure out if we should allow adding a reference even
	// if all the existing references are archived, or if we have
	// no references at all. Also figure out what to do with an
	// addReference without a preceding Put.

	err = j.putRefEntry(id, blockRefEntry{
		Status:  liveBlockRef,
		Context: context,
	})
	if err != nil {
		return err
	}

	return j.appendJournalEntry(
		addRefOp, map[BlockID][]BlockContext{id: {context}})
}

func (j *blockJournal) removeReferences(
	ctx context.Context, contexts map[BlockID][]BlockContext,
	removeUnreferencedBlocks bool) (liveCounts map[BlockID]int, err error) {
	j.log.CDebugf(ctx, "Removing references for %v (remove unreferenced blocks=%t)",
		contexts, removeUnreferencedBlocks)
	defer func() {
		if err != nil {
			j.deferLog.CDebugf(ctx,
				"Removing references for %v (remove unreferenced blocks=%t)",
				contexts, removeUnreferencedBlocks, err)
		}
	}()

	if j.isShutdown {
		return nil, errBlockJournalShutdown
	}

	liveCounts = make(map[BlockID]int)

	for id, idContexts := range contexts {
		refs := j.refs[id]
		if refs == nil {
			// This block is already gone; no error.
			continue
		}

		for _, context := range idContexts {
			err := refs.remove(context)
			if err != nil {
				return nil, err
			}
		}

		count := len(refs)
		if count == 0 {
			delete(j.refs, id)
			if removeUnreferencedBlocks {
				// TODO: Decrement j.blockBytes, but only if the block
				// was part of a blockPutOp, not an addRefOp.
				err := os.RemoveAll(j.blockPath(id))
				if err != nil {
					return nil, err
				}
			}
		}
		liveCounts[id] = count
	}

	err = j.appendJournalEntry(removeRefsOp, contexts)
	if err != nil {
		return nil, err
	}

	return liveCounts, nil
}

func (j *blockJournal) archiveReferences(
	ctx context.Context, contexts map[BlockID][]BlockContext) (err error) {
	j.log.CDebugf(ctx, "Archiving references for %v", contexts)
	defer func() {
		if err != nil {
			j.deferLog.CDebugf(ctx,
				"Archiving references for %v,", contexts, err)
		}
	}()

	if j.isShutdown {
		return errBlockJournalShutdown
	}

	for id, idContexts := range contexts {
		for _, context := range idContexts {
			refNonce := context.GetRefNonce()
			refEntry, err := j.getRefEntry(id, refNonce)
			switch err.(type) {
			case BServerErrorBlockNonExistent:
				return BServerErrorBlockNonExistent{
					fmt.Sprintf(
						"Block ID %s (ref %s) doesn't "+
							"exist and cannot be archived.",
						id, refNonce),
				}
			case nil:
				break

			default:
				return err
			}

			err = refEntry.checkContext(context)
			if err != nil {
				return err
			}

			refEntry.Status = archivedBlockRef
			err = j.putRefEntry(id, refEntry)
			if err != nil {
				return err
			}
		}
	}

	return j.appendJournalEntry(archiveRefsOp, contexts)
}

// getNextEntryToFlush returns the info for the next journal entry to
// flush, if any. If there is no next journal entry to flush, the
// returned *blockJournalEntry will be nil.
func (j *blockJournal) getNextEntryToFlush(ctx context.Context) (
	journalOrdinal, *blockJournalEntry, []byte,
	BlockCryptKeyServerHalf, error) {
	if j.isShutdown {
		return 0, nil, nil, BlockCryptKeyServerHalf{},
			errBlockJournalShutdown
	}

	earliestOrdinal, err := j.j.readEarliestOrdinal()
	if os.IsNotExist(err) {
		return 0, nil, nil, BlockCryptKeyServerHalf{}, nil
	} else if err != nil {
		return 0, nil, nil, BlockCryptKeyServerHalf{}, err
	}

	e, err := j.readJournalEntry(earliestOrdinal)
	if err != nil {
		return 0, nil, nil, BlockCryptKeyServerHalf{}, err
	}

	var data []byte
	var serverHalf BlockCryptKeyServerHalf
	if e.Op == blockPutOp {
		id, _, err := e.getSingleContext()
		if err != nil {
			return 0, nil, nil, BlockCryptKeyServerHalf{}, err
		}

		data, serverHalf, err = j.getData(id)
		if err != nil {
			return 0, nil, nil, BlockCryptKeyServerHalf{}, err
		}
	}

	return earliestOrdinal, &e, data, serverHalf, nil
}

func flushBlockJournalEntry(
	ctx context.Context, log logger.Logger,
	bserver BlockServer, tlfID TlfID, entry blockJournalEntry, data []byte,
	serverHalf BlockCryptKeyServerHalf) error {
	log.CDebugf(ctx, "Flushing block op %v", entry)

	switch entry.Op {
	case blockPutOp:
		id, context, err := entry.getSingleContext()
		if err != nil {
			return err
		}

		err = bserver.Put(ctx, tlfID, id, context, data, serverHalf)
		if err != nil {
			return err
		}

	case addRefOp:
		id, context, err := entry.getSingleContext()
		if err != nil {
			return err
		}

		// TODO: If the reference add fails, retry with a
		// Put. This is tricky: see KBFS-1148 and KBFS-1255.
		err = bserver.AddBlockReference(ctx, tlfID, id, context)
		if err != nil {
			if isRecoverableBlockError(err) {
				log.CWarningf(ctx,
					"Recoverable block error encountered on AddBlockReference: %v", err)
			}
			return err
		}

	case removeRefsOp:
		_, err := bserver.RemoveBlockReferences(
			ctx, tlfID, entry.Contexts)
		if err != nil {
			return err
		}

	case archiveRefsOp:
		err := bserver.ArchiveBlockReferences(
			ctx, tlfID, entry.Contexts)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("Unknown op %s", entry.Op)
	}

	return nil
}

func (j *blockJournal) removeFlushedEntry(ctx context.Context,
	ordinal journalOrdinal, entry blockJournalEntry) error {
	if j.isShutdown {
		// TODO: This creates a race condition if we shut down
		// after we've flushed an op but before we remove
		// it. Make sure we handle re-flushed ops
		// idempotently.
		return errBlockJournalShutdown
	}

	// Fix up the block byte count if we've finished a Put.
	if entry.Op == blockPutOp {
		id, _, err := entry.getSingleContext()
		if err != nil {
			return err
		}
		b, err := j.getDataSize(id)
		if err != nil {
			return err
		}

		if b > j.blockBytes {
			return fmt.Errorf("Block %v is bigger than our current count "+
				"of journal block bytes (%d > %d)", id, b, j.blockBytes)
		}
		j.blockBytes -= b
	}

	earliestOrdinal, err := j.j.readEarliestOrdinal()
	if err != nil {
		return err
	}

	if ordinal != earliestOrdinal {
		return fmt.Errorf("Expected ordinal %d, got %d",
			ordinal, earliestOrdinal)
	}

	_, err = j.j.removeEarliest()
	if err != nil {
		return err
	}

	// TODO: This is a hack to work around KBFS-1439. Figure out a
	// better way to update j.refs to reflect the removed entry.
	refs, err := j.readJournal(ctx)
	if err != nil {
		return err
	}
	j.refs = refs

	return nil
}

func (j *blockJournal) shutdown() {
	j.isShutdown = true

	// Double-check the on-disk journal with the in-memory one.
	ctx := context.Background()
	refs, err := j.readJournal(ctx)
	if err != nil {
		panic(err)
	}

	if !reflect.DeepEqual(refs, j.refs) {
		panic(fmt.Sprintf("refs = %v != j.refs = %v", refs, j.refs))
	}
}
