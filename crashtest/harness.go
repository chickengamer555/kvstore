// Package crashtest kills a real process at a randomised point and checks what
// the store looks like afterwards.
//
// The shape is: a parent forks a child, the child writes a schedule derived
// entirely from a seed and prints one line per acknowledged write, the parent
// kills it at an offset also derived from that seed, and then the parent
// reopens the directory and checks three things.
//
//	B1  every acknowledged write is there, and the one operation that was
//	    in flight when the process died may have happened or not
//	B3  no value ever comes back that the schedule did not write
//	B4  two independent first-replays of the crashed directory produce
//	    byte-identical state
//
// What this proves and what it does not, stated plainly because the difference
// is the whole point of the exercise:
//
// It proves the store survives its process dying at an arbitrary instruction -
// mid-write, mid-fsync, mid-rename, mid-delete. That is a real and useful
// property and it is where torn records, half-finished checkpoints and
// unreachable segments come from.
//
// It does NOT prove the fsync is doing its job. After Process.Kill the page
// cache is untouched and the kernel writes out unsynced data anyway, so a store
// that never called fsync at all would sail through this corpus. Only losing
// power catches that, and nothing in user space can arrange a real one. That
// half is proven by the simulated-disk tests in the root package, which
// replace the platter rather than the process: writes are only durable once
// Sync has promoted them, the power is taken away in-process, and the store is
// reopened on what is left. The two are complementary and neither covers the
// other - a process kill reaches code paths no simulator models, and a
// simulated power cut reaches losses no process kill can produce.
package crashtest

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chickengamer555/kvstore"
)

// Environment variables the parent sets on the child. A process that finds
// EnvSeed set is a child and must run the schedule instead of whatever it
// would normally do.
const (
	EnvSeed            = "KV_CRASH_SEED"
	EnvDir             = "KV_CRASH_DIR"
	EnvMaxOps          = "KV_CRASH_MAXOPS"
	EnvCheckpointBytes = "KV_CRASH_CHECKPOINT_BYTES"
)

const (
	// DefaultMaxOps caps the schedule so a child that is never killed still
	// terminates. Every seed in the corpus is killed long before this.
	DefaultMaxOps = 600

	childTimeout = 60 * time.Second
)

// CheckpointBytesFor picks the child's checkpoint bound from its seed, between
// 256 bytes and 16KB.
//
// This is here because of a measurement, not a guess. With a single fixed
// bound of 2KB, 240 seeds produced exactly one child killed inside a
// checkpoint and none killed between a checkpoint and the deletion of the
// segments it replaced - which are precisely the windows the checkpoint code
// exists to be safe in. The reason is that the store spends almost all of its
// wall time blocked in fsync on the ordinary write path, so that is where a
// randomly timed signal lands.
//
// Varying the bound with the seed means a good share of the corpus runs a
// child that spends most of its life checkpointing, without any crash point
// being placed by hand. The offset is still random; what changes is how much
// of the child's time is spent in the code worth interrupting.
func CheckpointBytesFor(seed uint64) int64 {
	return 256 << (seed % 7)
}

// Op is one operation in a seeded schedule.
type Op struct {
	Key    string
	Value  []byte
	Delete bool
}

// OpAt returns operation i of the schedule for seed.
//
// It is a pure function of (seed, i) and holds no state, which is what lets the
// parent work out what the child must have written without simulating it - and
// without trusting anything the child said beyond how far it got.
func OpAt(seed uint64, i int) Op {
	var in [16]byte
	binary.LittleEndian.PutUint64(in[0:], seed)
	binary.LittleEndian.PutUint64(in[8:], uint64(i))
	h := sha256.Sum256(in[:])

	key := fmt.Sprintf("k%04d", binary.LittleEndian.Uint16(h[0:])%600)
	// Deletes, but not in the first few operations - a schedule that opens by
	// deleting from an empty store tests very little.
	if i > 20 && h[2]%20 < 3 {
		return Op{Key: key, Delete: true}
	}
	n := 8 + int(h[3])%192
	val := make([]byte, n)
	for j := range val {
		val[j] = h[4+(j%28)] ^ byte(j)
	}
	return Op{Key: key, Value: val}
}

