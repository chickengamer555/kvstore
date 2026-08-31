package kvstore

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
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
