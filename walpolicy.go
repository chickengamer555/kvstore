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
// nil back after only the Write would believe something that is not true, and
// would find out on the next power cut rather than in a test.
func (w *wal) commit(enc []byte) (int, error) {
	n, err := w.f.Write(enc)
	if err != nil {
		return n, fmt.Errorf("kvstore: writing log records: %w", err)
	}
	w.emit("write-return", "")

	w.emit("sync-start", "")
	if err := w.f.Sync(); err != nil {
		return n, fmt.Errorf("kvstore: syncing log: %w", err)
	}
	w.syncs++
	w.emit("sync-return", "")
	return n, nil
}

// finish has nothing to do here: every record was already synced by the commit
// that wrote it, so there is never anything buffered at close time.
func (w *wal) finish() error { return nil }
