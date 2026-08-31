package crashtest_test

import (
	"fmt"
	"reflect"
	"slices"
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

	rec := tally.Reconcile([]uint64{11, 22, 33, 44})
	if rec.OK {
		t.Errorf("a corpus of 4 with 3 observations reconciled:\n%s", rec)
	}
	if rec.Observed != 3 {
		t.Errorf("Observed = %d, want 3", rec.Observed)
	}

	// What a reader has to be able to see in the line itself. The count of
	// observations, distinct from the size of the corpus; and which seed, and
	// why - because "2 seeds produced no observation" with no names is the same
	// kind of claim as a green tick.
	report := rec.String()
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
//
// And the seeds have to be NAMED. "9 never attempted" tells a reader the run
// was short; it does not tell them which nine, so it cannot be re-run, which
// is the only thing anyone wants at that point. The whole argument for this
// corpus is that a failure names the seed that caused it.
func TestATallyNamesTheSeedsItNeverAttempted(t *testing.T) {
	corpus := []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	var tally crashtest.Tally
	tally.Observe(1)

	rec := tally.Reconcile(corpus)
	if rec.OK {
		t.Errorf("a corpus of 10 with 1 observation reconciled:\n%s", rec)
	}
	if got := slices.Clone(rec.Unattempted); !slices.Equal(got, corpus[1:]) {
		t.Errorf("Unattempted = %v, want %v", got, corpus[1:])
	}
	report := rec.String()
	if !strings.Contains(report, "never attempted") {
		t.Errorf("the report does not say that any seed was never attempted:\n%s", report)
	}
	for _, seed := range corpus[1:] {
		if !strings.Contains(report, fmt.Sprint(seed)) {
			t.Errorf("the report does not name seed %d, so it cannot be re-run:\n%s", seed, report)
		}
	}
}

// The failure the count-based version cannot see, and the reason this
// reconciles sets rather than totals.
//
// Two observations of seed 11 and none of seed 22 is two observations over a
// corpus of two: the arithmetic balances and the run reports a complete
// corpus. One seed stood in for another and nothing said so. That is precisely
// the substitution this type exists to refuse, one level down from the one it
// was built for.
func TestATallyDoesNotReconcileWhenOneSeedStandsInForAnother(t *testing.T) {
	var tally crashtest.Tally
	tally.Observe(11)
	tally.Observe(11)

	rec := tally.Reconcile([]uint64{11, 22})
	if rec.OK {
		t.Fatalf("two observations of one seed reconciled a corpus of two:\n%s", rec)
	}
	if !slices.Equal(rec.Unattempted, []uint64{22}) {
		t.Errorf("Unattempted = %v, want [22] - seed 22 was never run", rec.Unattempted)
	}
	if !slices.Equal(rec.Repeated, []uint64{11}) {
		t.Errorf("Repeated = %v, want [11] - seed 11 was observed twice", rec.Repeated)
	}
	report := rec.String()
	for _, want := range []string{"11", "22"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not name seed %s:\n%s", want, report)
		}
	}
}

// Accounting against the wrong corpus. A tally handed an observation for a
// seed that is not in the corpus it is reconciled against has been fed by
// something that disagrees with it about what is being run, and the totals can
// still balance while it happens. It is not a durability bug; it is the
// bookkeeping going wrong in the one place whose entire job is bookkeeping.
func TestATallyRejectsAnObservationForASeedThatIsNotInTheCorpus(t *testing.T) {
	var tally crashtest.Tally
	tally.Observe(11)
	tally.Observe(99)

	rec := tally.Reconcile([]uint64{11, 22})
	if rec.OK {
		t.Fatalf("an observation of a seed outside the corpus reconciled:\n%s", rec)
	}
	if !slices.Equal(rec.Unexpected, []uint64{99}) {
		t.Errorf("Unexpected = %v, want [99]", rec.Unexpected)
	}
	if !slices.Equal(rec.Unattempted, []uint64{22}) {
		t.Errorf("Unattempted = %v, want [22]", rec.Unattempted)
	}
}

// The green case, and it has to be green for the right reason: an observation
// for every seed in the corpus, not merely no complaints.
func TestATallyReconcilesWhenEverySeedWasObserved(t *testing.T) {
	corpus := []uint64{1, 2, 3, 4, 5}
	var tally crashtest.Tally
	for _, seed := range corpus {
		tally.Observe(seed)
	}
	rec := tally.Reconcile(corpus)
	if !rec.OK {
		t.Errorf("five observations over a corpus of five did not reconcile:\n%s", rec)
	}
	if rec.Observed != 5 {
		t.Errorf("Observed = %d, want 5", rec.Observed)
	}
	if !strings.Contains(rec.String(), "5 observation") {
		t.Errorf("the report does not state how many observations were made:\n%s", rec)
	}
}

// The claim in Tally's doc comment, made checkable.
//
// The comment says the count and the verdict come out of the same call, so a
// caller cannot obtain the reassuring half without being handed the other half
// with it. That is a claim about the type's API surface, and an exported
// method returning a bare count is all it takes to make it false - which is
// what Observations() was. This pins the surface so the next such method has
// to argue with a test rather than slip in beside a comment that still says
// otherwise.
//
// What it establishes: no exported method hands out a number on its own. What
// it does not establish: that a caller prints both halves. Nothing here can
// stop someone printing rec.Observed and discarding rec.OK; the difference is
// that they were told.
func TestTheObservationCountCannotBeObtainedWithoutTheVerdict(t *testing.T) {
	want := []string{"NoObservation", "Observe", "Reconcile"}

	typ := reflect.TypeOf(&crashtest.Tally{})
	var got []string
	for i := range typ.NumMethod() {
		got = append(got, typ.Method(i).Name)
	}
	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Errorf("the exported method set of *Tally is %v, want %v.\n"+
			"A method that returns a count on its own makes Tally's doc comment false. Either it returns the\n"+
			"verdict with it, or the comment stops claiming the two are inseparable. Do not just add it here.",
			got, want)
	}
}
