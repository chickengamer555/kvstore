package kvstore

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// tracer collects the store's internal events in the order they happen. It is
// instrumentation on the real path, not a substitute for it: the fsync in the
// middle of this trace is a real fsync on a real file.
type tracer struct{ events []string }

func (tr *tracer) on(name, detail string) {
	if detail == "" {
		tr.events = append(tr.events, name)
		return
	}
	tr.events = append(tr.events, name+":"+detail)
}

func (tr *tracer) indexOf(name string) int {
	for i, e := range tr.events {
		if e == name || (len(e) > len(name) && e[:len(name)+1] == name+":") {
			return i
		}
	}
	return -1
}

func (tr *tracer) since(from int, name string) int {
	for i := from; i < len(tr.events); i++ {
		if tr.events[i] == name {
			return i
		}
	}
	return -1
}

func openTraced(t *testing.T, dir string) (*Store, *tracer) {
	t.Helper()
	tr := &tracer{}
	s, err := OpenWith(Options{Dir: dir, trace: tr.on})
	if err != nil {
		t.Fatalf("OpenWith(%q): %v", dir, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, tr
}

func segmentPaths(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "LOG.*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return paths
}

// B1. The claim is that the bytes are durable the instant Put returns, so this
// test never calls Close and never reads the store's in-memory map. It reads
// the log file off disk and replays it, which is the only thing a process that
// restarts after a kill has to work with.
//
// It does not prove the store survives a kill - a test in the same process
// cannot. crashtest/ does that with a real SIGKILL. What this proves is the
// weaker and still necessary property: at the moment Put returned, the record
// was already in the file.
func TestAckedWriteSurvivesImmediateKill(t *testing.T) {
	dir := t.TempDir()
	s, _ := openTraced(t, dir)

	if err := s.Put("alpha", []byte("one")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	paths := segmentPaths(t, dir)
	if len(paths) != 1 {
		t.Fatalf("found %d log segments in %s, want 1", len(paths), dir)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("reading the log without closing the store: %v", err)
	}

	seen := map[string][]byte{}
	_, _, _, reason := replayBytes(raw, 0, 1, func(r record) { seen[r.key] = r.value })
	if reason != stopEndOfLog {
		t.Fatalf("replaying the live log stopped for reason %q, want %q", reason, stopEndOfLog)
	}
	if got, ok := seen["alpha"]; !ok || !bytes.Equal(got, []byte("one")) {
		t.Fatalf("log contained %q for alpha (present=%v), want %q - Put returned before the record was on disk", got, ok, "one")
	}

	// And the same thing through the public door: a second store opened on the
	// same directory recovers it.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if got, ok := reopened.Get("alpha"); !ok || !bytes.Equal(got, []byte("one")) {
		t.Fatalf("after recovery Get(alpha) = %q, %v; want %q, true", got, ok, "one")
	}
}

// B1, the half that stops this suite over-claiming. A write whose record was
// torn by the crash is allowed to be gone. Not "gone but logged as an error" -
// gone, with recovery reporting success, because a torn tail is the normal
// shape of a log after a kill and treating it as corruption would make every
// crashed store refuse to open.
func TestUnackedWriteMayVanish(t *testing.T) {
	dir := t.TempDir()
	func() {
		s, _ := openTraced(t, dir)
		for _, k := range []string{"a", "b", "c"} {
			if err := s.Put(k, []byte("acked-"+k)); err != nil {
				t.Fatalf("Put(%s): %v", k, err)
			}
		}
	}()

	// Now simulate the kill: a fourth record that made it part-way into the
	// file before the process died.
	paths := segmentPaths(t, dir)
	if len(paths) != 1 {
		t.Fatalf("found %d log segments, want 1", len(paths))
	}
	var torn []byte
	torn, _ = appendRecord(torn, record{seq: 4, kind: kindPut, key: "d", value: []byte("never-acked")}, 0)
	torn = torn[:len(torn)-6]

	f, err := os.OpenFile(paths[0], os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("opening the log to tear it: %v", err)
	}
	if _, err := f.Write(torn); err != nil {
		t.Fatalf("writing the torn tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after a torn tail returned an error: %v - a torn tail is expected after a crash, not a corrupt store", err)
	}
	defer func() { _ = s.Close() }()

	for _, k := range []string{"a", "b", "c"} {
		if got, ok := s.Get(k); !ok || !bytes.Equal(got, []byte("acked-"+k)) {
			t.Errorf("acknowledged key %q = %q, %v; want %q, true", k, got, ok, "acked-"+k)
		}
	}
	if got, ok := s.Get("d"); ok {
		t.Errorf("Get(d) = %q, true; the record was torn so it must not be returned", got)
	}
	if r := s.Recovery(); r.Stopped != stopTornRecord {
		t.Errorf("recovery stopped for reason %q, want %q", r.Stopped, stopTornRecord)
	}
}

// B2. write(2) returning means the bytes are in the page cache and a power cut
// loses them. The acknowledgement therefore has to come after fsync returns,
// not after the write returns, and this asserts the actual recorded order.
func TestFsyncPrecedesAck(t *testing.T) {
	dir := t.TempDir()
	s, tr := openTraced(t, dir)

	before := s.Stats().Syncs
	if err := s.Put("k", []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	start := tr.indexOf("write-return")
	if start < 0 {
		t.Fatalf("no write-return event recorded; trace was %v", tr.events)
	}
	syncStart := tr.since(start, "sync-start")
	syncReturn := tr.since(start, "sync-return")
	ack := tr.indexOf("ack:k")

	if syncStart < 0 || syncReturn < 0 || ack < 0 {
		t.Fatalf("incomplete trace %v; want write-return, sync-start, sync-return then ack", tr.events)
	}
	if !(start < syncStart && syncStart < syncReturn && syncReturn < ack) {
		t.Fatalf("event order was %v; the acknowledgement must come after sync-return, not after write-return", tr.events)
	}
	if got := s.Stats().Syncs - before; got != 1 {
		t.Errorf("Put performed %d fsyncs, want exactly 1", got)
	}
	if !Platform().AckAfterSync {
		t.Error("Platform().AckAfterSync is false - this build acknowledges before the log is synced")
	}
}

// B2. A newly created file's directory entry is not durable until the
// containing directory is itself synced. On Linux that is an explicit
// fsync(2) on a descriptor for the directory and the application owns it. On
// Windows there is no such call, and none is needed: NTFS journals metadata
// operations, so the directory entry is made durable by the filesystem rather
// than by us.
//
// Both of those are claims about the platform, and the store has to make the
// right one. What this test asserts is that the store always reaches the
// decision point and always reports honestly which side of it this build is
// on - it must never quietly do nothing and let the README imply otherwise.
func TestDirFsyncOnLogCreate(t *testing.T) {
	dir := t.TempDir()
	tr := &tracer{}
	s, err := OpenWith(Options{Dir: dir, trace: tr.on})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	defer func() { _ = s.Close() }()

	create := tr.indexOf("segment-create")
	dirSync := tr.indexOf("dir-sync")
	if create < 0 {
		t.Fatalf("no segment-create event; trace was %v", tr.events)
	}
	if dirSync < 0 {
		t.Fatalf("no dir-sync event; trace was %v - the store must reach this decision on every platform, even where the answer is that there is nothing to do", tr.events)
	}
	if dirSync < create {
		t.Errorf("dir-sync came before segment-create; trace was %v", tr.events)
	}

	g := Platform()
	if g.Platform != runtime.GOOS {
		t.Errorf("Platform().Platform = %q, want %q", g.Platform, runtime.GOOS)
	}
	if g.DirSyncNote == "" {
		t.Error("Platform().DirSyncNote is empty - the reason this platform does or does not need a directory fsync has to be stated somewhere a caller can read it")
	}

	if g.DirSync {
		if tr.events[dirSync] != "dir-sync:ok" {
			t.Errorf("this build claims directory fsync but recorded %q", tr.events[dirSync])
		}
	} else {
		if tr.events[dirSync] != "dir-sync:unsupported" {
			t.Errorf("this build does not have directory fsync but recorded %q", tr.events[dirSync])
		}
		// BLOCKED, not passed: nothing about directory-entry durability was
		// verified on this platform. The store says why, and CI on Linux is
		// where the positive case is actually checked.
		t.Logf("BLOCKED on %s: %s", g.Platform, g.DirSyncNote)
	}

	// The build tags are the thing most likely to rot here, so pin the one
	// platform where the answer is not negotiable.
	if runtime.GOOS == "linux" && !g.DirSync {
		t.Fatal("directory fsync is reported unavailable on Linux - the build tags are wrong, and this is exactly the regression CI exists to catch")
	}
}

// Group commit, and the property that makes it honest: the whole batch costs
// one fsync, and nothing in it is acknowledged until that fsync returns.
//
// This is the number the README labels as the one that trades away durability
// granularity, so the test pins both halves - the saving, and what it costs.
func TestPutBatchIsOneSyncAndFullyDurable(t *testing.T) {
	dir := t.TempDir()
	s, tr := openTraced(t, dir)

	entries := make([]Entry, 50)
	for i := range entries {
		entries[i] = Entry{Key: fmt.Sprintf("b%02d", i), Value: []byte(fmt.Sprintf("v%02d", i))}
	}
	before := s.Stats().Syncs
	if err := s.PutBatch(entries); err != nil {
		t.Fatalf("PutBatch: %v", err)
	}
	if got := s.Stats().Syncs - before; got != 1 {
		t.Errorf("a batch of %d entries cost %d fsyncs, want exactly 1", len(entries), got)
	}
	if idx := tr.indexOf("ack-batch"); idx < 0 {
		t.Fatalf("no ack-batch event; trace was %v", tr.events)
	} else if sync := tr.since(0, "sync-return"); sync < 0 || sync > idx {
		t.Errorf("the batch was acknowledged before its fsync returned; trace was %v", tr.events)
	}

	// Durable without Close, exactly like Put.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	for _, e := range entries {
		got, ok := reopened.Get(e.Key)
		if !ok || !bytes.Equal(got, e.Value) {
			t.Fatalf("after recovery %q = %q, %v; want %q, true", e.Key, got, ok, e.Value)
		}
	}
}

// The store says it is safe for concurrent use, and until now nothing checked
// that. Written for the race detector rather than for the assertions: under
// -race this exercises the map, the counters and the log's sequence and
// checksum state from several goroutines at once, which is the only place a
// data race in this package could hide.
//
// The assertions still matter. Every Put here is separately acknowledged, so
// clause B1 applies to all of them and every key has to come back after a
// reopen - concurrency is not an excuse for losing one.
func TestConcurrentWritersAllSurvive(t *testing.T) {
	dir := t.TempDir()
	const writers = 8
	const perWriter = 25

	func() {
		s, err := OpenWith(Options{Dir: dir, CheckpointBytes: 4 << 10})
		if err != nil {
			t.Fatalf("OpenWith: %v", err)
		}
		defer func() { _ = s.Close() }()

		var wg sync.WaitGroup
		errs := make([]error, writers)
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range perWriter {
					key := fmt.Sprintf("w%d-k%02d", w, i)
					if err := s.Put(key, []byte(key+"-value")); err != nil {
						errs[w] = err
						return
					}
					// Read our own writes back while others are writing.
					if got, ok := s.Get(key); !ok || !bytes.Equal(got, []byte(key+"-value")) {
						errs[w] = fmt.Errorf("read-your-writes failed for %s: %q, %v", key, got, ok)
						return
					}
				}
			}()
		}
		wg.Wait()
		for w, err := range errs {
			if err != nil {
				t.Fatalf("writer %d: %v", w, err)
			}
		}
		if got := s.Stats().Records; got != writers*perWriter {
			t.Errorf("store counted %d records, want %d", got, writers*perWriter)
		}
	}()

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.Len() != writers*perWriter {
		t.Fatalf("recovered %d keys, want %d", reopened.Len(), writers*perWriter)
	}
	for w := range writers {
		for i := range perWriter {
			key := fmt.Sprintf("w%d-k%02d", w, i)
			if got, ok := reopened.Get(key); !ok || !bytes.Equal(got, []byte(key+"-value")) {
				t.Fatalf("acknowledged key %q = %q, %v after recovery", key, got, ok)
			}
		}
	}
}
