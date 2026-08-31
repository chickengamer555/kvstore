package kvstore

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// The disk says no.
//
// Every test in this repository used to run against a filesystem that always
// worked. Nothing made a write short, nothing made an fsync fail, and nothing
// made a read return anything but bytes - so every error branch on the commit
// path had executed exactly zero times, and an error could have been swallowed
// anywhere along it without a single test noticing. That was measured rather
// than suspected: replacing `readErr = err` with `_ = err` in file.go left the
// whole suite green.
//
// These tests inject the failures a real disk produces. They are not about the
// error message; they are about clause B1, which does not stop applying because
// the hardware misbehaved. An acknowledged write survives a crash. A call that
// could not be completed must therefore not acknowledge.

var errSimulatedIO = errors.New("simulated EIO from the disk")

// B1, B2. An fsync that fails has still returned, and a store that treats
// "returned" as "durable" acknowledges a write that is not on the platter.
//
// The second half is the one that is easy to miss and is why this test writes
// a third key. When a commit fails part way through, the log has bytes in it
// that recovery cannot vouch for, and the writer's offset has moved past them.
// The NEXT record is therefore written beyond a point recovery stops at:
// fsynced, acknowledged, and unreachable for ever. So the assertion is not
// "Put returns an error" - it is the contract itself. Every key whose Put
// returned nil has to be readable after the power cut, however the disk
// behaved in between.
func TestAFailedSyncNeverProducesAnAcknowledgement(t *testing.T) {
	disk := newSimDisk()
	s := openSim(t, disk)

	acked := map[string][]byte{}
	put := func(k, v string) {
		t.Helper()
		if err := s.Put(k, []byte(v)); err == nil {
			acked[k] = []byte(v)
		}
	}

	put("a", "one")
	if len(acked) != 1 {
		t.Fatal("the first Put failed before anything was injected")
	}

	seg, err := findSimSegment(disk)
	if err != nil {
		t.Fatalf("locating the log segment: %v", err)
	}
	disk.FailNext(seg, "sync", 0, errSimulatedIO)

	if err := s.Put("b", []byte("two")); err == nil {
		t.Fatal("Put returned nil over an fsync that failed - an acknowledgement means the fsync covering the record returned successfully, not that it returned")
	} else if !errors.Is(err, errSimulatedIO) {
		t.Errorf("Put failed with %v, want the disk's own error wrapped - swallowing it and reporting something else loses the only information the caller has", err)
	}

	// Whatever the store does from here, it must not hand out a promise it
	// cannot keep.
	put("c", "three")

	disk.Crash()

	reopened := openSim(t, disk)
	for k, v := range acked {
		got, ok := reopened.Get(k)
		if !ok {
			t.Errorf("%q is gone after the power cut - Put acknowledged it, so it had to be durable. A commit that could not be synced leaves a gap in the log, and every record written after that gap is unreachable however well it was synced", k)
			continue
		}
		if !bytes.Equal(got, v) {
			t.Errorf("%q came back as %q, want %q", k, got, v)
		}
	}
}

// B1, B3. A write that only took some of its bytes.
//
// (*os.File).WriteAt loops until the buffer is written or the write returns an
// error, so a caller sees a short write only when the filesystem really could
// not finish - ENOSPC part way through a batch is the ordinary way there. The
// bytes that did land are a partial record, which is a torn tail; the store
// must not chain the next record on top of it.
func TestAShortWriteNeverProducesAnAcknowledgement(t *testing.T) {
	disk := newSimDisk()
	s := openSim(t, disk)

	acked := map[string][]byte{}
	put := func(k, v string) {
		t.Helper()
		if err := s.Put(k, []byte(v)); err == nil {
			acked[k] = []byte(v)
		}
	}

	put("a", "one")
	put("b", "two")

	seg, err := findSimSegment(disk)
	if err != nil {
		t.Fatalf("locating the log segment: %v", err)
	}
	// Ten bytes of a record that is a good deal longer: a header cut in half.
	disk.FailNext(seg, "writeat", 10, io.ErrShortWrite)

	if err := s.Put("c", []byte(strings.Repeat("c", 200))); err == nil {
		t.Fatal("Put returned nil over a short write - only part of the record reached the file")
	}

	put("d", "four")

	disk.Crash()

	reopened := openSim(t, disk)
	for k, v := range acked {
		if got, ok := reopened.Get(k); !ok || !bytes.Equal(got, v) {
			t.Errorf("acknowledged key %q = %q, %v after a short write; want %q, true", k, got, ok, v)
		}
	}
	if got, ok := reopened.Get("c"); ok {
		t.Errorf("Get(c) = %q, true - that record was never fully written and must not be returned", got)
	}
}

