package crashtest_test

import (
	"strings"
	"testing"

	"github.com/chickengamer555/kvstore/crashtest"
)

// B5. The corpus's claim is a number of observations, and this is the thing
// that stops that number from being a number of attempts.
//
// The reviewer's question, which this exists to answer: "how does a reader
// distinguish, from the outside, a corpus that ran 240 seeds from one that ran
// 238 and hung on two?" Before this, they could not. A seed the harness gives
// up on produces no findings, so it fails nothing; it is simply absent from a
// run that then prints a reassuring total.
func TestATallyThatIsMissingAnObservationCannotPrintACompleteCorpus(t *testing.T) {
	var tally crashtest.Tally
	for _, seed := range []uint64{11, 22, 33} {
		tally.Observe(seed)
	}
	tally.NoObservation(44, "its output pipe did not close")

	report, ok := tally.Reconcile(4)
	if ok {
		t.Errorf("a corpus of 4 with 3 observations reconciled:\n%s", report)
	}
	if tally.Observations() != 3 {
		t.Errorf("Observations() = %d, want 3", tally.Observations())
	}

	// What a reader has to be able to see in the line itself. The count of
	// observations, distinct from the size of the corpus; and which seed, and
	// why - because "2 seeds produced no observation" with no names is the same
	// kind of claim as a green tick.
	for _, want := range []string{"3 observation", "4 seeds", "seed 44", "its output pipe did not close"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not contain %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "4 observation") {
		t.Errorf("the report describes 4 observations when 3 were made:\n%s", report)
	}
}

// A seed that was never started is a different thing from a seed that started
// and produced nothing, and both are different from a seed that passed. All
// three have to be visible, because the reason a corpus goes short is worth
// knowing: a package deadline is not a wedged child.
func TestATallyCountsSeedsThatWereNeverAttempted(t *testing.T) {
	var tally crashtest.Tally
	tally.Observe(1)

	report, ok := tally.Reconcile(10)
	if ok {
		t.Errorf("a corpus of 10 with 1 observation reconciled:\n%s", report)
	}
	if !strings.Contains(report, "9") || !strings.Contains(report, "never attempted") {
		t.Errorf("the report does not say that 9 seeds were never attempted:\n%s", report)
	}
}

// The green case, and it has to be green for the right reason: an observation
// for every seed in the corpus, not merely no complaints.
func TestATallyReconcilesWhenEverySeedWasObserved(t *testing.T) {
	var tally crashtest.Tally
	for seed := uint64(1); seed <= 5; seed++ {
		tally.Observe(seed)
	}
	report, ok := tally.Reconcile(5)
	if !ok {
		t.Errorf("five observations over a corpus of five did not reconcile:\n%s", report)
	}
	if !strings.Contains(report, "5 observation") {
		t.Errorf("the report does not state how many observations were made:\n%s", report)
	}
}
