package kvstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
)

// A checkpoint is the live key set, serialised, plus the sequence number it was
// taken at. Once it is durable, every log record up to that sequence number is
// redundant and the segments holding them can be deleted - which is the only
// thing that bounds the log.
//
//	offset  size  field
//	     0     8  magic
//	     8     4  crc32c over everything from offset 12
//	    12     8  sequence number this checkpoint covers
//	    20     n  serialised state (see state.go)
const (
	checkpointName       = "CHECKPOINT"
	checkpointTempName   = "CHECKPOINT.tmp"
	checkpointHeaderSize = 20
)

var checkpointMagic = []byte("KVCKPT\x00\x01")

func checkpointChecksum(buf []byte) uint32 {
	return crc32.Checksum(buf[12:], castagnoli)
}

type checkpoint struct {
	seq  uint64
	data map[string][]byte
}

func encodeCheckpoint(seq uint64, payload []byte) []byte {
	buf := make([]byte, checkpointHeaderSize, checkpointHeaderSize+len(payload))
	copy(buf, checkpointMagic)
	binary.LittleEndian.PutUint64(buf[12:], seq)
	buf = append(buf, payload...)
	binary.LittleEndian.PutUint32(buf[8:], checkpointChecksum(buf))
	return buf
}

// loadCheckpoint reads the checkpoint file if there is a usable one.
//
// "Usable" is doing a lot of work here. A crash can leave this file half
// written, so it is exactly as trustworthy as a log record and gets the same
// treatment: a bad magic, a bad checksum or a payload that does not decode
// means there is no checkpoint, not that the store is broken. The log is still
// complete, because segments are only ever deleted after the checkpoint that
// replaces them is durable.
//
// The second return value distinguishes "there was no checkpoint" from "there
// was one and it was rejected". Those produce the same recovered state and are
// very different things to see in a report - the second one means a crash
// landed inside a checkpoint, which is worth knowing.
func loadCheckpoint(fsys fileSystem) (*checkpoint, bool) {
	raw, err := readAll(fsys, checkpointName)
	if err != nil {
		// Anything unreadable is treated as absent. That includes a permission
		// error, which is a stretch - but the log is authoritative either way,
		// and refusing to open a recoverable store is the worse failure.
		return nil, false
	}
	if len(raw) < checkpointHeaderSize || !bytes.Equal(raw[:8], checkpointMagic) {
		return nil, true
	}
	if binary.LittleEndian.Uint32(raw[8:]) != checkpointChecksum(raw) {
		return nil, true
	}
	data, err := decodeState(raw[checkpointHeaderSize:])
	if err != nil {
		// The checksum passed and the payload did not decode, so this is not a
		// torn file: it is a checkpoint written by a version of this code that
		// serialised state differently. Same treatment - fall back to the log.
		return nil, true
	}
	return &checkpoint{seq: binary.LittleEndian.Uint64(raw[12:]), data: data}, false
}

// writeCheckpoint makes a checkpoint durable, then makes its name durable.
//
// Write to a temporary name, fsync the contents, rename over the real name,
// fsync the directory. Rename is atomic, so a reader either sees the whole old
// checkpoint or the whole new one and never a mixture - which is what lets
// loadCheckpoint treat a bad checksum as "a crash happened during a
// checkpoint" rather than "the store is corrupt". Atomic is not the same as
// durable, though: until the directory is fsynced a reopening process still
// finds the old name, which is what the last step is for and what
// TestTheCheckpointIsDurableAsSoonAsItIsInstalled fails without.
//
// There used to be a fourth step, a directory sync between the fsync and the
// rename, to make the temporary name itself durable first. It is gone, and
// because removing an fsync and removing an inconvenience look identical in a
// diff, here is the whole argument rather than the conclusion.
//
// The window it covered is a crash after createTrunc and before the sync that
// follows the rename. What the platter can hold at that moment, with only the
// one directory sync, is: the old CHECKPOINT (or none), no CHECKPOINT.tmp
// entry, and every log segment. What it holds with the extra sync is the same
// thing plus a stray CHECKPOINT.tmp. That is the entire difference, and it was
// measured, not assumed - crashing at rename, at the sync after it, and at the
// sync after that, under both versions, produces those directory listings and
// no others.
//
// Neither state loses anything, and the reason is that the checkpoint has not
// been INSTALLED in this window: writeCheckpoint has not returned, so
// checkpointLocked has not rotated the log and has not unlinked a single
// segment. Recovery therefore finds the old checkpoint and the complete log
// and replays it. A stray CHECKPOINT.tmp is not read by loadCheckpoint, is not
// a segment name, and is truncated away by the next createTrunc. The one state
// that WOULD be dangerous - the new name durable over contents that are not -
// is prevented by f.Sync() above, before the rename, not by any directory
// sync.
//
// The rename cannot become durable without the create, because both are writes
// to the same directory and a filesystem that journals them cannot commit the
// second without the first: there would be no entry for rename(2) to move.
// That is what makes one post-rename fsync sufficient, and it is the canonical
// write-tmp, fsync-tmp, rename, fsync-dir recipe rather than anything invented
// here.
//
// No test in this repository distinguishes one directory sync from two - both
// pass, including the crash-at-every-call test below - because the difference
// is a stray file no reader consults.
// TestAPowerCutAnywhereInTheCheckpointPathLosesNothing checks the premise the
// argument stands on instead: that a power cut at any call this path makes
// leaves every acknowledged write readable. Invert the ordering in
// checkpointLocked so segments are removed before the checkpoint is durable
// and it fails at five of its ten crash points, naming the keys that went.
func writeCheckpoint(fsys fileSystem, seq uint64, payload []byte) error {
	f, err := fsys.createTrunc(checkpointTempName)
	if err != nil {
		return fmt.Errorf("kvstore: creating checkpoint: %w", err)
	}
	if _, err := f.WriteAt(encodeCheckpoint(seq, payload), 0); err != nil {
		return joinClose(fmt.Errorf("kvstore: writing checkpoint: %w", err), f)
	}
	if err := f.Sync(); err != nil {
		return joinClose(fmt.Errorf("kvstore: syncing checkpoint: %w", err), f)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("kvstore: closing checkpoint: %w", err)
	}
	if err := fsys.rename(checkpointTempName, checkpointName); err != nil {
		return fmt.Errorf("kvstore: installing checkpoint: %w", err)
	}
	if err := fsys.syncDir(); err != nil {
		return fmt.Errorf("kvstore: syncing directory after checkpoint rename: %w", err)
	}
	return nil
}

