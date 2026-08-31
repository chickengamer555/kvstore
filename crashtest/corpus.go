package crashtest

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"
)

//go:embed corpus.txt
var corpusFile string

// CorpusOrigin is the splitmix64 state the seed corpus was generated from.
//
// The corpus is a recorded file rather than "whatever the clock said", because
// a run that cannot be repeated cannot be argued with. But a recorded file
// raises the obvious question - was it curated? Were the seeds that failed
// quietly deleted? So the file is not arbitrary either: it is the output of
// GenerateCorpus below, and TestCorpusSizeFloor checks that it still is.
//
// Anyone can regenerate it and diff. Removing an inconvenient seed changes the
// file, and changing the file fails the suite.
const CorpusOrigin uint64 = 0x9E3779B97F4A7C15

// CorpusFloor is the number of seeds CI insists on. Below this the corpus
// stops being evidence and becomes a smoke test.
const CorpusFloor = 200

// GenerateCorpus returns the first n seeds of the corpus sequence: splitmix64
// from CorpusOrigin. Splitmix64 rather than anything cleverer because it is
// twenty lines, has no library behind it, and can be reimplemented in any
// language by anyone who wants to check this file independently.
func GenerateCorpus(n int) []uint64 {
	out := make([]uint64, 0, n)
	x := CorpusOrigin
	for range n {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		out = append(out, z^(z>>31))
	}
	return out
}

// Corpus is the recorded seed list, parsed from corpus.txt.
func Corpus() ([]uint64, error) {
	var out []uint64
	for i, line := range strings.Split(corpusFile, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		seed, err := strconv.ParseUint(line, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("crashtest: corpus.txt line %d: %w", i+1, err)
		}
		out = append(out, seed)
	}
	return out, nil
}
