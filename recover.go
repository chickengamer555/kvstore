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
	active      uint32
	activeBytes int64

	// drop lists segments after the point recovery stopped. They are
	// unreachable - the chain that would verify them is broken - so nothing in
	// them can ever be replayed, and leaving them would only confuse the next
	// recovery.
	drop []uint32
}

func listSegments(dir string) ([]uint32, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("kvstore: reading store directory: %w", err)
	}
	var out []uint32
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if n, ok := segmentIndex(e.Name()); ok {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// loadDir replays the whole directory: every log segment, in order.
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

	crc := uint32(0)
	seq := st.lastSeq
	stopAt := -1

	for i, n := range segs {
		name := segmentName(n)
		buf, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("kvstore: reading log segment %s: %w", name, err)
		}

		// Each segment starts its own checksum chain, because a checkpoint may
		// have deleted the segment its first record would otherwise chain to.
		// The sequence number is global and carries the ordering across the
		// boundary; the checksum chain is what ties records together within a
		// segment.
		endCRC, endSeq, consumed, reason := replayBytes(buf, 0, seq+1, func(r record) {
			st.report.Applied++
			if r.kind == kindDelete {
				delete(st.data, r.key)
				return
			}
			st.data[r.key] = r.value
		})

		crc, seq = endCRC, endSeq
		st.active, st.activeBytes = n, int64(consumed)

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