// Checkpoint folds the live key set into a checkpoint file and starts a fresh
// log segment, so that everything logged up to now can be deleted.
//
// Callers rarely need this: it runs on its own once the live segment passes
// Options.CheckpointBytes. It is exported because a process that knows it is
// about to be idle can shorten its own next recovery by calling it.
func (s *Store) Checkpoint() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return s.checkpointLocked()
}

// checkpointLocked is the one path in this store that deletes data on purpose,
// so the order of the four steps is the whole of its crash safety.
//
//  1. Write the checkpoint and make it durable. A crash before this finishes
//     leaves the old checkpoint (or none) and every segment still present, so
//     recovery replays the log in full and loses nothing.
//  2. Start a new segment based at this sequence number. A crash here leaves a
//     durable checkpoint plus every old segment; recovery loads the checkpoint
//     and then verifies but does not re-apply the records it already contains.
//  3. Delete the segments the checkpoint has made redundant. A crash part-way
//     through leaves some of them, which is the same case as step 2.
//  4. Sync the directory so the deletions are durable.
//
// The invariant behind all four: a log segment is only ever removed after the
// checkpoint that supersedes it is on disk. There is no window in which
// neither holds the data.
//
// And a fifth step that comes before all of them, because rotating is the one
// thing this store must not do while the live segment is poisoned. See the
// guard below.
func (s *Store) checkpointLocked() error {
	if s.log.failed != nil {
		// A segment whose commit failed has a tail recovery cannot vouch for,
		// and until it is reopened nothing has cut that tail away. Rotating
		// now moves s.log to a fresh segment and then trips over the failure
		// on close, returning before a single unlink - so the poisoned segment
		// stays on disk UNDERNEATH a successor the store is about to
		// acknowledge writes into. The next Open stops at the tail, finds
		// every later segment unreachable, and deletes them. Those writes are
		// acknowledged, and no crash was needed to lose them.
		//
		// Refusing is enough because the refusal does not outlive the segment:
		// reopening truncates the tail away and the caller gets its checkpoint
		// then. Refusing is also all that is available here - truncating the
		// live segment instead would mean writing to a file the store has
		// already been told it cannot write to.
		return fmt.Errorf("kvstore: refusing to checkpoint over a log segment whose last commit did not complete - reopen the store first: %w", s.log.failed)
	}

	seq := s.log.seq
	if seq == s.log.base {
		// Nothing has been written since the last checkpoint. Rotating here
		// would try to create a segment that already exists.
		return nil
	}

	if err := writeCheckpoint(s.fsys, seq, encodeState(s.data)); err != nil {
		return err
	}

	next, err := createSegment(s.fsys, seq, s.opts.trace)
	if err != nil {
		return err
	}
	old := s.log
	s.log = next
	if err := old.close(); err != nil {
		return fmt.Errorf("kvstore: closing the checkpointed segment: %w", err)
	}

	bases, err := listSegments(s.fsys)
	if err != nil {
		return err
	}
	removed := 0
	for _, base := range bases {
		if base >= seq {
			continue
		}
		if err := s.fsys.remove(segmentName(base)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("kvstore: removing checkpointed segment: %w", err)
		}
		removed++
	}
	if err := s.fsys.syncDir(); err != nil {
		return fmt.Errorf("kvstore: syncing after removing checkpointed segments: %w", err)
	}

	s.stats.Checkpoints++
	s.stats.LogBytes = s.log.bytes
	s.emit("checkpoint", fmt.Sprintf("seq=%d removed=%d", seq, removed))
	return nil
}
