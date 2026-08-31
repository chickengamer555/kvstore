package kvstore

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func dirBytes(t *testing.T, dir string, match func(name string) bool) int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() || !match(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		total += info.Size()
	}
	return total
}

func anyName(string) bool { return true }

// B6. An unbounded log works in a demo and fills the disk in production. This
// writes an order of magnitude more bytes than the bound and asserts the live
// log never gets near it.
//
// The key space is deliberately small and fixed, so the checkpoint itself is
// bounded too - which is the honest version of this claim. The log is bounded
// by checkpointing; the checkpoint is bounded by the size of the live key set,
// and nothing here bounds that.
func TestLogBoundedUnderSustainedWrites(t *testing.T) {
	dir := t.TempDir()
	const bound = 32 << 10
	const keys = 64
	const valueSize = 512

	s, err := OpenWith(Options{Dir: dir, CheckpointBytes: bound})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	defer func() { _ = s.Close() }()

	value := bytes.Repeat([]byte{'x'}, valueSize)
	var written int64
	peak := int64(0)
	for i := range 1200 {
		if err := s.Put(fmt.Sprintf("key-%03d", i%keys), value); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		written += valueSize
		if live := dirBytes(t, dir, isSegmentName); live > peak {
			peak = live
		}
	}

	if written < 10*bound {
		t.Fatalf("wrote only %d bytes against a bound of %d; the test has to exceed it by 10x to mean anything", written, bound)
	}
	if peak >= 2*bound {
		t.Fatalf("live log peaked at %d bytes against a bound of %d - checkpointing is not keeping up", peak, bound)
	}
	if s.Recovery().Segments > 2 && peak == 0 {
		t.Fatal("no log segments were measured")
	}
	total := dirBytes(t, dir, anyName)
	if total >= 4*bound+keys*(valueSize+32) {
		t.Errorf("whole store directory is %d bytes for %d live keys; checkpoints are not replacing the log they fold", total, keys)
	}
	t.Logf("wrote %d bytes; live log peaked at %d; directory settled at %d", written, peak, total)
}