// RunChild is the child half. It writes the schedule for seed into dir,
// printing one acknowledgement line to ack after every operation the store has
// confirmed durable.
//
// The line goes straight to the file descriptor with no buffering in front of
// it. It is the parent's only record of what was acknowledged, and it has to
// survive the process being killed microseconds later; a bufio.Writer here
// would lose exactly the lines that matter most and would make the store look
// better than it is.
func RunChild(dir string, seed uint64, maxOps int, checkpointBytes int64, ack io.Writer) error {
	s, err := kvstore.OpenWith(kvstore.Options{Dir: dir, CheckpointBytes: checkpointBytes})
	if err != nil {
		return fmt.Errorf("child: opening store: %w", err)
	}
	for i := range maxOps {
		op := OpAt(seed, i)
		if op.Delete {
			err = s.Delete(op.Key)
		} else {
			err = s.Put(op.Key, op.Value)
		}
		if err != nil {
			return fmt.Errorf("child: op %d: %w", i, err)
		}
		if _, err := fmt.Fprintf(ack, "ack %d\n", i); err != nil {
			return fmt.Errorf("child: reporting ack %d: %w", i, err)
		}
	}
	return s.Close()
}

// ChildFromEnv runs the child workload if this process was started as one.
//
// Both entry points call it first: the test binary re-executes itself
// (os.Args[0]) for the corpus, and cmd/crashrepro re-executes itself for the
// reproduce command and for the deliberately broken build.
func ChildFromEnv() (bool, error) {
	raw := os.Getenv(EnvSeed)
	if raw == "" {
		return false, nil
	}
	var seed uint64
	if _, err := fmt.Sscan(raw, &seed); err != nil {
		return true, fmt.Errorf("child: bad %s=%q: %w", EnvSeed, raw, err)
	}
	dir := os.Getenv(EnvDir)
	if dir == "" {
		return true, fmt.Errorf("child: %s is not set", EnvDir)
	}
	maxOps := DefaultMaxOps
	if raw := os.Getenv(EnvMaxOps); raw != "" {
		if _, err := fmt.Sscan(raw, &maxOps); err != nil {
			return true, fmt.Errorf("child: bad %s=%q: %w", EnvMaxOps, raw, err)
		}
	}
	checkpointBytes := CheckpointBytesFor(seed)
	if raw := os.Getenv(EnvCheckpointBytes); raw != "" {
		if _, err := fmt.Sscan(raw, &checkpointBytes); err != nil {
			return true, fmt.Errorf("child: bad %s=%q: %w", EnvCheckpointBytes, raw, err)
		}
	}
	return true, RunChild(dir, seed, maxOps, checkpointBytes, os.Stdout)
}

// Result is what one seed produced.
type Result struct {
	Seed uint64
	Dir  string

	// KillAfterAcks and KillJitter are the randomised offset, both derived
	// from the seed. The first is exact and reproducible; the second is a
	// delay, so where inside the following operation the signal actually lands
	// is not - see Reproducibility in the README.
	KillAfterAcks int
	KillJitter    time.Duration

	Acked        int
	FinishedFree bool // the child ran out of schedule before it could be killed
	Report       kvstore.RecoveryReport

	// CheckpointBytes is the bound this seed gave the child.
	CheckpointBytes int64

	// KilledInCheckpointWrite is true when a half-written CHECKPOINT.tmp was
	// left behind, which only happens if the signal landed between creating
	// that file and renaming it into place.
	KilledInCheckpointWrite bool

	Failures []string
}

// OK reports whether this seed passed.
func (r Result) OK() bool { return len(r.Failures) == 0 }

