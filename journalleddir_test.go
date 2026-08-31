package kvstore

import (
	"bytes"
	"testing"
)

// What this file closes.
//
// TestANewSegmentsDirectoryEntryIsMadeDurable proves the store PERFORMS a
// directory sync at the point it has to, and stops there on purpose. Its own
// comment names the half it does not reach: "Whether the platform's
// implementation of that call makes the entry durable is the platform's
// business - a real fsync(2) on POSIX, and a documented no-op on Windows where
// NTFS journals the metadata itself. That half is a claim about NTFS and
// syncdir_windows.go says so in those words."
//
// It was a claim in a comment. Every simulated-disk test ran the POSIX rule,
// so nothing executed the Windows build's argument, and the repository said as
// much: syncdir_windows.go ends "Nothing in this file has been verified by the
// crash corpus; the corpus runs on Linux."
//
// The two knobs are independent, and separating them is the whole point:
//
//   - whether the store's syncDir does anything - that is the PLATFORM's code,
//     a real fsync(2) in syncdir_unix.go and a no-op in syncdir_windows.go,
//     modelled here by noDirSyncFS.
//   - whether the directory is journalled - that is the FILESYSTEM's behaviour,
//     ext4 data=ordered versus NTFS $LogFile, modelled by JournalMetadata.
//
// The Windows build is the first paired with the second. The test below runs
// that pair, and then runs the no-op against an UNJOURNALLED directory, which
// has to lose the write. Without that second half the first would pass against
// a disk that had quietly stopped modelling the entry at all, and would be
// worth nothing - the same trap TestANewSegmentsDirectoryEntryIsMadeDurable
// guards with its "orphan" preamble.
//
// The boundary, stated rather than glossed: this proves the store is correct
// GIVEN the documented NTFS contract. It does not prove NTFS honours it. That
// residual is not reachable by any model and it is the same shape as whether
// the drive honours a flush, which bench/results.md records as unknown.

// noDirSyncFS is syncdir_windows.go: every call the store makes to syncDir
// returns nil without touching the platter. Everything else is the disk
// underneath, unchanged.
type noDirSyncFS struct{ fileSystem }

func (noDirSyncFS) syncDir() error { return nil }

// The Windows pair: no-op syncDir, journalled directory. An acknowledged write
// has to survive the power cut, because the filesystem made the name durable
// even though this code did not ask it to.
func TestAJournalledDirectoryKeepsTheSegmentWithoutADirectorySync(t *testing.T) {
	disk := newSimDisk()
	disk.JournalMetadata()

	// The simulator against itself first. Under the journal rule a created and
	// fsynced file must survive with no syncDir at all - if it does not, the
	// knob is not doing what its name says and the store assertion below would
	// be measuring the default POSIX model instead.
	fsys := disk.FS("sim")
	f, err := fsys.create("orphan")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.WriteAt([]byte("contents are durable"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	disk.Crash()
	found := false
	for _, n := range disk.Names() {
		if n == "orphan" {
			found = true
		}
	}
	if !found {
		t.Fatal("a created and fsynced file did not survive the power cut under JournalMetadata - the journal rule is not being applied, so the store assertion below would be running the POSIX model and proving nothing about Windows")
	}

	// Now the store, with the platform's syncDir doing nothing.
	s, err := OpenWith(Options{Dir: "sim", fsys: noDirSyncFS{disk.FS("sim")}})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Put("alpha", []byte("one")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	disk.Crash()

	segments := 0
	for _, n := range disk.Names() {
		if isSegmentName(n) {
			segments++
		}
	}
	if segments == 0 {
		t.Fatal("the log segment is gone after the power cut - on a journalled directory the entry is the filesystem's responsibility and it should have survived without a syncDir")
	}

	reopened, err := OpenWith(Options{Dir: "sim", fsys: noDirSyncFS{disk.FS("sim")}})
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got, ok := reopened.Get("alpha"); !ok || !bytes.Equal(got, []byte("one")) {
		t.Errorf("Get(alpha) = %q, %v after the power cut; want %q, true - an acknowledged write was lost with the store's syncDir doing nothing on a journalled directory", got, ok, "one")
	}
}

// The control, and the reason syncdir_unix.go exists. The same no-op syncDir
// against an UNJOURNALLED directory has to lose the acknowledged write: the
// segment's contents reach the platter and the entry naming them does not.
//
// This is the Windows build's code running on POSIX rules. It is not a state
// any shipped build reaches - the two are selected together by the build tag -
// and that is exactly why it needs a test rather than an argument: nothing
// else in the suite can show that the pairing is load-bearing.
func TestAnUnjournalledDirectoryLosesTheSegmentWithoutADirectorySync(t *testing.T) {
	disk := newSimDisk() // no JournalMetadata: the ext4 data=ordered rule

	s, err := OpenWith(Options{Dir: "sim", fsys: noDirSyncFS{disk.FS("sim")}})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Put("alpha", []byte("one")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	disk.Crash()

	for _, n := range disk.Names() {
		if isSegmentName(n) {
			t.Fatalf("segment %q survived a power cut with no directory sync on an unjournalled directory - the disk is not modelling the directory entry, which would make the journalled test above vacuous", n)
		}
	}

	reopened, err := OpenWith(Options{Dir: "sim", fsys: noDirSyncFS{disk.FS("sim")}})
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, ok := reopened.Get("alpha"); ok {
		t.Error("Get(alpha) came back after a power cut with no durable directory entry - the entry cannot have been made durable, so this store found a name that was never on the platter")
	}
}
