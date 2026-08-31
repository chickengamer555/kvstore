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

// Reconcile returns what to print and whether the run covered the corpus.
//
// It reconciles when the number of observations matches the size of the
// corpus and nothing was attempted without producing one. Seeds that were
// attempted and produced nothing are named with their reasons; seeds that were
// never started at all are counted separately, because a package deadline
// running out is a different fact from a child wedging and the difference is
// what a reader needs.
func (t *Tally) Reconcile(corpus int) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	missing := append([]Missing(nil), t.missing...)
	sort.Slice(missing, func(i, j int) bool { return missing[i].Seed < missing[j].Seed })
	unattempted := corpus - t.observed - len(missing)

	var b strings.Builder
	fmt.Fprintf(&b, "corpus reconciliation: %d seeds in the corpus, %d observations made, %d attempted and produced no observation, %d never attempted",
		corpus, t.observed, len(missing), unattempted)
	if t.observed == corpus && len(missing) == 0 && unattempted == 0 {
		return b.String(), true
	}
	b.WriteString("\n  A seed that produced no observation is not a seed that passed: it has no findings, so it fails nothing, and the corpus is that many observations short of what this run claims.")
	for _, m := range missing {
		fmt.Fprintf(&b, "\n  seed %d: %s", m.Seed, m.Reason)
	}
	return b.String(), false
}
