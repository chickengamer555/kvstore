package kvstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// replayBytes walks the records in buf, in order, handing each one to apply.
//
// It stops at the first record it cannot vouch for and reports why. The
// important property is that it never skips: a record that fails its checksum
// or breaks the sequence chain ends the replay, and nothing after it is read.
// Skipping the bad record and carrying on would let a crash that scrambled the
// middle of the log resurrect writes made after the ones it lost, which is a
// far worse outcome than losing the tail.
//
// startCRC is the checksum of the record before the first one in buf, and
// wantSeq is the sequence number that record must carry. Together they are the
// link that makes a lifted or reordered record fail.
//
// Returns the checksum and sequence number of the last accepted record, how
// many bytes were consumed, and the reason it stopped.
func replayBytes(buf []byte, startCRC uint32, wantSeq uint64, apply func(record)) (uint32, uint64, int, stopReason) {
	crc := startCRC
	seq := wantSeq - 1
	off := 0

	for off < len(buf) {
		r, n, next, err := decodeRecord(buf[off:], crc, seq+1)
		if err != nil {
			return crc, seq, off, reasonFor(err)
		}
		apply(r)
		crc = next
		seq = r.seq
		off += n
	}
	return crc, seq, off, stopEndOfLog
}

// dirState is everything Open needs to know after reading what is on disk.
type dirState struct {
	data     map[string][]byte
	report   RecoveryReport
	segments int

	lastSeq uint64
	lastCRC uint32

	// active is the segment to append to, and activeBytes is how much of it
	// recovery was able to vouch for. Anything past that point is a torn tail
	// and gets truncated away when the segment is reopened.
	active      uint64
	activeBytes int64

	// drop lists segments after the point recovery stopped. They are
	// unreachable - the chain that would verify them is broken - so nothing in
	// them can ever be replayed, and leaving them would only confuse the next
	// recovery.
	drop []uint64
}

func listSegments(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("kvstore: reading store directory: %w", err)
	}
	var out []uint64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if base, ok := segmentBase(e.Name()); ok {
			out = append(out, base)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// loadDir replays the whole directory: the checkpoint if there is a usable
// one, then every log segment, in order.
//
// Determinism (clause B4) comes from this function having no inputs but the
// bytes on disk. There is no map iteration, no time, no randomness and no
// concurrency anywhere on this path, so two runs over the same directory apply
// the same records in the same order and end in the same state.
func loadDir(dir string) (*dirState, error) {
	segs, err := listSegments(dir)
	if err != nil {
		return nil, err
	}

	st := &dirState{data: map[string][]byte{}, segments: len(segs)}
	st.report.Segments = len(segs)

	ckpt, rejected := loadCheckpoint(dir)
	st.report.CheckpointRejected = rejected
	if ckpt != nil {
		st.data = ckpt.data
		st.report.CheckpointSeq = ckpt.seq
		st.report.UsedCheckpoint = true
	}

	// The log has to account for everything since the checkpoint. If the oldest
	// surviving segment begins after the checkpoint's sequence number, records
	// in between exist nowhere: a segment was deleted before the checkpoint
	// that superseded it was durable, which this store's ordering never does.
	// Opening anyway would hand back a store that is quietly missing writes.
	if len(segs) > 0 && segs[0] > st.report.CheckpointSeq {
		return nil, fmt.Errorf("kvstore: log starts at sequence %d but the checkpoint only covers %d - records %d..%d are in neither",
			segs[0]+1, st.report.CheckpointSeq, st.report.CheckpointSeq+1, segs[0])
	}

	crc := uint32(0)
	seq := st.report.CheckpointSeq
	stopAt := -1

	for i, base := range segs {
		name := segmentName(base)
		buf, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("kvstore: reading log segment %s: %w", name, err)
		}

		if i > 0 && base != seq {
			// Segments must abut. A gap means a segment went missing, and
			// closing over it would silently drop every record it held.
			st.report.Stopped = stopSequence
			st.report.StoppedAt = fmt.Sprintf("%s+0", name)
			stopAt = i - 1
			break
		}

		// Each segment starts its own checksum chain, because a checkpoint may
		// have deleted the segment its first record would otherwise chain to.
		// The sequence number is global and carries ordering across the
		// boundary; the checksum chain ties records together within a segment.
		endCRC, endSeq, consumed, reason := replayBytes(buf, 0, base+1, func(r record) {
			// Records the checkpoint already contains are verified - they are
			// part of the chain - but not re-applied. Applying only a
			// surviving suffix of them would overwrite newer values with older
			// ones, which is the exact bug that makes a crash during
			// checkpointing corrupt a store.
			if r.seq <= st.report.CheckpointSeq {
				st.report.Skipped++
				return
			}
			st.report.Applied++
			if r.kind == kindDelete {
				delete(st.data, r.key)
				return
			}
			st.data[r.key] = r.value
		})

		crc, seq = endCRC, endSeq
		st.active, st.activeBytes = base, int64(consumed)

		if reason != stopEndOfLog {
			st.report.Stopped = reason
			st.report.StoppedAt = fmt.Sprintf("%s+%d", name, consumed)
			stopAt = i
			break
		}
	}

	if stopAt >= 0 {
		st.drop = append(st.drop, segs[stopAt+1:]...)
		st.report.Dropped = len(st.drop)
	} else {
		st.report.Stopped = stopEndOfLog
	}
	st.lastSeq = seq
	st.lastCRC = crc
	st.report.LastSeq = seq
	return st, nil
}
