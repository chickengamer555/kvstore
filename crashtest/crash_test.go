package crashtest_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/chickengamer555/kvstore"
	"github.com/chickengamer555/kvstore/crashtest"
)

// TestMain is what makes this binary its own child. The parent re-executes
// os.Args[0] with the seed in the environment; the copy that starts up finds
// it here, runs the schedule and never reaches a single test.
func TestMain(m *testing.M) {
	ran, err := crashtest.ChildFromEnv()
	if ran {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func selfExe(t *testing.T) crashtest.Child {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot find the test binary to re-execute: %v", err)
	}
	return crashtest.Child{Argv: []string{exe}}
}

func corpus(t *testing.T) []uint64 {
	t.Helper()
	seeds, err := crashtest.Corpus()
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	return seeds
}

// firstN is deliberately tolerant of a short corpus: the tests that use a
// subset should report their own failure, not panic on a slice bound and take
// the whole binary with them.
func firstN(seeds []uint64, n int) []uint64 {
	if len(seeds) < n {
		return seeds
	}
	return seeds[:n]
}

// B5. The corpus is at least CorpusFloor seeds, and it is still the file the
// documented generator produces - so a seed that started failing cannot have
// been quietly deleted from it.
func TestCorpusSizeFloor(t *testing.T) {
	seeds := corpus(t)
	t.Logf("corpus: %d seeds, floor %d", len(seeds), crashtest.CorpusFloor)

	if len(seeds) < crashtest.CorpusFloor {
		t.Fatalf("corpus holds %d seeds, floor is %d", len(seeds), crashtest.CorpusFloor)
	}
	want := crashtest.GenerateCorpus(len(seeds))
	for i := range seeds {
		if seeds[i] != want[i] {
			t.Fatalf("corpus.txt line %d is %d, but the generator produces %d - the file has been edited by hand", i+1, seeds[i], want[i])
		}
	}
}

// B5, and through it B1, B3 and B4. Every seed forks a child, kills it at a
// randomised offset and checks the wreckage.
func TestSeededCorpusNoAckedLoss(t *testing.T) {
	child := selfExe(t)
	seeds := corpus(t)
	t.Logf("running %d seeds on %s/%s; platform guarantees: %+v",
		len(seeds), runtime.GOOS, runtime.GOARCH, kvstore.Platform())

	// A test that iterates an empty list passes without running anything, and
	// would go on passing if the corpus file ever went missing.
	if len(seeds) < crashtest.CorpusFloor {
		t.Fatalf("corpus holds %d seeds, floor is %d - nothing below the floor is evidence", len(seeds), crashtest.CorpusFloor)
	}

	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			res, err := crashtest.RunSeed(child, seed, t.TempDir())
			if err != nil {
				t.Fatalf("harness error on seed %d: %v", seed, err)
			}
			if !res.OK() {
				// The seed is in the message on purpose: it is the whole
				// reproduce instruction.
				t.Fatalf("%s\n  reproduce: go run ./crashtest/cmd/crashrepro -seed %d\n  %v",
					res, seed, res.Failures)
			}
		})
	}
}

// B4 over the corpus specifically. RunSeed already compares two independent
// first-replays of every crashed directory, so this asserts that the check is
// actually reachable rather than repeating it: a corpus in which no seed ever
// left a torn tail behind would satisfy determinism trivially and prove
// nothing about recovery.
func TestCrashCorpusRecoveryIsDeterministic(t *testing.T) {
	child := selfExe(t)
	seeds := firstN(corpus(t), 60)

	if len(seeds) == 0 {
		t.Fatal("empty corpus")
	}
	torn := 0
	for _, seed := range seeds {
		res, err := crashtest.RunSeed(child, seed, t.TempDir())
		if err != nil {
			t.Fatalf("harness error on seed %d: %v", seed, err)
		}
		if !res.OK() {
			t.Fatalf("%s\n  %v", res, res.Failures)
		}
		if res.Report.Stopped != "end-of-log" {
			torn++
		}
	}
	t.Logf("%d of %d seeds left a damaged tail that recovery had to stop at", torn, len(seeds))
	if torn == 0 {
		t.Fatalf("not one of %d seeds left a torn tail - the kills are landing in dead time, so the determinism check never sees a damaged log", len(seeds))
	}
}

