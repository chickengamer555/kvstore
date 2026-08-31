package kvstore

import (
	"bytes"
	"fmt"
	"os"
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
