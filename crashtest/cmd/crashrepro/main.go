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
	flag.Parse()

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
	if !res.OK() {
		os.Exit(1)
	}
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