// B6. Checkpointing is the one path that deletes data on purpose, so it is
// where crash-safety bugs concentrate. Everything acknowledged before, during
// and after a checkpoint has to still be there.
func TestRecoveryAfterCheckpointPreservesAcked(t *testing.T) {
	dir := t.TempDir()
	const bound = 8 << 10
	want := map[string]string{}

	// Deliberately no Close: a process that dies has not closed anything, and
	// nothing acknowledged may depend on Close having run.
	s, err := OpenWith(Options{Dir: dir, CheckpointBytes: bound})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	// Closed only after the test body has finished reopening the directory.
	// On Windows an unclosed handle stops t.TempDir cleaning up, and leaving
	// the leak would be a real bug in the test rather than a point about
	// durability.
	t.Cleanup(func() { _ = s.Close() })

	for i := range 400 {
		k := fmt.Sprintf("k%03d", i)
		v := fmt.Sprintf("v%03d-%s", i, bytes.Repeat([]byte{'p'}, 60))
		mustPut(t, s, k, v)
		want[k] = v
	}
	for i := 0; i < 400; i += 5 {
		k := fmt.Sprintf("k%03d", i)
		if err := s.Delete(k); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		delete(want, k)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopening a store abandoned mid-life: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	rep := reopened.Recovery()
	if !rep.UsedCheckpoint {
		t.Fatalf("recovery did not use a checkpoint; report was %+v - the bound of %d should have forced several", rep, bound)
	}
	if reopened.Len() != len(want) {
		t.Errorf("recovered %d keys, want %d", reopened.Len(), len(want))
	}
	for k, v := range want {
		got, ok := reopened.Get(k)
		if !ok || !bytes.Equal(got, []byte(v)) {
			t.Fatalf("acknowledged key %q = %q, %v; want %q, true (report %+v)", k, got, ok, v, rep)
		}
	}
	for i := 0; i < 400; i += 5 {
		if _, ok := reopened.Get(fmt.Sprintf("k%03d", i)); ok {
			t.Fatalf("key %s was deleted and acknowledged as deleted, but came back", fmt.Sprintf("k%03d", i))
		}
	}
	t.Logf("recovery report: %+v", rep)
}

// B6. A checkpoint file is exactly as trustworthy as a log record: a crash can
// leave it half written. Recovery must reject it on its checksum and fall back
// to the log, which is still complete because segments are only deleted after
// the checkpoint that replaces them is durable.
func TestPartialCheckpointIsIgnored(t *testing.T) {
	dir := t.TempDir()
	want := map[string]string{}
	func() {
		// A bound large enough that the store never checkpoints on its own, so
		// every segment is still present when the damaged checkpoint appears.
		s, err := OpenWith(Options{Dir: dir, CheckpointBytes: 1 << 30})
		if err != nil {
			t.Fatalf("OpenWith: %v", err)
		}
		defer func() { _ = s.Close() }()
		for i := range 20 {
			k := fmt.Sprintf("k%02d", i)
			v := fmt.Sprintf("v%02d", i)
			mustPut(t, s, k, v)
			want[k] = v
		}
	}()

	t.Run("truncated mid-payload", func(t *testing.T) {
		writeDamagedCheckpoint(t, dir, func(b []byte) []byte { return b[:len(b)-7] })
		assertFullLogReplay(t, dir, want)
	})

	t.Run("a flipped byte in the payload", func(t *testing.T) {
		writeDamagedCheckpoint(t, dir, func(b []byte) []byte {
			b[len(b)-3] ^= 0x20
			return b
		})
		assertFullLogReplay(t, dir, want)
	})

	t.Run("header only, no payload", func(t *testing.T) {
		writeDamagedCheckpoint(t, dir, func(b []byte) []byte { return b[:checkpointHeaderSize] })
		assertFullLogReplay(t, dir, want)
	})
}

// writeDamagedCheckpoint builds a well-formed checkpoint over a state that is
// NOT what the log says, then damages it. If recovery accepted it, the wrong
// values would surface - so the assertions below are checking the log won,
// not merely that Open returned.
func writeDamagedCheckpoint(t *testing.T, dir string, damage func([]byte) []byte) {
	t.Helper()
	payload := encodeState(map[string][]byte{"k00": []byte("WRONG"), "ghost": []byte("WRONG")})
	buf := make([]byte, checkpointHeaderSize, checkpointHeaderSize+len(payload))
	copy(buf, checkpointMagic)
	binary.LittleEndian.PutUint64(buf[12:], 20)
	buf = append(buf, payload...)
	binary.LittleEndian.PutUint32(buf[8:], checkpointChecksum(buf))

	if err := os.WriteFile(filepath.Join(dir, checkpointName), damage(buf), 0o644); err != nil {
		t.Fatalf("writing the damaged checkpoint: %v", err)
	}
}

func assertFullLogReplay(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open with a damaged checkpoint returned an error: %v - the log is intact, so this must recover", err)
	}
	defer func() { _ = s.Close() }()

	rep := s.Recovery()
	if !rep.CheckpointRejected {
		t.Errorf("recovery did not report rejecting the checkpoint; report was %+v", rep)
	}
	if rep.UsedCheckpoint {
		t.Errorf("recovery used a damaged checkpoint; report was %+v", rep)
	}
	if s.Len() != len(want) {
		t.Errorf("recovered %d keys, want %d", s.Len(), len(want))
	}
	for k, v := range want {
		got, ok := s.Get(k)
		if !ok || !bytes.Equal(got, []byte(v)) {
			t.Errorf("key %q = %q, %v; want %q, true", k, got, ok, v)
		}
	}
	if _, ok := s.Get("ghost"); ok {
		t.Error("a key that exists only in the damaged checkpoint was returned from Get")
	}
}

