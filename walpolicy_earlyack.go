//go:build kvearlyack

package kvstore

import "fmt"

// A DELIBERATELY BROKEN BUILD. Never ship this. `go build` and `go test`
// without -tags kvearlyack cannot see this file at all.
//
// It exists so the crash harness has something it is known to catch. A test
// suite that has only ever been run against correct code has not been shown to
// detect anything, and "the crash corpus passes" means nothing until the same
// corpus has been watched failing on a store that really does lose data.
//
// The bug it introduces is the specific one a process kill can actually
// detect, and choosing it took some care. Removing the fsync would NOT work:
// after Process.Kill the page cache is still intact and the kernel writes the
// data out anyway, so an unsynced store passes a process-kill harness happily.
// Only a power cut catches a missing fsync, and nothing in user space can
// stage a power cut.
//
// What a process kill does catch is data that never left the process. So this
// build does what somebody reaches for the first time they benchmark a WAL and
// find it slow: it buffers records in user space and acknowledges the write
// immediately, flushing every 4KB. Every one of those acknowledged-but-
// buffered records dies with the process.
//
// That limit is worth being clear about in both directions. The corpus proves
// this store survives death at an arbitrary instruction. It does not prove the
// fsync is doing its job; the ordering test and code review do that.
const ackAfterSync = false

const earlyAckFlushBytes = 4 << 10

func (w *wal) commit(enc []byte) (int, error) {
	w.pending = append(w.pending, enc...)
	if len(w.pending) < earlyAckFlushBytes {
		// Acknowledged. Nothing has reached the kernel. This is the bug.
		return len(enc), nil
	}
	if err := w.flushPending(); err != nil {
		return len(enc), err
	}
	return len(enc), nil
}

func (w *wal) flushPending() error {
	if len(w.pending) == 0 {
		return nil
	}
	n, err := w.f.WriteAt(w.pending, w.wrote)
	w.wrote += int64(n)
	if err != nil {
		return fmt.Errorf("kvstore: writing buffered log records: %w", err)
	}
	w.pending = w.pending[:0]
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("kvstore: syncing log: %w", err)
	}
	w.syncs++
	return nil
}

func (w *wal) finish() error { return w.flushPending() }