// Kinds returns the sorted, deduplicated failure categories - what a
// reproduction has to match.
//
// The distinction this exists for is between re-running a seed and replaying a
// preserved directory. Re-running a seed produces a different set of keys,
// because the instant the signal lands is the scheduler's business and not the
// harness's, so only the category is stable across re-runs. Replaying fixed
// bytes is a different matter entirely: nothing there varies, and the findings
// themselves - the strings, in order - must be identical. An earlier version of
// this comment blamed the scheduler for variation in bytes that cannot vary,
// which excused a genuine defect in verify(); TestReplayedFindingsAreInAStableOrder
// is what settles it now.
func (r Result) Kinds() []string {
	seen := map[string]bool{}
	for _, f := range r.Failures {
		seen[strings.SplitN(f, ":", 2)[0]] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (r Result) String() string {
	status := "pass"
	if !r.OK() {
		status = "FAIL " + strings.Join(r.Kinds(), ",")
	}
	return fmt.Sprintf("seed=%d killAfterAcks=%d jitter=%s acked=%d stopped=%s %s",
		r.Seed, r.KillAfterAcks, r.KillJitter, r.Acked, r.Report.Stopped, status)
}

// Child is the process to fork. Argv[0] is the executable; the corpus uses the
// test binary itself, and the negative control uses a separately built one.
type Child struct {
	Argv []string
	Env  []string
}

// RunSeed forks the child, kills it at the offset this seed dictates, and
// checks the directory it left behind.
func RunSeed(child Child, seed uint64, dir string) (Result, error) {
	if len(child.Argv) == 0 {
		return Result{}, errors.New("crashtest: no child executable")
	}

	// Everything about this seed's run, decided before the child starts.
	rng := rand.New(rand.NewPCG(seed, 0xA5A5A5A5A5A5A5A5))
	res := Result{
		Seed:            seed,
		Dir:             dir,
		KillAfterAcks:   120, // DELIBERATELY FIXED for this commit: every seed kills at the same point.
		KillJitter:      time.Duration(rng.IntN(3000)) * time.Microsecond,
		CheckpointBytes: CheckpointBytesFor(seed),
	}

	if err := runAndKill(child, seed, dir, &res); err != nil {
		return res, err
	}
	if err := verify(&res); err != nil {
		return res, err
	}
	return res, nil
}

// CaseFile is the small manifest written beside a preserved crash, holding the
// two numbers needed to check those bytes again: which schedule produced them
// and how far it got.
const CaseFile = "crashcase.txt"

// Preserve copies the crashed directory somewhere it will survive, together
// with the manifest ReplayCase needs.
//
// This is the half of reproduction that is exact. Re-running a seed reproduces
// the schedule and the intended kill point but not the instruction the signal
// lands on, so it reproduces a failure most of the time and not always.
// Replaying these bytes reproduces the identical failure every time, for as
// long as the directory exists - which is what you actually want at the point
// where you are trying to fix something.
func (r Result) Preserve(root string) (string, error) {
	caseDir := filepath.Join(root, fmt.Sprintf("seed-%d", r.Seed))
	if err := copyDir(r.Dir, filepath.Join(caseDir, "store")); err != nil {
		return "", err
	}
	manifest := fmt.Sprintf(`seed %d
acked %d
killAfterAcks %d
checkpointBytes %d
`,
		r.Seed, r.Acked, r.KillAfterAcks, r.CheckpointBytes)
	if err := os.WriteFile(filepath.Join(caseDir, CaseFile), []byte(manifest), 0o644); err != nil {
		return "", err
	}
	return caseDir, nil
}

// ReplayCase re-runs the checks over a directory preserved by Preserve.
//
// It works on a copy, because opening a store repairs the directory it opens -
// truncating a torn tail, dropping unreachable segments - and a crash case you
// can only examine once is not much of a crash case.
func ReplayCase(caseDir string) (Result, error) {
	manifest, err := os.ReadFile(filepath.Join(caseDir, CaseFile))
	if err != nil {
		return Result{}, fmt.Errorf("crashtest: reading the case manifest: %w", err)
	}
	res := Result{}
	sc := bufio.NewScanner(bytes.NewReader(manifest))
	for sc.Scan() {
		line := sc.Text()
		var name string
		var value uint64
		if _, err := fmt.Sscan(line, &name, &value); err != nil {
			continue
		}
		switch name {
		case "seed":
			res.Seed = value
		case "acked":
			res.Acked = int(value)
		case "killAfterAcks":
			res.KillAfterAcks = int(value)
		case "checkpointBytes":
			res.CheckpointBytes = int64(value)
		}
	}

	scratch, err := os.MkdirTemp("", "kvstore-replay-")
	if err != nil {
		return res, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	defer func() { _ = os.RemoveAll(scratch + ".twin") }()

	res.Dir = filepath.Join(scratch, "store")
	if err := copyDir(filepath.Join(caseDir, "store"), res.Dir); err != nil {
		return res, err
	}
	if err := verify(&res); err != nil {
		return res, err
	}
	return res, nil
}

func runAndKill(child Child, seed uint64, dir string, res *Result) error {
	cmd := exec.Command(child.Argv[0], child.Argv[1:]...)
	cmd.Env = append(os.Environ(),
		EnvSeed+"="+fmt.Sprint(seed),
		EnvDir+"="+dir,
		EnvMaxOps+"="+fmt.Sprint(DefaultMaxOps),
		EnvCheckpointBytes+"="+fmt.Sprint(CheckpointBytesFor(seed)),
	)
	cmd.Env = append(cmd.Env, child.Env...)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("crashtest: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("crashtest: starting child: %w", err)
	}

	var acked atomic.Int64
	reached := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		var once sync.Once
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if !strings.HasPrefix(sc.Text(), "ack ") {
				continue
			}
			if int(acked.Add(1)) >= res.KillAfterAcks {
				once.Do(func() { close(reached) })
			}
		}
		// A read error here is the pipe closing because the child was killed,
		// which is the expected end of every run in the corpus.
	}()

	select {
	case <-reached:
		// The randomised part. The signal lands somewhere inside the next
		// operation or two - possibly between the write and the fsync,
		// possibly inside a checkpoint's rename, possibly between two of the
		// segment deletions. That is the point: those are the offsets nobody
		// thinks to place a crash at by hand.
		time.Sleep(res.KillJitter)
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("crashtest: killing child: %w", err)
		}
	case <-done:
		res.FinishedFree = true
	case <-time.After(childTimeout):
		_ = cmd.Process.Kill()
		<-done
		_ = cmd.Wait()
		return fmt.Errorf("crashtest: child for seed %d produced no output for %s", res.Seed, childTimeout)
	}

	<-done
	// Wait always reports "killed" here, which is the intended outcome, so its
	// error is not a failure of the run. A child that failed for a real reason
	// printed to stderr, which is inherited.
	_ = cmd.Wait()

	res.Acked = int(acked.Load())
	return nil
}

