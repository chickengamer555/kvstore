package crashtest

import (
	"fmt"
	"sort"
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
// without saying whether it could produce one, a wedged seed counted in a
// total. The rule those three add up to is in docs/verification.md; this type
// is the mechanical part of it for the corpus.
//
// Reconcile hands back the sentence and the verdict in one return, so a caller
// that prints the sentence has been given the verdict with it.
type Tally struct {
	mu       sync.Mutex
	observed int
	missing  []Missing
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
	t.observed++
}

// NoObservation records that a seed was attempted and produced nothing.
func (t *Tally) NoObservation(seed uint64, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.missing = append(t.missing, Missing{Seed: seed, Reason: reason})
}

// Observations is how many seeds produced a verdict.
func (t *Tally) Observations() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.observed
}

// Reconciliation is the answer to "did this run cover the corpus", together
// with every number a reader needs to check it. It is the only way to get the
// observation count, which is the point: the count and the verdict arrive in
// one value, so nobody prints the first without having been handed the second.
type Reconciliation struct {
	// Corpus is how many seeds the run was reconciled against.
	Corpus int
	// Observed is how many of them produced a verdict - pass or fail, both are
	// observations.
	Observed int
	// Missing is the seeds that were attempted and produced nothing, with the
	// reason for each.
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

// String is the line a run prints. It states the numbers first and then, if it
// does not reconcile, why not and which seeds.
func (r Reconciliation) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "corpus reconciliation: %d seeds in the corpus, %d observations made, %d attempted and produced no observation, %d never attempted",
		r.Corpus, r.Observed, len(r.Missing), r.Corpus-r.Observed-len(r.Missing))
	if r.OK {
		return b.String()
	}
	b.WriteString("\n  A seed that produced no observation is not a seed that passed: it has no findings, so it fails nothing, and the corpus is that many observations short of what this run claims.")
	for _, m := range r.Missing {
		fmt.Fprintf(&b, "\n  seed %d: %s", m.Seed, m.Reason)
	}
	return b.String()
}

// Reconcile reports whether the run covered the corpus.
//
// It compares the number of observations against the size of the corpus, and
// names the seeds that were attempted and produced nothing. Seeds that were
// never started at all are counted rather than named, because a package
// deadline running out is a different fact from a child wedging.
func (t *Tally) Reconcile(corpus []uint64) Reconciliation {
	t.mu.Lock()
	defer t.mu.Unlock()

	missing := append([]Missing(nil), t.missing...)
	sort.Slice(missing, func(i, j int) bool { return missing[i].Seed < missing[j].Seed })

	rec := Reconciliation{Corpus: len(corpus), Observed: t.observed, Missing: missing}
	rec.OK = rec.Observed == rec.Corpus && len(missing) == 0
	return rec
}
