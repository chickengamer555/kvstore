package kvstore

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// What the checksum chain does NOT do, pinned so that it cannot quietly
// change and so that the README cannot quietly overstate it again.
//
// Within a segment the chain is real: each record's crc32c is seeded with the
// previous record's, so a record that is internally perfect but followed a
// different predecessor fails, and TestRecordFromAnotherChainIsRejected
// catches exactly that. Across a segment boundary it is not. loadDir starts
// each segment at replayBytes(buf, 0, base+1, ...) - chain seeded with zero -
// because a checkpoint may have deleted the segment whose last record the
// first record would otherwise chain to. That reset is deliberate and the
// comment in recover.go says why.
//
// The consequence is this test. A whole segment taken from a different store
// and dropped in at a matching base is accepted: the sequence numbers abut, so
// the abutment check is satisfied, and the chain inside the file is internally
// consistent because it is a real log, just not this one's. Recovery reports
// end-of-log and serves the other store's values.
//
// That is a statement about the threat model, not the crash model. crc32c is a
// checksum, not a MAC: it detects damage, and it cannot detect substitution by
// anyone who can write to the directory - within a segment or across a
// boundary. No crash produces this state. Anything that can produce it can
// also replace the whole directory, and no per-record scheme fixes that.
//
// The smallest thing that WOULD close the boundary is a per-store identity
// written once at Open and used to seed each segment's chain in place of zero,
// so a segment from another store fails at its first record without needing
// its predecessor to still exist. That is not implemented, and this test is
// what would have to be rewritten first if it ever is.
func TestASegmentLiftedFromAnotherStoreAtAMatchingBoundaryIsAccepted(t *testing.T) {
	build := func(dir, mark string) uint64 {
		t.Helper()
		s, err := OpenWith(Options{Dir: dir, CheckpointBytes: 4 << 10})
		if err != nil {
			t.Fatalf("OpenWith(%s): %v", dir, err)
		}
		defer func() { _ = s.Close() }()
		// Identical shapes on both sides, so the checkpoint lands at the same
		// sequence number and the two stores end up with a segment of the same
		// name. Only the values differ, and by the same number of bytes.
		for i := range 400 {
			if err := s.Put(fmt.Sprintf("k%03d", i), []byte(mark+fmt.Sprintf("%03d", i))); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		if s.Stats().Checkpoints == 0 {
			t.Fatal("no checkpoint was taken, so there is no segment boundary to lift a segment across")
		}
		return s.log.base
	}

	dirA, dirB := t.TempDir(), t.TempDir()
	baseA := build(dirA, "A")
	baseB := build(dirB, "B")
	if baseA != baseB {
		t.Fatalf("the two stores checkpointed at different sequence numbers (%d and %d), so this test is not lifting a segment across a matching boundary", baseA, baseB)
	}

	seg := segmentName(baseA)
	lifted, err := os.ReadFile(filepath.Join(dirB, seg))
	if err != nil {
		t.Fatalf("reading %s from the other store: %v", seg, err)
	}
	if err := os.WriteFile(filepath.Join(dirA, seg), lifted, 0o644); err != nil {
		t.Fatalf("planting the other store's segment: %v", err)
	}

	reopened, err := OpenWith(Options{Dir: dirA, CheckpointBytes: 4 << 10})
	if err != nil {
		t.Fatalf("reopening the store with a foreign segment: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	r := reopened.Recovery()
	if r.Stopped != stopEndOfLog {
		t.Fatalf("recovery stopped for reason %q - if the chain now spans the segment boundary, this test is the thing to rewrite, not the code", r.Stopped)
	}

	// The last key in the lifted segment is the one that shows whose data came
	// back. Both stores wrote it; only one of them wrote it here.
	last := fmt.Sprintf("k%03d", 399)
	got, ok := reopened.Get(last)
	if !ok {
		t.Fatalf("%s is missing after the lift, which is neither the documented behaviour nor a fix", last)
	}
	if got[0] != 'B' {
		t.Fatalf("Get(%s) came back marked %q - this test exists to record that it comes back marked B, from the other store", last, got[0])
	}
}