// verify reopens the crashed directory and checks B1, B3 and B4.
func verify(res *Result) error {
	// The acknowledged prefix, folded exactly as the store should have.
	want := map[string][]byte{}
	for i := range res.Acked {
		op := OpAt(res.Seed, i)
		if op.Delete {
			delete(want, op.Key)
			continue
		}
		want[op.Key] = op.Value
	}

	// Exactly one operation can have been in flight when the process died,
	// because the child does them one at a time and waits for each to be
	// acknowledged. That one key is allowed to be in either state; every other
	// key is not.
	inflight := OpAt(res.Seed, res.Acked)

	// B4 first, and over two independent first-replays rather than a replay
	// and a re-replay: the first Open repairs the directory (truncating a torn
	// tail, dropping unreachable segments), so opening twice in a row would
	// compare a crashed directory against a repaired one.
	if _, err := os.Stat(filepath.Join(res.Dir, "CHECKPOINT.tmp")); err == nil {
		res.KilledInCheckpointWrite = true
	}

	twin := res.Dir + ".twin"
	if err := copyDir(res.Dir, twin); err != nil {
		return fmt.Errorf("crashtest: copying the crashed directory: %w", err)
	}

	s, err := kvstore.Open(res.Dir)
	if err != nil {
		res.Failures = append(res.Failures, "recovery-error: "+err.Error())
		return nil
	}
	defer func() { _ = s.Close() }()
	res.Report = s.Recovery()

	twinStore, err := kvstore.Open(twin)
	if err != nil {
		res.Failures = append(res.Failures, "recovery-error-on-twin: "+err.Error())
		return nil
	}
	defer func() { _ = twinStore.Close() }()

	if !bytes.Equal(s.Snapshot(), twinStore.Snapshot()) {
		res.Failures = append(res.Failures, "nondeterministic-recovery: two replays of the same crashed directory produced different state")
	}

	// B1: every acknowledged write is present and correct.
	//
	// Sorted, and this is not tidiness. Ranging over `want` directly put the
	// findings in Go's randomised map order, so replaying one preserved crash
	// directory reported the same failures in a different sequence every time -
	// which makes a failure impossible to diff against a fix, and was the same
	// mistake the recovered snapshot had already been sorted to avoid.
	wantKeys := make([]string, 0, len(want))
	for key := range want {
		wantKeys = append(wantKeys, key)
	}
	sort.Strings(wantKeys)

	for _, key := range wantKeys {
		value := want[key]
		got, ok := s.Get(key)
		if key == inflight.Key {
			if !ok && !inflight.Delete {
				res.Failures = append(res.Failures, fmt.Sprintf("acked-write-lost: %s (in-flight key, absent)", key))
			} else if ok && !bytes.Equal(got, value) && !bytes.Equal(got, inflight.Value) {
				res.Failures = append(res.Failures, fmt.Sprintf("corrupt-read: %s holds neither the acknowledged value nor the in-flight one", key))
			}
			continue
		}
		if !ok {
			res.Failures = append(res.Failures, fmt.Sprintf("acked-write-lost: %s", key))
			continue
		}
		if !bytes.Equal(got, value) {
			res.Failures = append(res.Failures, fmt.Sprintf("corrupt-read: %s holds %d bytes that are not the acknowledged value", key, len(got)))
		}
	}

	// B3, the other direction: nothing may come back that was never written.
	// Only the in-flight operation can add a key the acknowledged prefix does
	// not have.
	for i := range 600 {
		key := fmt.Sprintf("k%04d", i)
		if _, expected := want[key]; expected {
			continue
		}
		got, ok := s.Get(key)
		if !ok {
			continue
		}
		if key == inflight.Key && !inflight.Delete && bytes.Equal(got, inflight.Value) {
			continue
		}
		res.Failures = append(res.Failures, fmt.Sprintf("phantom-read: %s was never acknowledged but holds %d bytes", key, len(got)))
	}
	return nil
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}
