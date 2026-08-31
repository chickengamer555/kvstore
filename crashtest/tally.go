package crashtest

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Tally reconciles the seeds a run attempted against the observations it
// actually made.
//
// The two are not the same number and the corpus's whole claim rests on the
// second one. A seed the harness gives up on - a child that never reaches its
// kill point, an output pipe that does not close after the kill - produces no
// findings, so it fails nothing. It is simply absent, and a run that prints
// "240 seeds" underneath it is reporting a check that did not run as a check
// that passed.
//
// That is the shape this repository keeps finding in itself: a 32-bit CI step
// green over an input nothing reaches, a corpus reporting zero torn records
// without saying whether it could produce one, a seed the harness gave up on
// counted in a total. The rule those three add up to is in docs/verification.md; this type
// is the mechanical part of it for the corpus.
//
// It reconciles SETS, not totals. Every seed in the corpus must be accounted
// for exactly once, and nothing outside it may be accounted for at all, so two
// observations of one seed cannot balance against a seed that never ran.
// Nothing in the corpus can produce that substitution today - each subtest
// accounts for its own seed exactly once - but that is an argument about the
// caller, and it is the same argument that was made for counting seeds rather
// than observations.
//
// Reconcile is the only way to obtain the observation count: it comes back
// inside the same value as the verdict and the seed names, so nobody holds the
// reassuring number without having been handed the rest of it. That is a claim
// about this API surface and nothing else, and
// TestTheObservationCountCannotBeObtainedWithoutTheVerdict pins it. What it
// does not claim, because nothing here can enforce it, is that a caller prints
// both halves.
type Tally struct {
	mu       sync.Mutex
	observed map[uint64]int
	missing  map[uint64]string
}

// Missing is one seed that produced no observation, and why.
type Missing struct {
	Seed   uint64
	Reason string
}

// Observe records that a seed produced a verdict - pass or fail, both are
// observations.
func (t *Tally) Observe(seed uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.observed == nil {
		t.observed = map[uint64]int{}
	}
	t.observed[seed]++
}

// NoObservation records that a seed was attempted and produced nothing.
//
// A second, different reason for the same seed is kept alongside the first
// rather than replacing it. Two accounts of why one seed produced nothing is
// itself the interesting fact, and the later one is not more trustworthy than
// the earlier one.
func (t *Tally) NoObservation(seed uint64, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.missing == nil {
		t.missing = map[uint64]string{}
	}
	if prev, seen := t.missing[seed]; seen && prev != reason {
		reason = prev + "; and again: " + reason
	}
	t.missing[seed] = reason
}

// Reconciliation is the answer to "did this run cover the corpus", together
// with every number a reader needs to check it. It is the only way to get the
// observation count, which is the point: the count and the verdict arrive in
// one value, so nobody prints the first without having been handed the second.
type Reconciliation struct {
	// Corpus is how many seeds the run was reconciled against.
	Corpus int
	// Observed is how many of them produced a verdict - pass or fail, both are
	// observations. A seed observed twice counts once here and is named in
	// Repeated, so this can never exceed Corpus.
	Observed int
	// Missing is the seeds that were attempted and produced nothing, with the
	// reason for each, ordered by seed.
	Missing []Missing
	// Unattempted is the seeds in the corpus that were never accounted for at
	// all, named so they can be re-run.
	Unattempted []uint64
	// Repeated is the seeds accounted for more than once.
	Repeated []uint64
	// Unexpected is the seeds accounted for that are not in this corpus.
	Unexpected []uint64
	// OK is true only when every seed in the corpus produced exactly one
	// observation and nothing else was accounted for.
	OK bool
}

// Reconcile reports whether the run covered the corpus.
//
// Four things have to hold and each is reported separately, because they are
// four different failures: every seed in the corpus observed, none attempted
// without producing an observation, none observed twice, and none accounted
// for that is not in the corpus.
func (t *Tally) Reconcile(corpus []uint64) Reconciliation {
	t.mu.Lock()
	defer t.mu.Unlock()

	inCorpus := make(map[uint64]bool, len(corpus))
	for _, seed := range corpus {
		inCorpus[seed] = true
	}

	rec := Reconciliation{Corpus: len(corpus)}
	for seed, n := range t.observed {
		if inCorpus[seed] {
			rec.Observed++
		} else {
			rec.Unexpected = append(rec.Unexpected, seed)
		}
		if n > 1 {
			rec.Repeated = append(rec.Repeated, seed)
		}
	}
	for seed, reason := range t.missing {
		rec.Missing = append(rec.Missing, Missing{Seed: seed, Reason: reason})
		if !inCorpus[seed] {
			rec.Unexpected = append(rec.Unexpected, seed)
		}
	}
	for seed := range inCorpus {
		_, attempted := t.missing[seed]
		if t.observed[seed] == 0 && !attempted {
			rec.Unattempted = append(rec.Unattempted, seed)
		}
	}

	// Map iteration order is random, and a report whose lines move between two
	// runs of the same failure cannot be diffed. Everything printed is ordered.
	slices.Sort(rec.Unattempted)
	slices.Sort(rec.Repeated)
	slices.Sort(rec.Unexpected)
	rec.Unexpected = slices.Compact(rec.Unexpected)
	slices.SortFunc(rec.Missing, func(a, b Missing) int { return cmp.Compare(a.Seed, b.Seed) })

	rec.OK = rec.Observed == rec.Corpus &&
		len(rec.Missing) == 0 &&
		len(rec.Unattempted) == 0 &&
		len(rec.Repeated) == 0 &&
		len(rec.Unexpected) == 0
	return rec
}

// namesShown caps how many seed numbers one line prints. A run that reached
// none of a 240-seed corpus would otherwise print all 240, and what a reader
// can act on is the count plus enough names to start from. The line says so
// when it has capped.
const namesShown = 32

// String is the line a run prints. It states the numbers first, and then, if
// it does not reconcile, why not and which seeds.
func (r Reconciliation) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "corpus reconciliation: %d seeds in the corpus, %d %s made, %d attempted and produced no observation, %d never attempted",
		r.Corpus, r.Observed, plural(r.Observed, "observation"), len(r.Missing), len(r.Unattempted))
	if r.OK {
		return b.String()
	}
	b.WriteString("\n  A seed that produced no observation is not a seed that passed: it has no findings, so it fails nothing, and the corpus is that many observations short of what this run claims.")
	for _, m := range r.Missing {
		fmt.Fprintf(&b, "\n  seed %d: %s", m.Seed, m.Reason)
	}
	writeSeeds(&b, "never attempted", r.Unattempted)
	writeSeeds(&b, "observed more than once, so another seed went unrun in its place", r.Repeated)
	writeSeeds(&b, "accounted for but not in this corpus, so whatever fed this tally and whatever reconciled it disagree about what was run", r.Unexpected)
	return b.String()
}

func writeSeeds(b *strings.Builder, label string, seeds []uint64) {
	if len(seeds) == 0 {
		return
	}
	fmt.Fprintf(b, "\n  %d %s %s:", len(seeds), plural(len(seeds), "seed"), label)
	for _, seed := range seeds[:min(len(seeds), namesShown)] {
		fmt.Fprintf(b, " %d", seed)
	}
	if len(seeds) > namesShown {
		fmt.Fprintf(b, " ... and %d more", len(seeds)-namesShown)
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
