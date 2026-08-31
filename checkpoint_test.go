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
