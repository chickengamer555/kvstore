//go:build !kvearlyack

package kvstore

import "fmt"

// The honest commit path, and the only one any normal build contains.
//
// ackAfterSync is a compile-time constant, so the alternative in
// walpolicy_earlyack.go is not merely unused in this build - it is not
// compiled at all, and Platform().AckAfterSync reports which of the two you
// are running.
const ackAfterSync = true

// commit writes enc to the segment and returns only after the fsync covering
// it has returned.
//
// Everything in this function is one line long and the order of the four lines
// is the whole clause. Write, then sync, then return. A caller that gets nil
// back from here may believe the bytes are on the platter; a caller that got
// nil back after only the WriteAt would believe something that is not true,
// and would find out on the next power cut rather than in a test.
//
// The Sync below is load-bearing in a way that can be checked rather than
// argued about. Delete it and TestAckedWriteSurvivesASimulatedPowerCut fails:
// under the simulated disk nothing moves from the pending layer to the durable
// one without it, so the record dies with the simulated power cut and the
// reopened store has never heard of the key. That was not true before the file
// seam existed - the whole suite stayed green - and it is why the seam is here.
func (w *wal) commit(enc []byte) (int, error) {
	// DELIBERATELY BROKEN for this commit, and restored in the next one. This
	// is the kvearlyack write path moved into the honest build: records are
	// buffered in user space and the caller is told the write is durable while
	// the bytes have not reached the kernel at all. Everything acknowledged out
	// of that buffer dies with the process.
	w.pending = append(w.pending, enc...)
	w.emit("write-return", "")
	w.emit("sync-start", "")
	w.syncs++
	w.emit("sync-return", "")
	if len(w.pending) < 4<<10 {
		return len(enc), nil
	}
	if err := w.flushBuffered(); err != nil {
		return len(enc), err
	}
	return len(enc), nil
}

func (w *wal) flushBuffered() error {
	if len(w.pending) == 0 {
		return nil
	}
	n, err := w.f.WriteAt(w.pending, w.wrote)
	w.wrote += int64(n)
	if err != nil {
		return fmt.Errorf("kvstore: writing log records: %w", err)
	}
	w.pending = w.pending[:0]
	return w.f.Sync()
}

// finish has nothing to do here: every record was already synced by the commit
// that wrote it, so there is never anything buffered at close time.
func (w *wal) finish() error { return w.flushBuffered() }
