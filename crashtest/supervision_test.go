package crashtest_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chickengamer555/kvstore/crashtest"
)

// The harness supervises a child it cannot see inside. These tests are about
// what it does when that child does not behave - and, more than that, about
// what it SAYS.
//
// The corpus's entire claim is that it is N recorded observations of real
// kernel state. A seed the harness gives up on produces no observation, no
// finding and no failure: it produces a timeout, which reads as flake. So each
// of these asserts two things - that the harness stops rather than blocking,
// and that the sentence it stops with is true. The second matters as much as
// the first: run 33374624703 was diagnosed as a deadlock in the store for two
// hours on the strength of a message that said "produced no output" about a
// child that had produced a hundred and fifty lines of it.

// The fake children. These are not the store: they are processes that
// misbehave in one specific way each, so the harness's supervision can be
// tested without waiting for a real misbehaviour to turn up in CI.
//
// This binary is already its own child - TestMain re-executes it for the
// corpus - so it is its own fake child too, selected by an environment
// variable that only this file sets. Nothing in the shipping harness knows
// these modes exist.
const (
	envFakeChild = "KV_CRASH_FAKECHILD"

	// silent produces no output at all and never exits. The idle bound is what
	// has to stop it.
	fakeSilent = "silent"
	// chatty acknowledges steadily and forever, and never reaches any kill
	// point. Nothing about it is idle; it is simply not going to finish.
	fakeChatty = "chatty"
	// orphan acknowledges steadily, and holds the write end of its own stdout
	// open in a grandchild that outlives it. When the harness kills it the pipe
	// stays open and the reader never sees EOF - which is the shape run
	// 33374624703 wedged on, staged portably.
	fakeOrphan = "orphan"
	// sleeper is the grandchild. It does nothing but hold the handle.
	fakeSleeper = "sleeper"

	// envHolder is the copy of this binary the grandchild is run from, and
	// envHold names a file it holds the pipe open for as long as that file
	// exists. A sentinel rather than a duration, so the test releases the
	// grandchild when it is finished with it instead of guessing how long it
	// will need it - a guess that is either too short to stage the wedge or
	// too long to clean up after.
	envHolder = "KV_CRASH_HOLDER"
	envHold   = "KV_CRASH_HOLD_WHILE"
)