// The window a crash during checkpointing actually leaves behind: the
// checkpoint is durable, the new segment exists, and the old segments have not
// been deleted yet. Recovery then sees records it already has.
//
// What this test establishes is idempotence, and that is weaker than it used
// to claim. Replaying every surviving superseded record over the checkpoint
// lands on the same values it started from, because the checkpoint is a fold
// of the complete prefix and what survives a crash mid-deletion is a SUFFIX of
// that prefix - checkpointLocked unlinks oldest first, and loadDir refuses a
// gap between segments. So this window cannot lose data with or without the
// skip in loadDir, and the assertions below are correspondingly hard to fail.
// The case where the skip is load-bearing is a stopped replay, and it has its
// own test directly beneath this one.
//
// The auto-checkpoint path deletes the old segments immediately, so this
// reconstructs the window by hand: snapshot the directory, checkpoint, put the
// old segments back.
func TestRecoveryIgnoresRecordsTheCheckpointAlreadyCovers(t *testing.T) {
	dir := t.TempDir()
	want := map[string]string{}

	s, err := OpenWith(Options{Dir: dir, CheckpointBytes: 1 << 30})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i := range 30 {
		k := fmt.Sprintf("k%02d", i%10)
		v := fmt.Sprintf("round%d-key%d", i/10, i%10)
		mustPut(t, s, k, v)
		want[k] = v
	}
	saved := copySegments(t, dir)

	if err := s.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// Writes after the checkpoint, so the old segments carry strictly older
	// values for the same keys - which is what makes re-application visible.
	for i := range 10 {
		k := fmt.Sprintf("k%02d", i)
		v := fmt.Sprintf("after-checkpoint-%d", i)
		mustPut(t, s, k, v)
		want[k] = v
	}

	clean := snapshotOf(t, dir)
	for name, content := range saved {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatalf("restoring %s: %v", name, err)
		}
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopening with the pre-checkpoint segments restored: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	// A precondition on the test's own setup, not a correctness assertion:
	// if the restore did not put superseded records back, the window was never
	// staged and everything below is checking nothing. Deleting the skip in
	// loadDir turns this line red and leaves every assertion after it green,
	// which is the whole reason the next test exists.
	if rep := reopened.Recovery(); rep.Skipped == 0 {
		t.Fatalf("the restored segments hold 30 records the checkpoint already covers but recovery skipped none of them, so this test never staged its own window; report was %+v", rep)
	}
	for k, v := range want {
		got, ok := reopened.Get(k)
		if !ok || !bytes.Equal(got, []byte(v)) {
			t.Errorf("key %q = %q, %v; want %q, true - an old record was re-applied over a newer one", k, got, ok, v)
		}
	}
	if !bytes.Equal(clean, reopened.Snapshot()) {
		t.Error("recovering with the superseded segments present produced different state from recovering without them")
	}
}

