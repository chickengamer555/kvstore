package kvstore

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func mustPut(t *testing.T, s *Store, k, v string) {
	t.Helper()
	if err := s.Put(k, []byte(v)); err != nil {
		t.Fatalf("Put(%q): %v", k, err)
	}
}

func snapshotOf(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	defer func() { _ = s.Close() }()
	return s.Snapshot()
}

// B4. Two replays of one directory have to produce the same bytes. If they do
// not, a crash can leave two machines reading the same log and disagreeing
// about the data - and every other durability test here becomes unfalsifiable,
// because a failure cannot be reproduced.
func TestReplayIsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	func() {
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = s.Close() }()
		for i := range 40 {
			mustPut(t, s, fmt.Sprintf("key-%02d", i), fmt.Sprintf("value-%d", i*7))
		}
		for i := 0; i < 40; i += 3 {
			if err := s.Delete(fmt.Sprintf("key-%02d", i)); err != nil {
				t.Fatalf("Delete: %v", err)
			}
		}
		mustPut(t, s, "key-00", "resurrected")
	}()

	first := snapshotOf(t, dir)
	second := snapshotOf(t, dir)

	if len(first) == 0 {
		t.Fatal("Snapshot() is empty for a store with 40 keys written to it")
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("two replays of %s differ: %d bytes vs %d bytes", dir, len(first), len(second))
	}

	// A snapshot that ignored the data would be trivially "deterministic", so
	// pin that it actually depends on the contents.
	other := t.TempDir()
	func() {
		s, err := Open(other)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = s.Close() }()
		mustPut(t, s, "key-00", "something else")
	}()
	if bytes.Equal(first, snapshotOf(t, other)) {
		t.Fatal("two stores with different contents serialise identically - the snapshot is not reading the data")
	}
}

// B4, over the shape a crash actually leaves behind. A torn tail makes recovery
// stop early; it must stop at the same place both times.
func TestReplayIsByteIdenticalAfterATornTail(t *testing.T) {
	dir := t.TempDir()
	func() {
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = s.Close() }()
		for i := range 12 {
			mustPut(t, s, fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
		}
	}()

	paths := segmentPaths(t, dir)
	if len(paths) != 1 {
		t.Fatalf("found %d segments, want 1", len(paths))
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(paths[0], raw[:len(raw)-9], 0o644); err != nil {
		t.Fatalf("tearing the log: %v", err)
	}

	first := snapshotOf(t, dir)
	second := snapshotOf(t, dir)
	if !bytes.Equal(first, second) {
		t.Fatalf("two replays of a torn log differ: %d bytes vs %d bytes", len(first), len(second))
	}
	if len(first) == 0 {
		t.Fatal("a torn log recovered to an empty store; 11 records before the tear should have survived")
	}
}

// B4. Go map iteration order is deliberately randomised, so a serialisation
// that walks the map produces different bytes on every run within a single
// process. Two stores holding the same pairs must serialise identically
// however they got there.
func TestSnapshotOrderIndependentOfInsertOrder(t *testing.T) {
	forward := t.TempDir()
	backward := t.TempDir()

	keys := []string{"delta", "alpha", "charlie", "bravo", "echo"}

	func() {
		s, err := Open(forward)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = s.Close() }()
		for _, k := range keys {
			mustPut(t, s, k, "v-"+k)
		}
	}()
	func() {
		s, err := Open(backward)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = s.Close() }()
		for i := len(keys) - 1; i >= 0; i-- {
			mustPut(t, s, keys[i], "v-"+keys[i])
		}
	}()

	a := snapshotOf(t, forward)
	b := snapshotOf(t, backward)
	if len(a) == 0 || len(b) == 0 {
		t.Fatalf("empty snapshots (%d, %d bytes) for stores holding %d keys", len(a), len(b), len(keys))
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("same contents, different insert order, different bytes:\n  %x\n  %x", a, b)
	}
}

