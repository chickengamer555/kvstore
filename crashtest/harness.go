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

// Supervision bounds one seed's run. The zero value means the defaults, which
// is what the corpus uses; a test that needs to reach a bound in seconds
// rather than in minutes sets its own.
//
// The bounds are here rather than as constants because a bound nothing can
// reach is a bound nothing has checked. Reaching childTimeout for real takes a
// minute per case and a child that misbehaves on purpose.
type Supervision struct {
	// Idle is how long the harness waits without progress before it gives up
	// on a child.
	Idle time.Duration
	// Wall is the outer bound on one seed, whatever the child is doing.
	Wall time.Duration
	// Drain is how long the harness waits for the child's output pipe to close
	// after the kill.
	Drain time.Duration
}

// DefaultSupervision is what the corpus runs with.
func DefaultSupervision() Supervision {
	return Supervision{Idle: childTimeout, Wall: 3 * time.Minute, Drain: 20 * time.Second}
}

func (s Supervision) withDefaults() Supervision {
	d := DefaultSupervision()
	if s.Idle <= 0 {
		s.Idle = d.Idle
	}
	if s.Wall <= 0 {
		s.Wall = d.Wall
	}
	if s.Drain <= 0 {
		s.Drain = d.Drain
	}
	return s
}

// Ceiling is what the supervision above bounds: the wall clock on the child,
// plus the two drain waits that follow the kill. A caller with a deadline of
// its own uses it to decide whether there is room to start another seed - see
// the corpus reconciliation in crash_test.go.
//
// It is NOT the longest a seed can take, and it used to say that it was. RunSeed
// is runAndKill followed by verify, and verify has no clock on it at all: a
// copyDir of the crashed directory, two Opens that each replay the whole log,
// and a snapshot comparison, on the platform where filesystem work is fifty
// times its Linux twin. So a seed started with exactly Ceiling remaining can
// still be inside verify when the package alarm fires, and then the parent's
// t.Cleanup never runs and the reconciliation that would have named it is never
// printed. That is the one path by which a seed can still go missing without
// appearing anywhere, and it is open.
//
// The margin is small - the red run's own numbers put a seed at about eleven
// seconds against a three-minute wall bound - so this is a hole in a guarantee
// rather than a live fire. Closing it means giving verify a deadline it can be
// told about. Until then the word "guarantee" does not belong near this, and
// the caller adds its own margin instead of trusting one that is not here.
func (s Supervision) Ceiling() time.Duration {
	s = s.withDefaults()
	return s.Wall + 2*s.Drain
}

// KillPlan is the randomised kill point for a seed: how many acknowledgements
// the child gets before the signal, and the delay after the last of them.
//
// It is a pure function of the seed and it is the one the harness uses - the
// plan for any seed can therefore be computed without running it, which is
// what makes "the seed is printed on every run" worth anything.
func KillPlan(seed uint64) (afterAcks int, jitter time.Duration) {
	rng := rand.New(rand.NewPCG(seed, killPlanStream))
	return 5 + rng.IntN(DefaultMaxOps-200), time.Duration(rng.IntN(3000)) * time.Microsecond
}

// killPlanStream is the second half of the PCG seed. It is written down rather
// than derived so that changing it is a visible act: every recorded kill point
// in docs/verification.md is a function of it.
const killPlanStream = 0xA5A5A5A5A5A5A5A5

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

	// Observed is true when this seed produced a verdict: the child ran, the
	// harness read its acknowledgements to the end, and the directory it left
	// behind was checked.
	//
	// It exists because the alternative to a verdict is not a failure, it is
	// nothing at all - a seed the harness gave up on has no findings, and
	// OK() would report it as passing. The corpus counts observations, not
	// attempts, and this is the field that distinguishes them.
	Observed bool

	// CheckpointBytes is the bound this seed gave the child.
	CheckpointBytes int64

	// KilledInCheckpointWrite is true when a half-written CHECKPOINT.tmp was
	// left behind, which only happens if the signal landed between creating
	// that file and renaming it into place.
	KilledInCheckpointWrite bool

	Failures []string
}

// OK reports whether this seed passed. It is only meaningful when Observed is
// true: a seed that produced no observation has no failures either.
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

	// Supervision bounds this child. The zero value is DefaultSupervision.
	Supervision Supervision
}

// RunSeed forks the child, kills it at the offset this seed dictates, and
// checks the directory it left behind.
func RunSeed(child Child, seed uint64, dir string) (Result, error) {
	if len(child.Argv) == 0 {
		return Result{}, errors.New("crashtest: no child executable")
	}

	// Everything about this seed's run, decided before the child starts.
	afterAcks, jitter := KillPlan(seed)
	res := Result{
		Seed:            seed,
		Dir:             dir,
		KillAfterAcks:   afterAcks,
		KillJitter:      jitter,
		CheckpointBytes: CheckpointBytesFor(seed),
	}

	if err := runAndKill(child, seed, dir, &res); err != nil {
		return res, err
	}
	if err := verify(&res); err != nil {
		return res, err
	}
	res.Observed = true
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
	res.Observed = true
	return res, nil
}