// B6, B4, and the answer to "construct the sequence where re-applying
// superseded records actually loses data".
//
// It is not the crash window above. Re-applying a suffix of the superseded
// records is a no-op, because the checkpoint is already a fold of the whole
// prefix they came from. Re-applying a PREFIX of them is not a no-op at all:
// it puts back values the checkpoint has already superseded, and every key the
// prefix touches goes backwards in time.
//
// A prefix is what gets applied when replay stops partway through a superseded
// segment. So: a checkpoint at sequence 30, and beneath it a segment holding
// records 1..15 followed by eight bytes of a record that never finished.
// Recovery walks records 1..15, hits the torn tail and stops. With the skip in
// loadDir the fifteen records are verified and discarded and the state is the
// checkpoint's. Without it, all ten keys come back one or two rounds stale -
// measured, not argued: k00..k04 revert to round1 and k05..k09 to round0.
//
// Honesty about what this stages. This store does not currently produce that
// directory. It needs a segment with a tail recovery cannot vouch for that is
// NOT the last segment, and the two paths that could leave one are closed:
// a commit that fails poisons the segment and checkpointLocked now refuses to
// rotate off it, and a torn tail from a power cut is by construction at the
// end of the newest segment. So this is a log the FORMAT admits rather than a
// crash the store can take today - which is the reason to assert it here.
// Recovery is the wrong place to depend on an invariant maintained three files
// away, and the skip costs one comparison.
//
// The test deliberately stops at the reopen and does not write afterwards. In
// this state the recovered sequence counter is 15 while the checkpoint claims
// 30, so the next record would take a number the checkpoint already covers.
// That is a second consequence of the same unreachable directory and it is
// recorded as a gap rather than fixed here, because fixing it means moving the
// sequence chain inside a segment and that is a log format change.
func TestAStoppedReplayNeverReAppliesSupersededRecords(t *testing.T) {
	dir := t.TempDir()

	s, err := OpenWith(Options{Dir: dir, CheckpointBytes: 1 << 30})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}

	round := func(i int) string { return fmt.Sprintf("round%d-key%d", i/10, i%10) }
	key := func(i int) string { return fmt.Sprintf("k%02d", i%10) }

	for i := range 15 {
		mustPut(t, s, key(i), round(i))
	}
	// Fifteen records: every key once, then k00..k04 a second time. Whatever
	// is in these bytes is strictly older than the checkpoint written below.
	firstFifteen := copySegments(t, dir)

	want := map[string]string{}
	for i := 15; i < 30; i++ {
		mustPut(t, s, key(i), round(i))
		want[key(i)] = round(i)
	}
	if len(want) != 10 {
		t.Fatalf("the second run of writes covered %d keys, want all 10 - otherwise some key's newest value is in the truncated prefix and the test cannot tell re-application from correct recovery", len(want))
	}

	if err := s.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	clean := s.Snapshot()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Put the superseded segment back, with a record that never finished on
	// the end of it. Eight bytes: less than a header, so replay stops there
	// rather than reading a length out of noise.
	for name, content := range firstFifteen {
		torn := append(append([]byte{}, content...), 1, 2, 3, 4, 5, 6, 7, 8)
		if err := os.WriteFile(filepath.Join(dir, name), torn, 0o644); err != nil {
			t.Fatalf("restoring %s: %v", name, err)
		}
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopening with a superseded segment whose tail is torn: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	for k, v := range want {
		got, ok := reopened.Get(k)
		if !ok || !bytes.Equal(got, []byte(v)) {
			t.Errorf("key %q = %q, %v; want %q, true - replay stopped inside a run of records the checkpoint already covers, and applying the part it did read put a stale value back over a newer one", k, got, ok, v)
		}
	}
	if !bytes.Equal(clean, reopened.Snapshot()) {
		t.Error("recovering over a partly-replayed superseded segment produced different state from the checkpoint it was written from")
	}
}

func copySegments(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !isSegmentName(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		out[e.Name()] = b
	}
	return out
}

// A limitation, pinned rather than guaranteed - the second test in this
// repository of that kind, and it is here because it is the measurement behind
// a premise the documentation was arguing from.
//
// It builds its directory by hand, and that turned out to be the weakness: a
// reviewer reversed the unlink order in checkpointLocked and this test stayed
// green, because nothing here calls it. TestAPowerCutPartWayThroughTheUnlinks
// KeepsTheCounterLevelWithTheCheckpoint, at the bottom of this file, is the
// one that does. Both are worth having - this one isolates what the bad
// directory costs, that one proves the store can produce it.
//
// checkpointLocked deletes superseded segments oldest first and syncs the
// directory afterwards, so what a crash part way through leaves is a SUFFIX of
// them. Two arguments lean on that: the one that downgrades the
// superseded-record skip to "buys work, not correctness", and the row in
// docs/verification.md saying the syncDir after those removals costs nothing.
// Neither argument was ever measured against the alternative, so here is the
// alternative.
//
// Leave a PREFIX instead - the oldest segment still on disk under a checkpoint
// that covers it, with the live segment above the hole - and recovery stops at
// the hole, exactly as the test above requires it to, and then OpenWith deletes
// everything past the stop point. The ten records in the live segment were
// acknowledged, are in no checkpoint, and are gone. No crash is involved.
//
// So the unlink order is not a tidiness preference, it is load-bearing, and
// the reason it is load-bearing is that recovery DROPS what it cannot reach
// rather than refusing to open. That is the open gap - the abut check turns a
// missing file into a stop, and the drop turns the stop into deletion - and
// this test is where it is written down. Closing it means loadDir refusing
// rather than reporting a stop when the gap is between segments rather than at
// the end of the last one, which changes when the store declines to open at
// all and has the 240-seed corpus downstream of it. When that changes, this
// test changes with it: it is a record of what happens today, not a
// requirement that it keep happening.
func TestAnUnlinkOrderOtherThanOldestFirstLosesAcknowledgedWrites(t *testing.T) {
	dir := t.TempDir()
	firstSegment, _ := buildThreeRounds(t, dir)

	// The checkpoint covering records 1..20 stays where it is. Put the segment
	// based at 0 back underneath it: superseded, redundant, and in the way.
	for name, content := range firstSegment {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatalf("restoring %s: %v", name, err)
		}
	}
	if got := segmentBases(t, dir); len(got) != 2 || got[0] != 0 || got[1] != 20 {
		t.Fatalf("staging: segments %v, want [0 20] under a checkpoint covering 20", got)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	lost := 0
	for i := range 10 {
		k := roundKey(i)
		got, ok := reopened.Get(k)
		if !ok {
			t.Errorf("key %q is absent entirely; the checkpoint covering records 1..20 holds a value for every key", k)
			continue
		}
		if string(got) == "round2-"+k {
			continue
		}
		lost++
	}
	if lost != 10 {
		t.Errorf("%d of 10 acknowledged records from the live segment were lost, not 10 - recovery's behaviour over a superseded segment left below a hole has CHANGED. If loadDir now refuses to open, or reaches the segment above the gap, that is an improvement and this test is what has to be rewritten to say so; do not delete it", lost)
	}
	if _, ok := reopened.Get(gapKey); !ok {
		t.Errorf("key %q is in the checkpoint that covers records 1..20 and did not come back, so this test is not staging what it says it is", gapKey)
	}
}