// The serialised form has to survive a round trip, and - because the same
// codec decodes checkpoint files, which a crash can leave half written - it
// has to reject a truncated buffer rather than returning whatever it managed
// to read.
func TestStateRoundTripsAndRejectsTruncation(t *testing.T) {
	original := map[string][]byte{
		"":         []byte("empty key"),
		"alpha":    nil,
		"bravo":    []byte("two"),
		"\x00\x01": []byte{0xff, 0x00, 0x7f},
	}
	enc := encodeState(original)

	got, err := decodeState(enc)
	if err != nil {
		t.Fatalf("decodeState of a freshly encoded state: %v", err)
	}
	if len(got) != len(original) {
		t.Fatalf("round trip produced %d keys, want %d", len(got), len(original))
	}
	for k, v := range original {
		if !bytes.Equal(got[k], v) {
			t.Errorf("key %q round tripped to %q, want %q", k, got[k], v)
		}
	}

	for _, cut := range []int{1, len(enc) / 2, len(enc) - 1} {
		if _, err := decodeState(enc[:cut]); err == nil {
			t.Errorf("decodeState accepted a buffer truncated to %d of %d bytes", cut, len(enc))
		}
	}
}

// buildThreeRounds writes thirty records into dir across three segments and
// returns the bytes of the first segment and of the checkpoint that covers it,
// so that a caller can reassemble a directory the store would never leave.
//
// Records 1..10 set k0..k9 to round0. Records 11..20 set k0..k8 to round1 and
// write gapKey, which appears nowhere else in the log. Records 21..30 set
// k0..k9 to round2. Every round rotates the log, so the segments are based at
// 0, 10 and 20 and each round lives in exactly one of them.
func buildThreeRounds(t *testing.T, dir string) (firstSegment map[string][]byte, checkpointAt10 []byte) {
	t.Helper()

	s, err := OpenWith(Options{Dir: dir, CheckpointBytes: 1 << 30})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}

	for i := range 10 {
		mustPut(t, s, roundKey(i), "round0-"+roundKey(i))
	}
	firstSegment = copySegments(t, dir)

	if err := s.Checkpoint(); err != nil {
		t.Fatalf("first Checkpoint: %v", err)
	}
	checkpointAt10, err = os.ReadFile(filepath.Join(dir, checkpointName))
	if err != nil {
		t.Fatalf("reading the checkpoint that covers records 1..10: %v", err)
	}

	for i := range 9 {
		mustPut(t, s, roundKey(i), "round1-"+roundKey(i))
	}
	mustPut(t, s, gapKey, "written in the segment that goes missing")

	if err := s.Checkpoint(); err != nil {
		t.Fatalf("second Checkpoint: %v", err)
	}
	for i := range 10 {
		mustPut(t, s, roundKey(i), "round2-"+roundKey(i))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := segmentBases(t, dir); len(got) != 1 || got[0] != 20 {
		t.Fatalf("staging: segments %v after two checkpoints, want just the one based at 20", got)
	}
	return firstSegment, checkpointAt10
}

func roundKey(i int) string { return fmt.Sprintf("k%d", i) }

const gapKey = "written-only-in-the-segment-that-goes-missing"