func runAndKill(child Child, seed uint64, dir string, res *Result) error {
	sup := child.Supervision.withDefaults()
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
	// The instant of the most recent acknowledgement, as UnixNano. This is what
	// makes the idle bound an idle bound: it is reset by progress, so a child
	// that is slow is not confused with a child that is stuck.
	var lastAck atomic.Int64
	started := time.Now()
	lastAck.Store(started.UnixNano())

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
			lastAck.Store(time.Now().UnixNano())
			if int(acked.Add(1)) >= res.KillAfterAcks {
				once.Do(func() { close(reached) })
			}
		}
		// A read error here is the pipe closing because the child was killed,
		// which is the expected end of every run in the corpus.
	}()

	// Polled rather than timed, because two different clocks have to be
	// checked and one of them is reset by the child.
	poll := time.NewTicker(pollInterval(sup))
	defer poll.Stop()

waiting:
	for {
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
			break waiting

		case <-done:
			res.FinishedFree = true
			break waiting

		case now := <-poll.C:
			idle, elapsed := now.Sub(time.Unix(0, lastAck.Load())), now.Sub(started)
			if idle < sup.Idle && elapsed < sup.Wall {
				continue
			}
			// Give up on this seed, and say which clock ran out and what the
			// child was doing when it did. These two sentences are the entire
			// difference between "the store deadlocked" and "the runner was
			// slow", and the previous version of this line asserted the first
			// one about children that were producing output the whole time.
			n := acked.Load()
			_ = cmd.Process.Kill()
			_ = stdout.Close()
			_ = cmd.Wait()
			if idle >= sup.Idle {
				return fmt.Errorf("crashtest: seed %d produced no observation: no acknowledgement for %s (%d acknowledged of the %d this seed needs, %s since the child started)",
					res.Seed, sup.Idle.Round(time.Millisecond), n, res.KillAfterAcks, elapsed.Round(time.Millisecond))
			}
			return fmt.Errorf("crashtest: seed %d produced no observation: still short of its kill point after %s (%d acknowledged of the %d this seed needs, the last one %s ago - progressing, not stuck)",
				res.Seed, sup.Wall.Round(time.Millisecond), n, res.KillAfterAcks, idle.Round(time.Millisecond))
		}
	}

	if err := drain(done, stdout, sup.Drain, res.Seed, acked.Load()); err != nil {
		// Wait closes the parent's end of the pipe, which is the other thing
		// that can unblock an abandoned reader.
		_ = cmd.Wait()
		return err
	}
	// Wait always reports "killed" here, which is the intended outcome, so its
	// error is not a failure of the run. A child that failed for a real reason
	// printed to stderr, which is inherited.
	_ = cmd.Wait()

	res.Acked = int(acked.Load())
	return nil
}

// pollInterval is how often the two clocks are checked. Twenty times inside
// the shorter bound, and never more often than every 50ms - a test that sets a
// one-second idle bound needs a finer grain than the corpus does.
func pollInterval(sup Supervision) time.Duration {
	shortest := sup.Idle
	if sup.Wall < shortest {
		shortest = sup.Wall
	}
	if d := shortest / 20; d > 50*time.Millisecond {
		return d
	}
	return 50 * time.Millisecond
}

// drain waits for the reader goroutine to reach the end of the child's output.
//
// It is bounded, and the justification for that is a principle rather than an
// incident. The receive here used to be bare on both paths out of the select
// above, on the reasoning that a killed child's pipe closes and the reader
// returns. That reasoning has a hole with a name: the write end of a pipe
// survives the process that was given it, so any descendant holding a copy
// keeps it open, and a supervisor whose last act is an unbounded receive is not
// a supervisor. The shipped child spawns nothing, so this cannot happen to the
// corpus today; it is guarded because "today" is a property of a program that
// changes and the cost of the guard is one select.
//
// It was originally written for a different and wrong reason. Run 33374624703
// was read as a wedged pipe, and it was not one - the two readers parked in
// Scan in that dump belonged to children that were alive and acknowledging.
// docs/verification.md has the log. The bound survives the retraction of the
// story it was built for; the sentence claiming the dump showed a terminated
// child's handle stay readable does not, and is gone.
//
// Once the pipe has had to be closed by force the acknowledgement stream is
// truncated at an arbitrary point, so res.Acked is no longer the number of
// operations the child confirmed durable. Everything downstream of that number
// is then wrong in the direction that matters: verify would check a shorter
// prefix and report writes the child really did acknowledge as phantom reads.
// So this seed produces no observation, which is a thing the corpus counts,
// rather than a passing Result, which is a thing it would believe.
func drain(done <-chan struct{}, stdout io.Closer, wait time.Duration, seed uint64, acked int64) error {
	select {
	case <-done:
		return nil
	case <-time.After(wait):
	}
	_ = stdout.Close()
	select {
	case <-done:
		return fmt.Errorf("crashtest: seed %d produced no observation: the child was killed at %d acknowledgements but its output pipe did not close within %s; closing it by force unblocked the reader, so the acknowledged set is truncated at an unknown point rather than final",
			seed, acked, wait)
	case <-time.After(wait):
		return fmt.Errorf("crashtest: seed %d produced no observation: the child was killed at %d acknowledgements and its reader did not unblock within %s even after the pipe was closed by force; that goroutine is abandoned",
			seed, acked, wait)
	}
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