// B6 and B1, and the closing of a gap this repository had written down and
// left open.
//
// A reviewer reversed the unlink loop in checkpointLocked to newest-first,
// left the listSegments sort alone, and ran the whole suite plus all 240
// corpus seeds. Everything stayed green - while three separate comments argue
// from that order being oldest first. The premise sweep in
// docs/verification.md had already found the same hole and filed it with the
// remedy, and then shipped it open.
//
// The test above it measures what the wrong order costs from a hand-built
// directory. That is why the reversal survived: no test made the STORE produce
// the directory. This one does, from the store's own API and two power cuts,
// with nothing written by hand.
//
//	put 10, checkpoint, power cut at the second syncDir - after the new
//	segment's name is durable and before a single unlink. That leaves segments
//	{0, 10} under a checkpoint covering 10, which is a state this store
//	reaches whenever a machine loses power mid-checkpoint.
//
//	reopen, put 10 more, checkpoint again. Now there are TWO segments to
//	unlink, which is the first time the order can matter at all - and it is
//	why the crash points in TestAPowerCutAnywhereInTheCheckpointPathLosesNothing
//	never reached it: that test's checkpoint has exactly one remove.
//
//	power cut after the first unlink.
//
// Oldest first leaves {10, 20}: a suffix, it abuts, replay reaches the live
// segment and the recovered sequence counter is 20, level with the checkpoint.
// Newest first leaves {0, 20}: replay stops at the gap, OpenWith drops segment
// 20, and the store opens with its counter at 10 while the checkpoint claims
// 20. Nothing is missing yet - the checkpoint still holds all twenty keys -
// which is exactly why the suite stayed green. The loss is one write and one
// reopen later: the next Put takes sequence 11, the checkpoint covers 20, and
// the recovery after that skips it as superseded. An acknowledged write, gone,
// with no second crash.
//
// So the last two assertions here are the ones that matter, and they are the
// ones no existing test made. docs/verification.md called this directory - a
// recovered counter behind the checkpoint - one "the store does not produce".
// Under the shipped order it still does not. It is one reversed loop away.
func TestAPowerCutPartWayThroughTheUnlinksKeepsTheCounterLevelWithTheCheckpoint(t *testing.T) {
	const big = 1 << 20 // nothing checkpoints on its own
	disk := newSimDisk()
	open := func() *Store {
		t.Helper()
		s, err := OpenWith(Options{Dir: "sim", CheckpointBytes: big, fsys: disk.FS("sim")})
		if err != nil {
			t.Fatalf("OpenWith: %v", err)
		}
		return s
	}

	acked := map[string]string{}
	put := func(s *Store, k, v string) {
		t.Helper()
		if err := s.Put(k, []byte(v)); err != nil {
			t.Fatalf("Put(%s): %v", k, err)
		}
		acked[k] = v
	}

	// First power cut: after the checkpoint is installed and its successor
	// segment is durable, before any unlink. Leaves two segments where a clean
	// checkpoint would have left one.
	s := open()
	for i := range 10 {
		put(s, fmt.Sprintf("k%02d", i), fmt.Sprintf("r0-%02d", i))
	}
	disk.CrashAtNth("syncdir", 2)
	if op, cut := runUntilPowerCut(t, func() { _ = s.Checkpoint() }); !cut {
		t.Fatalf("the first checkpoint never reached the second syncDir; it stopped at %q", op)
	}
	if got := segmentBasesOnDisk(t, disk); len(got) != 2 {
		t.Fatalf("after the first power cut the directory holds segments %v, want two of them - the staging this test needs is a checkpoint whose unlinks never ran", got)
	}

	// Ten more acknowledged writes, then the checkpoint with two unlinks to do.
	//
	// From here the unlinks are durable as they are issued. Without that the
	// order cannot be observed at all: a crash mid-loop reverts every unlink
	// at once, so both orders leave all three segments and both recover
	// identically. See PromoteUnlinksEarly - that blind spot is why the
	// reversal survived the whole suite.
	disk.PromoteUnlinksEarly()
	s = open()
	for i := 10; i < 20; i++ {
		put(s, fmt.Sprintf("k%02d", i), fmt.Sprintf("r1-%02d", i))
	}
	disk.CrashAtNth("remove", 1)
	if op, cut := runUntilPowerCut(t, func() { _ = s.Checkpoint() }); !cut {
		t.Fatalf("the second checkpoint never reached its first remove; it stopped at %q", op)
	}
	if got := segmentBasesOnDisk(t, disk); len(got) != 2 {
		t.Fatalf("after the second power cut the directory holds segments %v, want two - one of the three was unlinked and the power went before the next", got)
	}

	// Nothing is lost yet under either order, because the checkpoint covers
	// everything acknowledged so far. This assertion is here to say so: it is
	// the one the wrong order also passes.
	reopened := open()
	for k, v := range acked {
		if got, ok := reopened.Get(k); !ok || string(got) != v {
			t.Fatalf("acknowledged key %q = %q, %v immediately after the power cut inside the unlinks; want %q, true", k, got, ok, v)
		}
	}

	// The assertion that does the work. The recovered log has to account for
	// the checkpoint: if replay stopped below it, the counter is behind, and
	// the next record written takes a number the checkpoint already covers.
	rep := reopened.Recovery()
	if rep.LastSeq < rep.CheckpointSeq {
		t.Errorf("recovery stopped at sequence %d under a checkpoint covering %d, so the log no longer accounts for the checkpoint. The next write takes a number the checkpoint already contains and the recovery after that will skip it. Report: %+v", rep.LastSeq, rep.CheckpointSeq, rep)
	}

	// And the consequence, measured rather than argued: one more acknowledged
	// write, one more reopen, is it still there.
	put(reopened, "after-the-crash", "kept")
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	final := open()
	defer func() { _ = final.Close() }()
	for k, v := range acked {
		if got, ok := final.Get(k); !ok || string(got) != v {
			t.Errorf("acknowledged key %q = %q, %v after reopening the store that was recovered from the interrupted unlinks; want %q, true", k, got, ok, v)
		}
	}
}

// segmentBasesOnDisk reads the segment bases straight off the simulated
// platter, so a test can check what a power cut left without opening a store -
// opening one repairs the directory it opens.
func segmentBasesOnDisk(t *testing.T, d *simDisk) []uint64 {
	t.Helper()
	var out []uint64
	for _, name := range d.DurableNames() {
		if base, ok := segmentBase(name); ok {
			out = append(out, base)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