// runFakeChild is called from TestMain before anything else. It returns the
// exit code; it usually does not return at all, because the harness kills it.
func runFakeChild(mode string) int {
	switch mode {
	case fakeSilent:
		time.Sleep(10 * time.Minute)
		return 0

	case fakeChatty:
		ackForever()
		return 0

	case fakeOrphan:
		holder := os.Getenv(envHolder)
		if holder == "" {
			fmt.Fprintf(os.Stderr, "%s is not set\n", envHolder)
			return 1
		}
		grandchild := exec.Command(holder)
		grandchild.Env = append(os.Environ(), envFakeChild+"="+fakeSleeper)
		// The point of the whole exercise: the grandchild inherits this
		// process's stdout, which is the harness's pipe. Killing this process
		// therefore does not close it.
		grandchild.Stdout = os.Stdout
		if err := grandchild.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		ackForever()
		return 0

	case fakeSleeper:
		// Holds the inherited pipe until the test says it is done, and no
		// longer than a minute whatever happens - an orphan with no way to die
		// is a worse bug than the one being staged.
		sentinel := os.Getenv(envHold)
		if sentinel == "" {
			fmt.Fprintf(os.Stderr, "%s is not set\n", envHold)
			return 1
		}
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(sentinel); err != nil {
				return 0
			}
			time.Sleep(100 * time.Millisecond)
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown fake child mode %q\n", mode)
	return 2
}

func ackForever() {
	for i := 0; ; i++ {
		if _, err := fmt.Printf("ack %d\n", i); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func fakeChild(t *testing.T, mode string, sup crashtest.Supervision) crashtest.Child {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot find the test binary to re-execute: %v", err)
	}
	return crashtest.Child{
		Argv:        []string{exe},
		Env:         []string{envFakeChild + "=" + mode},
		Supervision: sup,
	}
}

// runSeedWithin runs one seed and fails the test if RunSeed has not returned by
// `patience`, rather than letting the package time out.
//
// The difference is the whole subject of this file. A test that hangs reports
// nothing: `go test` kills the binary, no test is named, and what a reader gets
// is a twenty-minute timeout with a goroutine dump under it. A test that fails
// at a bound reports which seed, which mode and how long - which is exactly the
// distinction the corpus reconciliation makes for real seeds.
func runSeedWithin(t *testing.T, child crashtest.Child, seed uint64, patience time.Duration) (crashtest.Result, error) {
	t.Helper()
	type outcome struct {
		res crashtest.Result
		err error
	}
	dir := t.TempDir()
	ch := make(chan outcome, 1)
	go func() {
		res, err := crashtest.RunSeed(child, seed, dir)
		ch <- outcome{res, err}
	}()
	start := time.Now()
	select {
	case o := <-ch:
		t.Logf("RunSeed returned after %s: err=%v", time.Since(start).Round(time.Millisecond), o.err)
		return o.res, o.err
	case <-time.After(patience):
		t.Fatalf("RunSeed did not return within %s. The harness is blocked on a child it has already given up on, so this seed will never produce an observation and never produce a failure either - it will produce a package timeout.", patience)
		return crashtest.Result{}, nil
	}
}

// B5. A child that says nothing is given up on, and the harness says how much
// nothing it said.
//
// The old message was "produced no output for 1m0s", which was true here and
// false everywhere else it could fire. This asserts the true half stays true
// and that the number a reader needs - how many acknowledgements arrived
// before the silence - is in the sentence rather than thrown away.
func TestAChildThatNeverAcknowledgesIsGivenUpOnAndCounted(t *testing.T) {
	t.Parallel()
	sup := crashtest.Supervision{Idle: time.Second, Wall: 30 * time.Second, Drain: 2 * time.Second}
	res, err := runSeedWithin(t, fakeChild(t, fakeSilent, sup), 1, 20*time.Second)

	if err == nil {
		t.Fatalf("RunSeed returned no error for a child that never acknowledged anything: %+v", res)
	}
	if res.Observed {
		t.Errorf("the result is marked observed, but nothing was observed: %+v", res)
	}
	for _, want := range []string{"no observation", "0 acknowledged"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not contain %q: %v", want, err)
		}
	}
}

// B5, and the false statement of fact. A child that is acknowledging steadily
// and simply will not reach its kill point is not silent, and must not be
// reported as silent.
//
// Reproduced from a review: a substitute child printing one acknowledgement
// every 400ms for the full sixty seconds - a hundred and fifty lines, the last
// of them 100ms before the deadline - came back as "produced no output for
// 1m0s". That sentence is what sent the diagnosis of run 33374624703 toward a
// deadlock in the store and then toward flake. Neither was true.
func TestAChildThatIsStillAcknowledgingIsNotReportedAsSilent(t *testing.T) {
	t.Parallel()
	// A seed whose kill point is far enough out that a child acknowledging
	// every 20ms cannot reach it inside the wall-clock bound below. The plan is
	// a pure function of the seed, so this is decided rather than hoped for.
	var seed uint64
	for s := uint64(1); s < 1000; s++ {
		if acks, _ := crashtest.KillPlan(s); acks > 300 {
			seed = s
			break
		}
	}
	if seed == 0 {
		t.Fatal("no seed under 1000 has a kill point past 300 acknowledgements; this test needs one it cannot reach")
	}
	killPoint, _ := crashtest.KillPlan(seed)

	sup := crashtest.Supervision{Idle: 2 * time.Second, Wall: 3 * time.Second, Drain: 2 * time.Second}
	res, err := runSeedWithin(t, fakeChild(t, fakeChatty, sup), seed, 25*time.Second)

	if err == nil {
		t.Fatalf("RunSeed returned no error for a child that never reached its kill point of %d: %+v", killPoint, res)
	}
	if res.Observed {
		t.Errorf("the result is marked observed, but nothing was observed: %+v", res)
	}
	msg := err.Error()

	// The child acknowledged roughly 3s/20ms = 150 times. Whatever the exact
	// number, "no output" is a false statement about it.
	for _, forbidden := range []string{"no output", "0 acknowledged"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("the message says %q about a child that acknowledged continuously for the whole window: %v", forbidden, err)
		}
	}
	if !strings.Contains(msg, "acknowledged") || !strings.Contains(msg, fmt.Sprint(killPoint)) {
		t.Errorf("the message names neither what the child did nor what it was waiting for; a reader cannot tell slow from hung: %v", err)
	}
}