func segmentBases(t *testing.T, dir string) []uint64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var out []uint64
	for _, e := range entries {
		if base, ok := segmentBase(e.Name()); ok {
			out = append(out, base)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// round0State is what a store holds after records 1..10 and nothing else.
func round0State(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenWith(Options{Dir: dir, CheckpointBytes: 1 << 30})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	defer func() { _ = s.Close() }()
	for i := range 10 {
		mustPut(t, s, roundKey(i), "round0-"+roundKey(i))
	}
	return s.Snapshot()
}

// B1, B3, and the premise this repository's own retraction stands on.
//
// docs/verification.md argues that re-applying superseded records is a no-op on
// data, because what a crash mid-deletion leaves is a SUFFIX of the prefix the
// checkpoint already folded. That argument has a premise: segments must abut,
// so a gap can never be closed over silently. A reviewer deleted the abut check
// in loadDir and the entire suite stayed green, 240-seed corpus included. The
// sentence used to downgrade one claim was itself resting on an unproven line.
//
// The directory this builds is the one the check exists for: a segment based at
// 0 holding records 1..10, a segment based at 20 holding records 21..30, no
// segment in between, and no checkpoint - so recovery starts from sequence 0
// and has to account for everything after it.
//
// Records 11..20 are gone. Every segment starts its own checksum chain (a
// checkpoint may have deleted the segment the first record would otherwise
// chain to), so nothing inside the segment based at 20 is detectably wrong: its
// records verify against each other and their sequence numbers follow their own
// base. The only thing that says records 11..20 ever existed is that the second
// segment does not begin where the first one ended. Without that comparison
// recovery serves a state that never existed on any timeline - the round-2
// values present, the round-1 values and gapKey silently absent.
//
// Reachability, stated rather than implied: this store does not produce this
// directory. checkpointLocked unlinks oldest first and syncs the directory
// afterwards, so what a crash leaves is a suffix. The gap needs a lost or
// hand-removed file. That is the same standing as
// TestAStoppedReplayNeverReAppliesSupersededRecords - a directory the log
// FORMAT admits rather than a crash the store can take - and it is the reason
// the check has to be tested rather than argued: an argument that a directory
// cannot arise is exactly what this check is the backstop for.
func TestRecoveryRefusesToServeRecordsPastAMissingSegment(t *testing.T) {
	dir := t.TempDir()
	firstSegment, _ := buildThreeRounds(t, dir)

	// Put the segment based at 0 back and drop the checkpoint, leaving the
	// segment based at 10 - records 11..20 - missing.
	if err := os.Remove(filepath.Join(dir, checkpointName)); err != nil {
		t.Fatalf("removing the checkpoint: %v", err)
	}
	for name, content := range firstSegment {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatalf("restoring %s: %v", name, err)
		}
	}
	if got := segmentBases(t, dir); len(got) != 2 || got[0] != 0 || got[1] != 20 {
		t.Fatalf("staging: segments %v, want [0 20] with the one based at 10 missing", got)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open over a log with a hole in it: %v - recovery is expected to stop at the hole and serve what precedes it, not to refuse", err)
	}
	defer func() { _ = reopened.Close() }()

	for i := range 10 {
		k := roundKey(i)
		want := "round0-" + k
		got, ok := reopened.Get(k)
		if !ok || !bytes.Equal(got, []byte(want)) {
			t.Errorf("key %q = %q, %v; want %q, true - recovery closed over the missing segment and served records 21..30 from beyond it, while records 11..20 are gone", k, got, ok, want)
		}
	}
	if _, ok := reopened.Get(gapKey); ok {
		t.Errorf("key %q was only ever written in the missing segment and recovery returned it", gapKey)
	}
	if want := round0State(t); !bytes.Equal(want, reopened.Snapshot()) {
		t.Error("recovering a log with a hole in it produced state that is not the state at the hole - records past a missing segment were applied")
	}
}

// B1, and the other half of the same premise.
//
// The abut check above compares one segment against the previous one, which
// leaves the first segment with nothing to compare against. That is what the
// check at the top of loadDir is for: if the oldest surviving segment begins
// after the sequence number the checkpoint covers, the records in between are
// in neither, and there is nothing left to detect it further down. A reviewer
// deleted those four lines too, and the suite stayed green.
//
// Staged directly: the checkpoint that covers records 1..10 put back over the
// current one, with only the segment based at 20 on disk. Records 11..20 are in
// no file. Recovery must refuse, because unlike the gap above there is no
// earlier point to stop at and serve - the state it would hand back has the
// round-2 writes over the round-0 checkpoint with the round-1 writes missing,
// and no reader could tell.
func TestRecoveryRefusesToOpenWhenTheLogStartsPastTheCheckpoint(t *testing.T) {
	dir := t.TempDir()
	_, checkpointAt10 := buildThreeRounds(t, dir)

	if err := os.WriteFile(filepath.Join(dir, checkpointName), checkpointAt10, 0o644); err != nil {
		t.Fatalf("restoring the checkpoint that covers records 1..10: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		return
	}
	// It opened. Say what it is serving, because "Open should have returned an
	// error" on its own does not show what the four lines are worth.
	for i := range 10 {
		k := roundKey(i)
		want := "round0-" + k
		got, ok := reopened.Get(k)
		if !ok || !bytes.Equal(got, []byte(want)) {
			t.Errorf("key %q = %q, %v - the log begins at sequence 21 and the checkpoint covers 10, so records 11..20 exist nowhere; this value comes from beyond the hole", k, got, ok)
		}
	}
	if _, ok := reopened.Get(gapKey); ok {
		t.Errorf("key %q was only ever written in records 11..20 and recovery returned it", gapKey)
	}
	_ = reopened.Close()
	t.Fatal("Open returned a store whose oldest log segment begins at sequence 21 over a checkpoint covering sequence 10 - ten acknowledged records are in neither the log nor the checkpoint and nothing said so")
}
