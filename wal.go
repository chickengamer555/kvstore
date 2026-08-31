package kvstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The log is a sequence of numbered segments, LOG.000001 upwards. Only the
// highest-numbered one is ever written to; the rest are immutable until a
// checkpoint makes them unnecessary and deletes them.
const segmentPrefix = "LOG."

func segmentName(n uint32) string { return fmt.Sprintf("%s%06d", segmentPrefix, n) }

func segmentIndex(name string) (uint32, bool) {
	if !strings.HasPrefix(name, segmentPrefix) {
		return 0, false
	}
	n, err := strconv.ParseUint(name[len(segmentPrefix):], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

// wal is the append-only log segment currently being written.
//
// It carries the sequence number and checksum of the last record it wrote,
// because those two values are what the next record chains to. Keeping them on
// the writer rather than re-reading the tail of the file means a write never
// has to read.
type wal struct {
	dir     string
	index   uint32
	path    string
	f       *os.File
	seq     uint64
	crc     uint32
	bytes   int64
	syncs   int64
	trace   func(name, detail string)
	scratch []byte

	// pending exists only for the deliberately broken kvearlyack build, which
	// buffers records in user space and acknowledges before they have reached
	// the kernel at all. It is unused in every normal build.
	pending []byte
}

func (w *wal) emit(name, detail string) {
	if w.trace != nil {
		w.trace(name, detail)
	}
}

// createSegment makes a new, empty segment and makes its existence durable.
//
// The order is: create the file, sync the file, sync the containing directory.
// The last step is the one people leave out. Without it the file's contents are
// durable and the directory entry naming the file is not, so a crash can leave
// a store whose log exists on disk and has no name - see syncdir_unix.go.
func createSegment(dir string, n uint32, seq uint64, crc uint32, trace func(string, string)) (*wal, error) {
	path := filepath.Join(dir, segmentName(n))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("kvstore: creating log segment: %w", err)
	}
	w := &wal{dir: dir, index: n, path: path, f: f, seq: seq, crc: crc, trace: trace}
	w.emit("segment-create", path)

	if err := f.Sync(); err != nil {
		return nil, joinClose(fmt.Errorf("kvstore: syncing new log segment: %w", err), f)
	}
	if err := syncDir(dir); err != nil {
		return nil, joinClose(fmt.Errorf("kvstore: syncing log directory: %w", err), f)
	}
	if dirSyncSupported {
		w.emit("dir-sync", "ok")
	} else {
		w.emit("dir-sync", "unsupported")
	}
	return w, nil
}

// reopenSegment reopens an existing segment for append, truncating it to the
// last byte recovery was able to vouch for.
//
// The truncation is not optional. A crash leaves a torn record at the end of
// the file; appending after it would put every future record beyond a point
// where recovery stops, so they would be written, fsynced, acknowledged and
// then never read again. Cutting the file back to validBytes is what makes the
// log usable after the crash it was designed to survive.
func reopenSegment(dir string, n uint32, seq uint64, crc uint32, validBytes int64, trace func(string, string)) (*wal, error) {
	path := filepath.Join(dir, segmentName(n))
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("kvstore: reopening log segment: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, joinClose(fmt.Errorf("kvstore: sizing log segment: %w", err), f)
	}
	if info.Size() != validBytes {
		if err := f.Truncate(validBytes); err != nil {
			return nil, joinClose(fmt.Errorf("kvstore: truncating torn tail: %w", err), f)
		}
		if err := f.Sync(); err != nil {
			return nil, joinClose(fmt.Errorf("kvstore: syncing after truncation: %w", err), f)
		}
		trace2(trace, "truncate", fmt.Sprintf("%s:%d->%d", path, info.Size(), validBytes))
	}
	if _, err := f.Seek(validBytes, 0); err != nil {
		return nil, joinClose(fmt.Errorf("kvstore: seeking to log tail: %w", err), f)
	}
	return &wal{dir: dir, index: n, path: path, f: f, seq: seq, crc: crc, bytes: validBytes, trace: trace}, nil
}

func trace2(trace func(string, string), name, detail string) {
	if trace != nil {
		trace(name, detail)
	}
}

// joinClose reports the original error, closing f on the way out. The close
// error is deliberately discarded and only here: the caller is already on a
// failure path, and replacing a real error with "close failed" loses the one
// piece of information that explains what went wrong.
func joinClose(err error, f *os.File) error {
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("%w (and closing the file failed: %v)", err, cerr)
	}
	return err
}

// appendRecords writes recs as one contiguous run and returns only after the
// fsync covering them has returned.
//
// One write and one fsync for the whole batch, however many records are in it.
// That is the whole of the difference between the per-write and group-commit
// numbers in bench/results.md: the syscall cost barely moves, and what changes
// is the point at which the caller is entitled to believe the data is safe -
// every record, or every batch.
//
// The write and the sync themselves live in commit(), which is build-tagged,
// so that the crash harness has a deliberately broken build to prove it can
// catch. See walpolicy.go.
func (w *wal) appendRecords(recs []record) error {
	w.scratch = w.scratch[:0]
	crc := w.crc
	seq := w.seq
	for i := range recs {
		seq++
		recs[i].seq = seq
		w.scratch, crc = appendRecord(w.scratch, recs[i], crc)
	}

	n, err := w.commit(w.scratch)
	w.bytes += int64(n)
	if err != nil {
		// A short or failed write leaves the segment in an unknown state, so
		// the in-memory chain must not advance past it. Recovery finds a torn
		// tail here, which is exactly what it is for.
		return err
	}

	w.crc = crc
	w.seq = seq
	return nil
}

func (w *wal) close() error {
	if w.f == nil {
		return nil
	}
	f := w.f
	w.f = nil
	if err := w.finish(); err != nil {
		return joinClose(err, f)
	}
	return f.Close()
}