// holderCopy stages the grandchild: a copy of this test binary, and the
// sentinel file it holds the inherited pipe open for. It returns both paths.
//
// The grandchild has to be run from a copy rather than from the binary itself.
// It outlives the child by design, and on Windows a running executable cannot
// be deleted, so `go test` cleaning up its own binary would fail with "Access
// is denied" at the end of an otherwise green run. That is exactly the class of
// noise this repository should not be shipping at the bottom of a green log.
func holderCopy(t *testing.T) (exePath, sentinel string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot find the test binary to copy: %v", err)
	}
	src, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("reading the test binary: %v", err)
	}
	dir, err := os.MkdirTemp("", "kvstore-holder-")
	if err != nil {
		t.Fatalf("staging the holder: %v", err)
	}
	exePath = filepath.Join(dir, filepath.Base(exe))
	if err := os.WriteFile(exePath, src, 0o755); err != nil {
		t.Fatalf("writing the holder: %v", err)
	}
	sentinel = filepath.Join(dir, "hold")
	if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
		t.Fatalf("writing the holder sentinel: %v", err)
	}
	t.Cleanup(func() {
		// Release the grandchild first, then wait for it to go: the directory
		// cannot be removed while the copy inside it is running.
		_ = os.Remove(sentinel)
		for range 100 {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("the holder copy in %s outlived its test; it exits on its own within a minute", dir)
	})
	return exePath, sentinel
}

// B5, and the liveness defect run 33374624703 actually died of.
//
// childTimeout used to guard only the select before the kill. Both paths out of
// it then blocked on a bare `<-done`, so if the reader goroutine never saw EOF
// on the killed child's stdout the harness sat there until the package timeout
// twenty minutes later - no observation, no finding, no failing seed, and a red
// CI run whose only content is a goroutine dump.
//
// The runner's version of "the pipe did not close" is still unexplained: the
// child was a compiled binary with no grandchild, and an overlapped ReadFile
// stayed parked on a handle whose only writer had been terminated. This test
// does not reproduce that cause. It reproduces the observable - a write end
// that is still open after the child is dead - by having the child hand its
// stdout to a grandchild that outlives it, which is portable and is the same
// thing from the harness's side.
//
// What is asserted is only what the harness can promise: it stops, and it says
// the seed produced no observation rather than silently returning a Result
// built from an acknowledgement stream it could not finish reading.
func TestAPipeThatDoesNotCloseAfterTheKillIsBoundedRatherThanBlocking(t *testing.T) {
	t.Parallel()
	// A seed with a kill point close enough to the start that the child is dead
	// while the grandchild is still holding the pipe. The whole staging depends
	// on that order, so it is decided from the plan rather than hoped for.
	var seed uint64
	for s := uint64(1); s < 1000; s++ {
		if acks, _ := crashtest.KillPlan(s); acks < 25 {
			seed = s
			break
		}
	}
	if seed == 0 {
		t.Fatal("no seed under 1000 has a kill point inside 25 acknowledgements; this test needs the kill to land early")
	}
	killPoint, _ := crashtest.KillPlan(seed)

	sup := crashtest.Supervision{Idle: 10 * time.Second, Wall: 30 * time.Second, Drain: time.Second}
	child := fakeChild(t, fakeOrphan, sup)
	holder, sentinel := holderCopy(t)
	child.Env = append(child.Env, envHolder+"="+holder, envHold+"="+sentinel)
	t.Logf("seed %d is killed after %d acknowledgements, about %s in", seed, killPoint, time.Duration(killPoint)*20*time.Millisecond)
	res, err := runSeedWithin(t, child, seed, 25*time.Second)

	if err == nil {
		t.Fatalf("RunSeed returned normally although the acknowledgement stream was never finished: %+v", res)
	}
	if res.Observed {
		t.Errorf("the result is marked observed, but the acknowledged set was never final: %+v", res)
	}
	if !strings.Contains(err.Error(), "no observation") {
		t.Errorf("the message does not say the seed produced no observation: %v", err)
	}
}

// The plan for a seed is decided before the child starts and derived from
// nothing but the seed, which is what makes a failing seed reproducible. This
// asserts KillPlan is that function rather than a second copy of it, by
// checking it against what a real run actually did.
func TestTheKillPlanIsAFunctionOfTheSeedAlone(t *testing.T) {
	t.Parallel()
	seeds := firstN(corpus(t), 3)
	child := selfExe(t)
	for _, seed := range seeds {
		wantAcks, wantJitter := crashtest.KillPlan(seed)
		if again, _ := crashtest.KillPlan(seed); again != wantAcks {
			t.Fatalf("KillPlan(%d) is not deterministic: %d then %d", seed, wantAcks, again)
		}
		res, err := crashtest.RunSeed(child, seed, t.TempDir())
		if err != nil {
			t.Fatalf("seed %d produced no observation: %v", seed, err)
		}
		if res.KillAfterAcks != wantAcks || res.KillJitter != wantJitter {
			t.Errorf("seed %d ran with kill point %d/%s but KillPlan says %d/%s - the plan a reader can compute is not the plan the harness used",
				seed, res.KillAfterAcks, res.KillJitter, wantAcks, wantJitter)
		}
	}
}
