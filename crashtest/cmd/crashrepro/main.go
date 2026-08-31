// Command crashrepro runs one seed of the crash corpus and says what happened.
//
// It is the documented way to reproduce a failure. When a corpus seed fails,
// the test prints the seed, and this is the command that takes it:
//
//	go run ./crashtest/cmd/crashrepro -seed 7960286522194355700
//
// To watch the harness catch a store that really does lose data, build the
// same command against the deliberately broken write path and give it any
// seed at all:
//
//	go run -tags kvearlyack ./crashtest/cmd/crashrepro -seed 7960286522194355700
//
// The program is both halves of the exercise. Run normally it is the parent;
// re-executed by the parent with KV_CRASH_SEED set in its environment it is the
// child, which is what makes the broken build reachable without a second
// binary.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/chickengamer555/kvstore"
	"github.com/chickengamer555/kvstore/crashtest"
)

func main() {
	// Child first: a process with the seed in its environment must run the
	// schedule and nothing else.
	if ran, err := crashtest.ChildFromEnv(); ran {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	seed := flag.Uint64("seed", 0, "re-run this seed: same schedule, same intended kill point")
	replay := flag.String("replay", "", "re-check a crash directory preserved by a failing run: same bytes, same verdict, every time")
	keep := flag.String("dir", "", "keep the crashed store in this directory instead of a temporary one")
	writeCorpus := flag.String("write-corpus", "", "regenerate the seed corpus into this file and exit")
	failures := flag.String("failure-dir", "crash-failures", "where to preserve the crashed directory if the seed fails")
	shapes := flag.Bool("corpus-shapes", false, "run the whole corpus and print where the kills actually landed")
	workers := flag.Int("workers", runtime.NumCPU(), "children to run at once with -corpus-shapes")
	flag.Parse()

	if *shapes {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := corpusShapes(crashtest.Child{Argv: []string{exe}}, *workers); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if *writeCorpus != "" {
		if err := regenerate(*writeCorpus); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if *replay != "" {
		res, err := crashtest.ReplayCase(*replay)
		if err != nil {
			fmt.Fprintln(os.Stderr, "replay error:", err)
			os.Exit(2)
		}
		report(res, *replay)
		if !res.OK() {
			os.Exit(1)
		}
		return
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot find my own executable:", err)
		os.Exit(1)
	}

	dir := *keep
	if dir == "" {
		dir, err = os.MkdirTemp("", "kvstore-crash-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer func() { _ = os.RemoveAll(dir) }()
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	g := kvstore.Platform()
	fmt.Printf("platform      %s  ack-after-sync=%v  dir-fsync=%v\n", g.Platform, g.AckAfterSync, g.DirSync)
	fmt.Printf("              %s\n", g.DirSyncNote)

	res, err := crashtest.RunSeed(crashtest.Child{Argv: []string{exe}}, *seed, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness error:", err)
		os.Exit(2)
	}

	report(res, dir)
	if res.OK() {
		return
	}

	// The run that just failed holds the only copy of these bytes, and the
	// temporary directory is about to be deleted. Keep it: re-running this seed
	// reproduces the failure about half the time, and replaying the wreckage
	// reproduces it every time.
	caseDir, perr := res.Preserve(*failures)
	if perr != nil {
		fmt.Fprintln(os.Stderr, "could not preserve the failing case:", perr)
		os.Exit(1)
	}
	fmt.Printf("preserved     %s\n", caseDir)
	fmt.Printf("replay it     go run ./crashtest/cmd/crashrepro -replay %s\n", caseDir)
	os.Exit(1)
}

func report(res crashtest.Result, dir string) {
	fmt.Printf("seed          %d\n", res.Seed)
	fmt.Printf("kill point    after %d acknowledgements, plus %s\n", res.KillAfterAcks, res.KillJitter)
	fmt.Printf("acknowledged  %d operations before the process died\n", res.Acked)
	fmt.Printf("recovery      %+v\n", res.Report)
	fmt.Printf("store dir     %s\n", dir)

	if res.OK() {
		fmt.Println("verdict       pass - every acknowledged write survived, nothing phantom, recovery deterministic")
		return
	}
	fmt.Printf("verdict       FAIL (%d finding(s))\n", len(res.Failures))
	for _, f := range res.Failures {
		fmt.Println("             ", f)
	}
}

func regenerate(path string) error {
	var buf []byte
	buf = append(buf, "# Crash corpus: seeds for crashtest.RunSeed.\n#\n"...)
	buf = append(buf, "# Not hand-picked. This file is splitmix64 seeded with crashtest.CorpusOrigin\n"...)
	buf = append(buf, "# (0x9E3779B97F4A7C15), and TestCorpusSizeFloor checks that it still is - so a\n"...)
	buf = append(buf, "# seed that started failing cannot be quietly removed from it.\n#\n"...)
	buf = append(buf, "# Regenerate:  go run ./crashtest/cmd/crashrepro -write-corpus crashtest/corpus.txt\n"...)
	for _, s := range crashtest.GenerateCorpus(240) {
		buf = append(buf, fmt.Sprintf("%d\n", s)...)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o644)
}

// corpusShapes runs every seed in the recorded corpus and counts where the
// kills actually landed.
//
// This exists because the README quotes that distribution, and a number in a
// README that a reader cannot regenerate is worth less than no number. It is
// also the honest measure of how much the corpus covers: "240 seeds" says how
// many times the harness ran, not how many interesting places it interrupted.
func corpusShapes(child crashtest.Child, workers int) error {
	seeds, err := crashtest.Corpus()
	if err != nil {
		return err
	}
	if workers < 1 {
		workers = 1
	}

	var mu sync.Mutex
	// Every shape starts at zero, so a shape that never happens still prints a
	// row. That matters more than it sounds: the rows this table is quoted for
	// are the zeros, and a tally built from observed keys alone cannot produce
	// one - the reader would have to infer it from a missing line, which is
	// indistinguishable from the classifier having stopped firing.
	counts := crashtest.NewShapeCounts()
	// And the same argument one level up. The shape rows say what the seeds
	// that produced an observation landed on; this says how many seeds produced
	// one, which is the denominator the whole table is read against.
	var tally crashtest.Tally
	checkpointed, failed := 0, 0

	work := make(chan uint64)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seed := range work {
				dir, err := os.MkdirTemp("", "kvshapes-")
				if err != nil {
					// Dropping this used to lose the seed silently, which is
					// the same defect the tally exists to close, in the one
					// place that knows the reason.
					tally.NoObservation(seed, "no working directory: "+err.Error())
					continue
				}
				res, runErr := crashtest.RunSeed(child, seed, dir)
				if runErr != nil {
					tally.NoObservation(seed, runErr.Error())
					_ = os.RemoveAll(dir)
					continue
				}
				tally.Observe(seed)
				mu.Lock()
				counts.Add(res)
				if res.Report.UsedCheckpoint {
					checkpointed++
				}
				if !res.OK() {
					failed++
				}
				mu.Unlock()
				_ = os.RemoveAll(dir)
				_ = os.RemoveAll(dir + ".twin")
			}
		}()
	}
	for _, s := range seeds {
		work <- s
	}
	close(work)
	wg.Wait()

	g := kvstore.Platform()
	reconciliation, reconciles := tally.Reconcile(len(seeds))

	// The header says how many seeds were OBSERVED, not how many are in the
	// corpus. Those have been the same number every time this has been run, and
	// the header said so on the strength of that assumption rather than of the
	// count - which is the same defect as the zero rows below it, one level up.
	fmt.Printf("| | %d seeds observed of %d, %s/%s |\n|---|---|\n",
		tally.Observations(), len(seeds), runtime.GOOS, runtime.GOARCH)
	for _, row := range counts.Rows() {
		fmt.Printf("| %s | %d |\n", row.Shape, row.N)
	}
	fmt.Printf("| **classified** | **%d** |\n", counts.Total())
	fmt.Printf("| recovered through a checkpoint | %d |\n", checkpointed)
	fmt.Printf("| seeds that failed | %d |\n", failed)
	fmt.Printf("| seeds that produced no observation | %d |\n", len(seeds)-tally.Observations())
	fmt.Printf("\n%s\n", reconciliation)
	fmt.Printf("\ndirectory fsync on this build: %v (%s)\n", g.DirSync, g.Platform)
	if failed > 0 {
		return fmt.Errorf("%d of %d seeds failed", failed, len(seeds))
	}
	if !reconciles {
		return fmt.Errorf("the corpus did not reconcile: %d observations from %d seeds", tally.Observations(), len(seeds))
	}
	return nil
}