// B1. A write that failed outright, taking no bytes at all.
//
// This is the friendliest of the three failures - nothing reached the file, so
// there is no gap to write past - and it is here because the store must not
// distinguish. A caller cannot tell how far a failed write got, so neither
// should the guarantee.
func TestAFailedWriteNeverProducesAnAcknowledgement(t *testing.T) {
	disk := newSimDisk()
	s := openSim(t, disk)

	acked := map[string][]byte{}
	put := func(k, v string) {
		t.Helper()
		if err := s.Put(k, []byte(v)); err == nil {
			acked[k] = []byte(v)
		}
	}

	put("a", "one")

	seg, err := findSimSegment(disk)
	if err != nil {
		t.Fatalf("locating the log segment: %v", err)
	}
	disk.FailNext(seg, "writeat", 0, errSimulatedIO)

	if err := s.Put("b", []byte("two")); err == nil {
		t.Fatal("Put returned nil over a write that took no bytes")
	}
	put("c", "three")

	disk.Crash()

	reopened := openSim(t, disk)
	for k, v := range acked {
		if got, ok := reopened.Get(k); !ok || !bytes.Equal(got, v) {
			t.Errorf("acknowledged key %q = %q, %v after a failed write; want %q, true", k, got, ok, v)
		}
	}
}

// B3. A read that fails during recovery is not the end of the log.
//
// readAll stops at io.EOF, which is the file having been shorter than Stat
// said - a real thing a concurrent truncation does. Every other error is a
// failure to read bytes that are there, and treating it as the end of the log
// would hand back a store quietly missing every record after it. That
// distinction lives in one `if` in file.go, and until this test nothing
// exercised it: changing `readErr = err` to `_ = err` left the suite green.
func TestARecoveryReadFailureIsReportedRatherThanTakenAsTheEndOfTheLog(t *testing.T) {
	disk := newSimDisk()
	s := openSim(t, disk)

	for _, k := range []string{"a", "b", "c"} {
		if err := s.Put(k, []byte("value-"+k)); err != nil {
			t.Fatalf("Put(%s): %v", k, err)
		}
	}
	seg, err := findSimSegment(disk)
	if err != nil {
		t.Fatalf("locating the log segment: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	disk.FailNext(seg, "readat", 0, errSimulatedIO)

	reopened, err := OpenWith(Options{Dir: "sim", fsys: disk.FS("sim")})
	if err == nil {
		t.Cleanup(func() { _ = reopened.Close() })
		t.Fatalf("Open succeeded over a log segment that could not be read: it reported %d applied records and %d live keys. A read error is not the end of the log, and taking it for one returns a store silently missing everything after the bad sector",
			reopened.Recovery().Applied, reopened.Len())
	}
	if !errors.Is(err, errSimulatedIO) {
		t.Errorf("Open failed with %v, want the disk's own error wrapped", err)
	}
}

// B6. The one invariant file.go documents as load-bearing and nothing checked.
//
// checkpointLocked creates the next segment by name and relies on create
// failing if that name is already taken: a segment it silently reopened would
// be appended to from sequence zero, on top of records that are still live.
// simFS models ErrExist correctly, so the simulator was never the problem -
// nothing simply exercised the collision. Removing O_EXCL from osFS.create
// left the whole suite green.
func TestCreatingAFileThatAlreadyExistsFails(t *testing.T) {
	fsys := osFS{dir: t.TempDir()}
	name := segmentName(900)

	f, err := fsys.create(name)
	if err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	if _, err := f.WriteAt([]byte("live records"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := fsys.create(name)
	if err == nil {
		_ = again.Close()
		t.Fatal("create succeeded over an existing file - checkpointLocked relies on this failing, and a segment reopened silently is one that gets written over from the start")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Errorf("create failed with %v, want os.ErrExist", err)
	}
}
