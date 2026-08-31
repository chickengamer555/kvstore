package kvstore

import (
	"fmt"
	"strconv"
	"strings"
)

// A segment is named for its BASE sequence number: the sequence of the record
// immediately before its first one. LOG.00000000000000000000 holds records 1
// upwards, and after a checkpoint at sequence 900 the next segment is
// LOG.00000000000000000900 and holds 901 upwards.
//
// Putting the base in the name rather than in a file header is deliberate.
// Recovery has to know where a segment starts in the global sequence before it
// reads a byte of it, because after a checkpoint the earlier segments are gone
// and there is nothing left to count from. Fixed-width zero padding means
// lexical order is numeric order, so a plain sort of the directory listing is
// the replay order.
const segmentPrefix = "LOG."

func segmentName(base uint64) string { return fmt.Sprintf("%s%020d", segmentPrefix, base) }

func segmentBase(name string) (uint64, bool) {
	if !strings.HasPrefix(name, segmentPrefix) {
		return 0, false
	}
	rest := name[len(segmentPrefix):]
	if len(rest) != 20 {
		return 0, false
	}
	n, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func isSegmentName(name string) bool {
	_, ok := segmentBase(name)
	return ok
}

// wal is the append-only log segment currently being written.
//
// It carries the sequence number and checksum of the last record it wrote,
// because those two values are what the next record chains to. Keeping them on
// the writer rather than re-reading the tail of the file means a write never
// has to read.
type wal struct {
	fsys    fileSystem
	base    uint64
	name    string
	path    string
	f       file
	seq     uint64
	crc     uint32
	bytes   int64
	syncs   int64
	trace   func(name, detail string)
	scratch []byte

	// wrote is the offset the next WriteAt goes to: how many bytes have been
	// handed to the file. In every normal build it is equal to bytes, because
	// commit writes each batch through as it arrives. In the deliberately
	// broken kvearlyack build bytes runs ahead of it, and the difference is
	// exactly the data that has been acknowledged and not written.
	wrote int64

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
func createSegment(fsys fileSystem, base uint64, trace func(string, string)) (*wal, error) {
	name := segmentName(base)
	f, err := fsys.create(name)
	if err != nil {
		return nil, fmt.Errorf("kvstore: creating log segment: %w", err)
	}
	w := &wal{fsys: fsys, base: base, name: name, path: fsys.path(name), f: f, seq: base, trace: trace}
	w.emit("segment-create", w.path)

	if err := f.Sync(); err != nil {
		return nil, joinClose(fmt.Errorf("kvstore: syncing new log segment: %w", err), f)
	}
	if err := fsys.syncDir(); err != nil {
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
func reopenSegment(fsys fileSystem, base uint64, seq uint64, crc uint32, validBytes int64, trace func(string, string)) (*wal, error) {
	name := segmentName(base)
	path := fsys.path(name)
	size, err := fsys.size(name)
	if err != nil {
		return nil, fmt.Errorf("kvstore: sizing log segment: %w", err)
	}
	f, err := fsys.open(name)
	if err != nil {
		return nil, fmt.Errorf("kvstore: reopening log segment: %w", err)
	}
	// DELIBERATELY DELETED for this commit: the truncation back to validBytes,
	// and with it the seek. The segment is reopened at its full length, so the
	// next record is appended after the torn tail. Restored in the next commit.
	_ = validBytes
	return &wal{fsys: fsys, base: base, name: name, path: path, f: f, seq: seq, crc: crc, bytes: size, wrote: size, trace: trace}, nil
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
func joinClose(err error, f file) error {
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
	// finish() before the handle is dropped: in a build that buffers, this is
	// where the buffer is written out, and it needs the file to still be here.
	err := w.finish()
	w.f = nil
	if err != nil {
		return joinClose(err, f)
	}
	return f.Close()
}
