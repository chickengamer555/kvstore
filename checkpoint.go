package kvstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
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

func checkpointPath(dir string) string     { return filepath.Join(dir, checkpointName) }
func checkpointTempPath(dir string) string { return filepath.Join(dir, checkpointTempName) }

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
func loadCheckpoint(dir string) (*checkpoint, bool) {
	raw, err := os.ReadFile(checkpointPath(dir))
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
// Write to a temporary name, fsync the contents, sync the directory so the
// temporary name itself exists, rename over the real name, sync the directory
// again so the rename is durable. Rename is atomic, so a reader either sees
// the whole old checkpoint or the whole new one and never a mixture - which is
// what lets loadCheckpoint treat a bad checksum as "a crash happened during a
// checkpoint" rather than "the store is corrupt".
func writeCheckpoint(dir string, seq uint64, payload []byte) error {
	tmp := checkpointTempPath(dir)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("kvstore: creating checkpoint: %w", err)
	}
	if _, err := f.Write(encodeCheckpoint(seq, payload)); err != nil {
		return joinClose(fmt.Errorf("kvstore: writing checkpoint: %w", err), f)
	}
	if err := f.Sync(); err != nil {
		return joinClose(fmt.Errorf("kvstore: syncing checkpoint: %w", err), f)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("kvstore: closing checkpoint: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("kvstore: syncing directory before checkpoint rename: %w", err)
	}
	if err := os.Rename(tmp, checkpointPath(dir)); err != nil {
		return fmt.Errorf("kvstore: installing checkpoint: %w", err)
	}
	if err := syncDir(dir); err != nil {
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
func (s *Store) checkpointLocked() error {
	seq := s.log.seq
	if seq == s.log.base {
		// Nothing has been written since the last checkpoint. Rotating here
		// would try to create a segment that already exists.
		return nil
	}

	if err := writeCheckpoint(s.dir, seq, encodeState(s.data)); err != nil {
		return err
	}

	next, err := createSegment(s.dir, seq, s.opts.trace)
	if err != nil {
		return err
	}
	old := s.log
	s.log = next
	if err := old.close(); err != nil {
		return fmt.Errorf("kvstore: closing the checkpointed segment: %w", err)
	}

	bases, err := listSegments(s.dir)
	if err != nil {
		return err
	}
	removed := 0
	for _, base := range bases {
		if base >= seq {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, segmentName(base))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("kvstore: removing checkpointed segment: %w", err)
		}
		removed++
	}
	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("kvstore: syncing after removing checkpointed segments: %w", err)
	}

	s.stats.Checkpoints++
	s.stats.LogBytes = s.log.bytes
	s.emit("checkpoint", fmt.Sprintf("seq=%d removed=%d", seq, removed))
	return nil
}