// B5. A corpus of 240 seeds means nothing if every seed kills the child at the
// same moment. This asserts the kill points are actually spread, and that
// children are genuinely being killed mid-schedule rather than finishing.
func TestKillPointsAreSpread(t *testing.T) {
	child := selfExe(t)
	seeds := firstN(corpus(t), 40)

	if len(seeds) == 0 {
		t.Fatal("empty corpus")
	}
	seen := map[int]bool{}
	lo, hi := 1<<30, 0
	for _, seed := range seeds {
		res, err := crashtest.RunSeed(child, seed, t.TempDir())
		if err != nil {
			t.Fatalf("harness error on seed %d: %v", seed, err)
		}
		if res.FinishedFree {
			t.Fatalf("seed %d: the child ran out of schedule before it could be killed; this seed proves nothing", seed)
		}
		seen[res.Acked] = true
		if res.Acked < lo {
			lo = res.Acked
		}
		if res.Acked > hi {
			hi = res.Acked
		}
	}

	t.Logf("%d distinct kill points across %d seeds, from %d to %d acknowledgements", len(seen), len(seeds), lo, hi)
	if len(seen) < len(seeds)*3/4 {
		t.Errorf("only %d distinct kill points across %d seeds - the offsets are not meaningfully randomised", len(seen), len(seeds))
	}
	if hi-lo < 100 {
		t.Errorf("kill points span only %d operations (%d..%d); a corpus clustered this tightly tests one moment repeatedly", hi-lo, lo, hi)
	}
}

// B5, the negative control, and the only test here that can tell you the rest
// of the file is worth anything.
//
// A crash corpus that has only ever run against correct code has not been
// shown to detect anything at all. So: build the same child against the
// deliberately broken write path in walpolicy_earlyack.go, which acknowledges
// writes while they are still sitting in a user-space buffer, and require that
// the harness catches it.
//
// Then reproduce. What "reproduce" means here is worth stating exactly,
// because claiming more would be a lie: the seed fixes the schedule and the
// kill point in acknowledgements, so the same seed fails the same way every
// time. It does not fix the instruction the signal lands on - that depends on
// the scheduler - so the exact set of lost keys shifts between runs. The
// category is reproducible; the last byte is not, and it is not reproducible
// for the same reason the test is worth running.
func TestSeedReproducesFailure(t *testing.T) {
	broken := buildBrokenChild(t)
	seeds := corpus(t)

	var first crashtest.Result
	found := false
	for _, seed := range firstN(seeds, 20) {
		res, err := crashtest.RunSeed(broken, seed, t.TempDir())
		if err != nil {
			t.Fatalf("harness error on seed %d: %v", seed, err)
		}
		if !res.OK() {
			first, found = res, true
			break
		}
	}
	if !found {
		t.Fatal("the deliberately broken build survived 20 seeds. Either the harness is not checking what it claims, or -tags kvearlyack no longer breaks anything - both make every green run in this file meaningless")
	}
	t.Logf("broken build failed on seed %d: %s", first.Seed, first)
	t.Logf("reproduce with: go run -tags kvearlyack ./crashtest/cmd/crashrepro -seed %d", first.Seed)

	for attempt := range 2 {
		again, err := crashtest.RunSeed(broken, first.Seed, t.TempDir())
		if err != nil {
			t.Fatalf("harness error reproducing seed %d: %v", first.Seed, err)
		}
		if again.OK() {
			t.Fatalf("seed %d failed once and then passed on re-run %d - the failure is not reproducible from its seed", first.Seed, attempt+1)
		}
		if !sameKinds(again.Kinds(), first.Kinds()) {
			t.Fatalf("seed %d failed as %v the first time and %v on re-run %d", first.Seed, first.Kinds(), again.Kinds(), attempt+1)
		}
	}

	// And the control's control: the honest build has to pass the seed that
	// killed the broken one, or the failure was never about the bug.
	honest, err := crashtest.RunSeed(selfExe(t), first.Seed, t.TempDir())
	if err != nil {
		t.Fatalf("harness error on the honest build: %v", err)
	}
	if !honest.OK() {
		t.Fatalf("the honest build also fails seed %d: %s\n  %v", first.Seed, honest, honest.Failures)
	}
}

func sameKinds(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// buildBrokenChild compiles crashrepro against the deliberately broken write
// path. It has to be a separate binary: the tag changes the store this process
// is itself linked against, so the test binary cannot be both.
func buildBrokenChild(t *testing.T) crashtest.Child {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("BLOCKED: no go toolchain on PATH, so the deliberately broken build cannot be compiled and the negative control did not run: %v", err)
	}
	out := filepath.Join(t.TempDir(), "crashchild-earlyack"+exeSuffix())
	cmd := exec.Command(goBin, "build", "-tags", "kvearlyack", "-o", out, "github.com/chickengamer555/kvstore/crashtest/cmd/crashrepro")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the deliberately broken child failed: %v\n%s", err, combined)
	}
	return crashtest.Child{Argv: []string{out}}
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
