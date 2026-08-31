package kvstore

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// These tests run the real store against the simulated disk in simdisk_test.go
// - the real Put, the real log, the real recovery, with only the platter
// replaced. They cover the three things a process-kill harness provably
// cannot reach, and which `crashrepro -corpus-shapes` measured the 240-seed
// corpus to produce exactly zero of:
//
//	a power cut, which loses everything not fsynced
//	a page that was half written when the power went
//	a write made durable out of order, leaving a hole
//
// After Process.Kill the page cache is intact and the kernel writes unsynced
// data out anyway, so the corpus cannot see any of this. That was a stated
// limitation of this repository and it is no longer one.

func openSim(t *testing.T, d *simDisk) *Store {
	t.Helper()
	s, err := OpenWith(Options{Dir: "sim", fsys: d.FS("sim")})
	if err != nil {
		t.Fatalf("OpenWith on the simulated disk: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// The simulator has to be falsifiable before anything built on it means
// anything. If Crash() quietly kept the pending layer, every test below would
// pass against a store that never fsynced - which is precisely the failure
// this whole seam exists to end. So this checks the disk against itself,
// through the same interface the store uses and nothing else.
func TestSimDiskLosesWritesThatWereNeverSynced(t *testing.T) {
	disk := newSimDisk()
	fsys := disk.FS("sim")

	f, err := fsys.create("F")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Make the name durable first, so what follows is about the contents and
	// nothing else. TestANewSegmentsDirectoryEntryIsMadeDurable covers the
	// other layer.
	if err := fsys.syncDir(); err != nil {
		t.Fatalf("syncDir: %v", err)
	}
	if _, err := f.WriteAt([]byte("unsynced"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// The writing process can read back its own unsynced write, exactly as it
	// can through the page cache on a real machine.
	got, err := readAll(fsys, "F")
	if err != nil {
		t.Fatalf("readAll before the crash: %v", err)
	}
	if string(got) != "unsynced" {
		t.Fatalf("before the crash the file read back %q, want %q", got, "unsynced")
	}

	disk.Crash()
	got, err = readAll(fsys, "F")
	if err != nil {
		t.Fatalf("readAll after the crash: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("after the power cut the file still held %q - the simulated disk is not discarding unsynced writes, so every test built on it is vacuous", got)
	}

	// And the other direction: a synced write survives, or the disk would
	// simply be broken rather than strict.
	if _, err := f.WriteAt([]byte("synced"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	disk.Crash()
	got, err = readAll(fsys, "F")
	if err != nil {
		t.Fatalf("readAll after the second crash: %v", err)
	}
	if string(got) != "synced" {
		t.Fatalf("after the power cut the synced file read back %q, want %q", got, "synced")
	}
}

// B2, the other half: the directory entry naming a new log segment.
//
// Creating a file and fsyncing it makes its CONTENTS durable. It does not make
// the entry that names it durable - that entry is the parent directory's own
// metadata, and on ext4 with data=ordered it can still be sitting in the
// journal when the power goes, leaving the file on the platter with nothing
// pointing at it. syncdir_unix.go is the fsync(2) on the directory descriptor
// that closes it, and until now the only test of it asserted that the store
// emitted a "dir-sync" event.
//
// The simulated disk models the entry as its own layer: a created file is
// readable at once but is deleted by Crash() unless syncDir has been called.
// Remove `fsys.syncDir()` from createSegment and this test fails with the log
// segment gone - see the red proof for
// simulated-disk/new-segments-directory-entry-is-made-durable.
//
// One boundary, stated rather than glossed: this proves the store PERFORMS a
// directory sync at the point it has to. Whether the platform's implementation
// of that call makes the entry durable is the platform's business - a real
// fsync(2) on POSIX, and a documented no-op on Windows where NTFS journals the
// metadata itself. That half is a claim about NTFS and syncdir_windows.go says
// so in those words.
func TestANewSegmentsDirectoryEntryIsMadeDurable(t *testing.T) {
	disk := newSimDisk()

	// First, the simulator against itself: a file created and fsynced, with no
	// directory sync, must not survive. Without this the test below would pass
	// against a disk that never modelled the entry at all.
	fsys := disk.FS("sim")
	orphan, err := fsys.create("orphan")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := orphan.WriteAt([]byte("contents are durable"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := orphan.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	disk.Crash()
	for _, n := range disk.Names() {
		if n == "orphan" {
			t.Fatal("a file whose directory was never synced survived the power cut - the simulated disk is not modelling the directory entry, so the assertion below would be vacuous")
		}
	}

	// Now the store. It creates a log segment, syncs it, and syncs the
	// directory. Everything acknowledged afterwards has to come back.
	s := openSim(t, disk)
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
		t.Fatal("the log segment is gone after the power cut - its directory entry was never made durable, so the file survived with no name and recovery cannot find it")
	}

	reopened := openSim(t, disk)
	if got, ok := reopened.Get("alpha"); !ok || !bytes.Equal(got, []byte("one")) {
		t.Errorf("Get(alpha) = %q, %v after the power cut; want %q, true", got, ok, "one")
	}
}

// B2, and the only test in this repository that can tell a store which fsyncs
// from a store which says it does.
//
// Nothing here inspects an event the store emitted or a counter the store
// incremented. The store writes, the power goes, and a second store opens the
// same platter. If the commit path did not fsync, the record never left the
// pending layer and the key is simply not there.
//
// Delete `w.f.Sync()` from walpolicy.go commit() and this test fails with
// "alpha is gone". That was checked, not assumed - see the red proof for
// simulated-disk/acked-write-survives-a-power-cut.
func TestAckedWriteSurvivesASimulatedPowerCut(t *testing.T) {
	disk := newSimDisk()
	s := openSim(t, disk)

	want := map[string][]byte{
		"alpha": []byte("one"),
		"beta":  []byte("two"),
		"gamma": bytes.Repeat([]byte("g"), 1500), // spans several pages
	}
	for _, k := range []string{"alpha", "beta", "gamma"} {
		if err := s.Put(k, want[k]); err != nil {
			t.Fatalf("Put(%s): %v", k, err)
		}
	}

	// The power goes here. No Close, no flush, no chance to tidy up - which is
	// the only condition under which the claim means anything.
	disk.Crash()

	if disk.Syncs() == 0 {
		t.Fatal("the disk recorded no fsync at all across three acknowledged writes")
	}
	// And the store's own counter, checked against the disk's rather than
	// trusted. Stats().Syncs is narration on its own - a number the store
	// increments about itself - and durability_test.go asserts on it in two
	// places with nothing outside the store agreeing.
	//
	// The two are not equal, and the difference is what writing this turned
	// up: the disk records one more fsync than the store counts, because
	// createSegment fsyncs the empty segment file before a record is in it and
	// that is not a commit. Three acknowledged Puts, three commit fsyncs, one
	// segment created. Stats.Syncs now says which of the two it counts.
	if reported, actual := s.Stats().Syncs, int64(disk.Syncs()); actual != reported+1 {
		t.Errorf("the store counted %d commit fsyncs and the disk recorded %d altogether; one segment was created here, so the disk should record exactly one more than the store does", reported, actual)
	}

	reopened := openSim(t, disk)
	for k, v := range want {
		got, ok := reopened.Get(k)
		if !ok {
			t.Errorf("%s is gone after the power cut - Put acknowledged it, so it had to be durable", k)
			continue
		}
		if !bytes.Equal(got, v) {
			t.Errorf("%s came back as %d bytes, want %d", k, len(got), len(v))
		}
	}
	if r := reopened.Recovery(); r.Stopped != stopEndOfLog {
		t.Errorf("recovery stopped for reason %q, want %q - every write here was synced, so the log should be intact", r.Stopped, stopEndOfLog)
	}
}

// B3, over the shape a process kill cannot make.
//
// A page is not written atomically. When the power goes part-way through
// making a page durable, the file is left with a prefix of the record and
// nothing after it - a torn tail produced by the real write path rather than
// by a test splicing bytes into a file, which is what record_test.go does.
//
// One honesty note about the model. On real hardware the fsync would never
// have returned and Put would never have acknowledged. Nothing in a Go test
// can model a call that does not come back, so the fault lets the Sync return
// and the test cuts the power on the next line. What is faithful is the state
// left on the platter, which is the only part recovery has to cope with.
func TestATornPageLeavesEveryAcknowledgedWriteRecoverable(t *testing.T) {
	disk := newSimDisk()
	s := openSim(t, disk)

	acked := []string{"a", "b", "c"}
	for _, k := range acked {
		if err := s.Put(k, []byte("acked-"+k)); err != nil {
			t.Fatalf("Put(%s): %v", k, err)
		}
	}

	seg, err := findSimSegment(disk)
	if err != nil {
		t.Fatalf("locating the log segment: %v", err)
	}
	before := len(disk.DurableBytes(seg))

	// The next fsync is interrupted after 30 bytes: a record header plus nine
	// bytes of a payload that claims far more.
	const torn = 30
	disk.FaultNextSync(seg, tornSync(torn))
	if err := s.Put("d", []byte(strings.Repeat("d", 300))); err != nil {
		t.Fatalf("Put(d): %v", err)
	}
	disk.Crash()

	if got := len(disk.DurableBytes(seg)); got != before+torn {
		t.Fatalf("the platter holds %d bytes, want %d - the torn page was not staged, so this test is not testing what it says", got, before+torn)
	}

	reopened := openSim(t, disk)
	for _, k := range acked {
		if got, ok := reopened.Get(k); !ok || !bytes.Equal(got, []byte("acked-"+k)) {
			t.Errorf("acknowledged key %q = %q, %v after a torn page; want %q, true", k, got, ok, "acked-"+k)
		}
	}
	if got, ok := reopened.Get("d"); ok {
		t.Errorf("Get(d) = %q, true - only 30 bytes of that record reached the platter and it must not be returned", got)
	}
	if r := reopened.Recovery(); r.Stopped != stopTornRecord {
		t.Errorf("recovery stopped for reason %q, want %q", r.Stopped, stopTornRecord)
	}

	// And the half that a test which only reads after recovery would miss.
	// The reopened log has to be cut back to the last byte recovery vouched
	// for, or the next record is written past the point the next recovery
	// stops at - fsynced, acknowledged, and then never read again. So: write
	// one more key, take the power away again, and open a third time.
	if err := reopened.Put("e", []byte("after-the-tear")); err != nil {
		t.Fatalf("Put(e) after recovering from a torn page: %v", err)
	}
	disk.Crash()

	third := openSim(t, disk)
	for _, k := range acked {
		if got, ok := third.Get(k); !ok || !bytes.Equal(got, []byte("acked-"+k)) {
			t.Errorf("acknowledged key %q = %q, %v after the second power cut; want %q, true", k, got, ok, "acked-"+k)
		}
	}
	if got, ok := third.Get("e"); !ok || !bytes.Equal(got, []byte("after-the-tear")) {
		t.Errorf("Get(e) = %q, %v - a write acknowledged after recovery was not readable, which is what happens when the torn tail is not truncated away", got, ok)
	}
}

// B3 and B4, over the other shape a process kill cannot make.
//
// A device with a volatile write cache may complete queued writes in any order
// it likes. If the power goes half way through, a later part of a write can be
// on the platter while an earlier part of the same write is not - and the gap
// reads back as zeros, because those bytes were never written.
//
// The property this pins is the one replayBytes exists for: recovery stops at
// the hole and never scans forward looking for something that decodes. The
// assertion below is deliberately in two halves - the tail fragment IS on the
// platter, and the key is still not readable - because a recovery that skipped
// the damage would satisfy the second half on its own by accident.
func TestAnOutOfOrderFlushIsNotSkippedOverOnRecovery(t *testing.T) {
	disk := newSimDisk()
	s := openSim(t, disk)

	acked := []string{"a", "b", "c"}
	for _, k := range acked {
		if err := s.Put(k, []byte("acked-"+k)); err != nil {
			t.Fatalf("Put(%s): %v", k, err)
		}
	}

	seg, err := findSimSegment(disk)
	if err != nil {
		t.Fatalf("locating the log segment: %v", err)
	}
	before := len(disk.DurableBytes(seg))

	// Big enough to span several pages, and ending in something recognisable
	// so the test can prove the out-of-order tail really landed.
	const marker = "REORDERED-TAIL!!"
	value := append(bytes.Repeat([]byte("x"), 2000), marker...)

	disk.FaultNextSync(seg, lastPageOnlySync())
	if err := s.Put("big", value); err != nil {
		t.Fatalf("Put(big): %v", err)
	}
	disk.Crash()

	platter := disk.DurableBytes(seg)
	if len(platter) <= before+simPageSize {
		t.Fatalf("the platter grew from %d to %d bytes - the out-of-order page was not staged", before, len(platter))
	}
	if !bytes.Contains(platter[before:], []byte(marker)) {
		t.Fatalf("the last page of the write is not on the platter, so this test is not testing an out-of-order flush")
	}
	if !bytes.Equal(platter[before:before+16], make([]byte, 16)) {
		t.Fatalf("the bytes at the start of the interrupted write are %x, want zeros - a page that was never written reads back as a hole", platter[before:before+16])
	}

	// Two independent recoveries of the same platter, because the first Open
	// repairs the directory and re-opening it would compare a crashed store
	// with a repaired one.
	twin := disk.Clone()
	reopened := openSim(t, disk)
	twinStore := openSim(t, twin)

	for _, k := range acked {
		if got, ok := reopened.Get(k); !ok || !bytes.Equal(got, []byte("acked-"+k)) {
			t.Errorf("acknowledged key %q = %q, %v after an out-of-order flush; want %q, true", k, got, ok, "acked-"+k)
		}
	}
	if got, ok := reopened.Get("big"); ok {
		t.Errorf("Get(big) = %d bytes, true - the record's own bytes were never made durable and recovery must not have reached past the hole to find its tail", len(got))
	}
	if r := reopened.Recovery(); r.Stopped == stopEndOfLog {
		t.Errorf("recovery reported %q over a log with a hole in it", r.Stopped)
	}
	if !bytes.Equal(reopened.Snapshot(), twinStore.Snapshot()) {
		t.Error("two independent recoveries of the same crashed platter produced different state")
	}
}

// The checkpoint path has its own fsync and its own ordering, and until now
// nothing established that the checkpoint's fsync was load-bearing either.
//
// The store is driven past its checkpoint bound, the power goes with no Close,
// and a second store opens the platter. Everything acknowledged has to be
// there whether it came back from the checkpoint or from the log that
// supersedes it.
func TestCheckpointedStateSurvivesASimulatedPowerCut(t *testing.T) {
	disk := newSimDisk()
	s, err := OpenWith(Options{Dir: "sim", CheckpointBytes: 4 << 10, fsys: disk.FS("sim")})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const n = 200
	for i := range n {
		if err := s.Put(fmt.Sprintf("k%03d", i), []byte(fmt.Sprintf("v%03d", i))); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
	if s.Stats().Checkpoints == 0 {
		t.Fatal("no checkpoint was taken, so this test is not exercising the checkpoint path")
	}

	disk.Crash()

	reopened, err := OpenWith(Options{Dir: "sim", CheckpointBytes: 4 << 10, fsys: disk.FS("sim")})
	if err != nil {
		t.Fatalf("reopening after the power cut: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	for i := range n {
		key := fmt.Sprintf("k%03d", i)
		want := fmt.Sprintf("v%03d", i)
		if got, ok := reopened.Get(key); !ok || string(got) != want {
			t.Fatalf("acknowledged key %q = %q, %v after the power cut; want %q, true", key, got, ok, want)
		}
	}
}

// The negative control, and the reason it is here rather than in crashtest/.
//
// A suite that has only ever run against correct code has not been shown to
// detect anything, so this repository builds a store that really does lose
// data - walpolicy_earlyack.go, which acknowledges writes while they are still
// in a user-space buffer - and requires the harness to catch it.
//
// Until now that requirement lived entirely in the crash corpus, and CI's
// first Linux run showed why that was not good enough: the same broken build,
// the same 24 seeds, caught 13 times on windows/amd64 and 4 times on
// ubuntu-latest. Nothing about the store changed between those runs. What
// changed is where a signal lands relative to a buffer flush, which is the
// scheduler's business and not this project's. A detection rate that moves by
// a factor of three across platforms is not a threshold anything should be
// asserted against.
//
// Under the simulated disk there is no rate. The broken build never gets the
// record to the file at all, so the power cut takes it every single time, on
// every platform. This is the control the rest of the file now rests on.
func TestTheBrokenBuildFailsThePowerCutTest(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("BLOCKED: no go toolchain on PATH, so the deliberately broken build could not be compiled and the negative control did not run: %v", err)
	}

	// Read the broken build's source, and not for show. walpolicy_earlyack.go
	// is excluded from this build by its tag, so it is not one of this test
	// binary's inputs - and `go test` will happily serve a cached PASS for this
	// package after that file has changed. I found that the hard way: the
	// suite reported green with the negative control disarmed. Opening the
	// file here registers it with the test cache, which is the only way a test
	// that shells out to a differently tagged build can be trusted to re-run.
	src, err := os.ReadFile("walpolicy_earlyack.go")
	if err != nil {
		t.Fatalf("reading the deliberately broken build: %v", err)
	}
	if !bytes.Contains(src, []byte("//go:build kvearlyack")) {
		t.Fatalf("walpolicy_earlyack.go is not the kvearlyack build any more")
	}

	// The child runs one named test, so it never re-enters this one.
	cmd := exec.Command(goBin, "test", "-count=1", "-tags", "kvearlyack",
		"-run", "^TestAckedWriteSurvivesASimulatedPowerCut$", ".")
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("the kvearlyack build PASSED the power-cut test. Either -tags kvearlyack no longer breaks anything, or the simulated disk stopped discarding unsynced writes - and both make every green run in powercut_test.go meaningless.\n%s", out)
	}
	if !bytes.Contains(out, []byte("is gone after the power cut")) {
		t.Fatalf("the broken build failed, but not on the assertion this control is about - it must lose an acknowledged key, not fall over for some other reason.\n%s", out)
	}
	t.Logf("the deliberately broken build loses acknowledged writes under the simulated power cut, deterministically")
}

// runUntilPowerCut runs fn and reports whether a simulated power cut stopped
// it part way through, and at which operation.
//
// The disk panics when an armed crash point fires, because a process does not
// carry on running after the power goes and neither may the store. Anything
// else that panics is a real failure and is re-raised untouched.
func runUntilPowerCut(t *testing.T, fn func()) (string, bool) {
	t.Helper()
	op, cut := "", false
	func() {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			pc, ok := r.(simPowerCut)
			if !ok {
				panic(r)
			}
			op, cut = pc.op, true
		}()
		fn()
	}()
	return op, cut
}

// B6, and the half of the directory-fsync claim that had nothing behind it.
//
// Checkpointing is the one path in this store that deletes data on purpose,
// and an unlink is a directory write like any other: until the directory is
// fsynced, the entry can come back. If it does, the log the checkpoint just
// superseded is on the platter again, and the log is bounded only until the
// machine loses power - which is the one moment the bound was for.
//
// Delete `s.fsys.syncDir()` from the end of checkpointLocked and this test
// fails, naming the segment that came back. That is checked, not argued: it
// was green against that deletion until the simulated disk started staging
// removes, which is what the commit before this one is about.
func TestACheckpointStillBoundsTheLogAfterAPowerCut(t *testing.T) {
	disk := newSimDisk()
	s, err := OpenWith(Options{Dir: "sim", CheckpointBytes: 4 << 10, fsys: disk.FS("sim")})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const n = 400
	want := map[string]string{}
	for i := range n {
		k := fmt.Sprintf("k%03d", i)
		v := fmt.Sprintf("v%03d", i)
		if err := s.Put(k, []byte(v)); err != nil {
			t.Fatalf("Put(%s): %v", k, err)
		}
		want[k] = v
	}
	if s.Stats().Checkpoints == 0 {
		t.Fatal("no checkpoint was taken, so this test is not exercising the checkpoint path")
	}
	live := segmentName(s.log.base)

	disk.Crash()

	for _, name := range disk.Names() {
		if isSegmentName(name) && name != live {
			t.Errorf("segment %q is back on the platter after the power cut, alongside the live %q - the unlinks that bound the log were never made durable, so the bound holds only until the machine loses power", name, live)
		}
	}

	reopened, err := OpenWith(Options{Dir: "sim", CheckpointBytes: 4 << 10, fsys: disk.FS("sim")})
	if err != nil {
		t.Fatalf("reopening after the power cut: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	for k, v := range want {
		if got, ok := reopened.Get(k); !ok || string(got) != v {
			t.Fatalf("acknowledged key %q = %q, %v after the power cut; want %q, true", k, got, ok, v)
		}
	}
}

// B6, and the window a test could not reach until the disk could crash inside
// one.
//
// checkpointLocked installs the checkpoint by renaming the temporary file over
// the real name, and rename(2) is atomic with respect to a reader - which is a
// different property from being durable. Until the directory is fsynced, a
// reopening process still finds the old name. So the power is taken away at
// the very next thing the store does, creating the segment that follows the
// checkpoint, and the checkpoint has to be there.
//
// Nothing is lost either way: the ordering in checkpointLocked is built so
// that a crash anywhere in it reverts the checkpoint and the deletions
// together, and recovery replays the log in full. This test is not about
// losing data. It is about the store having said it installed a checkpoint,
// and that being true one instruction later rather than whenever the next
// directory sync happens along.
//
// Delete the `fsys.syncDir()` after the rename in writeCheckpoint and this
// fails: recovery finds no checkpoint at all.
func TestTheCheckpointIsDurableAsSoonAsItIsInstalled(t *testing.T) {
	disk := newSimDisk()
	s, err := OpenWith(Options{Dir: "sim", CheckpointBytes: 4 << 10, fsys: disk.FS("sim")})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}

	// The store has already created the segment it opened with, so the next
	// create is the segment that follows its FIRST checkpoint. The power goes
	// as that call returns - after the checkpoint was installed, before
	// anything else has synced the directory, and before this store has ever
	// had a durable checkpoint to fall back to.
	disk.CrashAtNth("create", 1)

	acked := map[string]string{}
	op, cut := runUntilPowerCut(t, func() {
		for i := range 400 {
			k := fmt.Sprintf("k%03d", i)
			v := fmt.Sprintf("v%03d", i)
			if err := s.Put(k, []byte(v)); err != nil {
				t.Errorf("Put(%s): %v", k, err)
				return
			}
			acked[k] = v
		}
	})
	if !cut {
		t.Fatal("the store never created a second segment, so it never checkpointed and this test is not exercising the window it is named for")
	}
	if op != "create" {
		t.Fatalf("the power went at %q, want create", op)
	}

	reopened, err := OpenWith(Options{Dir: "sim", CheckpointBytes: 4 << 10, fsys: disk.FS("sim")})
	if err != nil {
		t.Fatalf("reopening after the power cut: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	// The platter first. r.UsedCheckpoint below is the reopened store telling
	// us what it found, and a report field the store fills in about itself is
	// exactly the kind of evidence this repository has already been caught
	// leaning on once. This line asks the disk instead, and it is the one that
	// still fires if someone deletes the two after it.
	if durable := disk.DurableNames(); !slices.Contains(durable, checkpointName) {
		t.Errorf("the durable directory after the power cut is %v, with no %s in it - the store had installed one by renaming over the real name, and that rename was never made durable", durable, checkpointName)
	}

	r := reopened.Recovery()
	if !r.UsedCheckpoint {
		t.Errorf("recovery found no checkpoint after the power cut (rejected=%v) - the store had installed one by renaming over the real name, and that rename was never made durable, so the platter still holds the directory as it was before it", r.CheckpointRejected)
	}
	if r.CheckpointSeq == 0 && r.UsedCheckpoint {
		t.Error("recovery used a checkpoint covering sequence 0, which is no checkpoint at all")
	}
	for k, v := range acked {
		if got, ok := reopened.Get(k); !ok || string(got) != v {
			t.Errorf("acknowledged key %q = %q, %v after the power cut; want %q, true", k, got, ok, v)
		}
	}
}

// B6, and the premise that the argument for removing a directory sync rests
// on.
//
// writeCheckpoint used to fsync the directory twice: once between creating the
// temporary file and renaming it over the real name, and once after. The first
// was backed out at b4565c5, on the argument that one fsync on the directory
// after the rename makes the whole of the directory state durable - the create
// included - so no crash could ever observe the earlier one.
//
// This test does NOT decide that question, and saying so is the point. It
// passes with one directory sync and it passes with two, because the two
// differ only in whether a stray CHECKPOINT.tmp is left on the platter, and
// nothing a store can observe after a crash depends on that. What it does is
// turn the argument's PREMISE from prose into an observation. The premise is
// that a power cut anywhere inside the checkpoint path loses nothing, because
// the checkpoint is not installed until writeCheckpoint returns and no segment
// is deleted until after that - so the store either comes back with the old
// checkpoint and every segment, or the new checkpoint and every segment, and
// both replay to the same keys.
//
// So the power is taken out at the end of every filesystem call the checkpoint
// path makes, one run per call, and every key acknowledged before it has to
// still be readable afterwards. The counts below are relative: CrashAtNth
// counts calls made after it is armed, and it is armed immediately before
// Checkpoint() is called, so this list is also a description of the syscalls
// that path issues and in what order.
func TestAPowerCutAnywhereInTheCheckpointPathLosesNothing(t *testing.T) {
	points := []struct {
		op   string
		n    int
		what string
	}{
		{"createtrunc", 1, "the temporary checkpoint file has just been created"},
		{"writeat", 1, "the checkpoint's bytes have been handed to the file"},
		{"sync", 1, "the checkpoint's bytes are durable, under the temporary name"},
		{"rename", 1, "the checkpoint is installed and the directory entry is not durable"},
		{"syncdir", 1, "the installed checkpoint is durable"},
		{"create", 1, "the segment that follows the checkpoint exists"},
		{"sync", 2, "that segment's contents are durable"},
		{"syncdir", 2, "that segment's name is durable"},
		{"remove", 1, "a superseded segment has been unlinked"},
		{"syncdir", 3, "the unlinks are durable"},
	}

	for _, p := range points {
		t.Run(fmt.Sprintf("%s-%d", p.op, p.n), func(t *testing.T) {
			disk := newSimDisk()
			// Large enough that nothing checkpoints on its own: the only
			// checkpoint in this test is the one called below, so the call
			// counts above mean what they say.
			s, err := OpenWith(Options{Dir: "sim", CheckpointBytes: 1 << 20, fsys: disk.FS("sim")})
			if err != nil {
				t.Fatalf("OpenWith: %v", err)
			}

			acked := map[string]string{}
			for i := range 50 {
				k := fmt.Sprintf("k%03d", i)
				v := fmt.Sprintf("v%03d", i)
				if err := s.Put(k, []byte(v)); err != nil {
					t.Fatalf("Put(%s): %v", k, err)
				}
				acked[k] = v
			}
			if s.Stats().Checkpoints != 0 {
				t.Fatal("the store checkpointed on its own, so the call counts below no longer describe the path this test crashes inside")
			}

			disk.CrashAtNth(p.op, p.n)
			op, cut := runUntilPowerCut(t, func() { _ = s.Checkpoint() })
			if !cut {
				t.Fatalf("the checkpoint path never reached call %d to %s, so nothing was crashed when %s", p.n, p.op, p.what)
			}
			if op != p.op {
				t.Fatalf("the power went at %q, want %q", op, p.op)
			}

			reopened, err := OpenWith(Options{Dir: "sim", CheckpointBytes: 1 << 20, fsys: disk.FS("sim")})
			if err != nil {
				t.Fatalf("reopening after the power cut when %s: %v", p.what, err)
			}
			defer func() { _ = reopened.Close() }()

			for k, v := range acked {
				if got, ok := reopened.Get(k); !ok || string(got) != v {
					t.Fatalf("acknowledged key %q = %q, %v after the power cut when %s; want %q, true", k, got, ok, p.what, v)
				}
			}
			if reopened.Len() != len(acked) {
				t.Errorf("the reopened store holds %d keys, want %d", reopened.Len(), len(acked))
			}
		})
	}
}
