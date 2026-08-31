package crashtest_test

import (
	"fmt"
	"os"
	"os/exec"
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
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		grandchild := exec.Command(exe)
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
		// Bounded, so a wedged test does not leave a process behind for long.
		// It must outlive the harness's drain bound, which is what makes the
		// pipe stay open past the kill.
		time.Sleep(30 * time.Second)
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
	sup := crashtest.Supervision{Idle: 10 * time.Second, Wall: 30 * time.Second, Drain: time.Second}
	res, err := runSeedWithin(t, fakeChild(t, fakeOrphan, sup), 2, 25*time.Second)

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
