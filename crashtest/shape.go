package crashtest

// The recovery shapes a crashed store can be left in, and the tally over them.
//
// This exists because of a specific way the previous version was misleading.
// Shapes were counted straight into a map[string]int and the results printed by
// ranging over the keys, which means a shape that never occurred has no key and
// can never print a row. The README quoted five rows and called the three zeros
// "the interesting ones" - and those were exactly the rows the command could
// not produce. A reader running the documented check got a shorter table and
// had to infer the zeros from absence.
//
// Worse, absence and "the classifier is broken" are indistinguishable in that
// output. If KilledInCheckpointWrite stopped being set for any reason, the
// printed table would be byte-identical to the one that was there.
//
// So the shape set is enumerated, the tally starts every shape at zero, and the
// rows come out in a fixed order whether the shape occurred or not. A zero is
// then something the command asserts rather than something the reader
// reconstructs.

// Shape names. The order here is the order rows are printed in, and it runs
// from the most specific classification to the least.
const (
	ShapeKilledWritingCheckpoint       = "killed while writing a checkpoint"
	ShapeCheckpointRejected            = "checkpoint rejected on its checksum"
	ShapeKilledBetweenRotateAndDelete  = "killed between rotating the log and deleting the old segments"
	ShapeKilledBeforeSupersededDeleted = "killed before the superseded segments were deleted"
	ShapeDamagedRecord                 = "recovery stopped at a damaged record"
	ShapeCleanTail                     = "log ended on a record boundary"
)

// Shapes is every shape Shape can return, in print order.
func Shapes() []string {
	return []string{
		ShapeKilledWritingCheckpoint,
		ShapeCheckpointRejected,
		ShapeKilledBetweenRotateAndDelete,
		ShapeKilledBeforeSupersededDeleted,
		ShapeDamagedRecord,
		ShapeCleanTail,
	}
}

// Shape classifies one crashed directory by what recovery found in it.
//
// The order of the cases is the classification: a run that was killed inside a
// checkpoint write is reported as that even though it also has more than one
// segment, because the narrower fact is the interesting one.
func Shape(r Result) string {
	switch {
	case r.KilledInCheckpointWrite:
		return ShapeKilledWritingCheckpoint
	case r.Report.CheckpointRejected:
		return ShapeCheckpointRejected
	case r.Report.Segments > 1:
		return ShapeKilledBetweenRotateAndDelete
	case r.Report.Skipped > 0:
		return ShapeKilledBeforeSupersededDeleted
	case r.Report.Stopped != "end-of-log":
		return ShapeDamagedRecord
	default:
		return ShapeCleanTail
	}
}

// ShapeRow is one line of the tally.
type ShapeRow struct {
	Shape string
	N     int
}

// ShapeCounts is a tally over every shape, including the ones that did not
// happen.
type ShapeCounts map[string]int

// NewShapeCounts starts every known shape at zero, which is what makes a zero
// row printable.
func NewShapeCounts() ShapeCounts {
	// STUB: an empty tally, which is exactly the bug this is meant to fix -
	// only shapes that occur get a key, so a shape that never happens can
	// never print a row.
	return ShapeCounts{}
}

// Add counts one result.
func (c ShapeCounts) Add(r Result) { c[Shape(r)]++ }

// Rows returns every shape in print order with its count, zeros included.
func (c ShapeCounts) Rows() []ShapeRow {
	// STUB: ranges the observed keys, so it can only report what happened.
	out := make([]ShapeRow, 0, len(c))
	for _, s := range Shapes() {
		if n, ok := c[s]; ok {
			out = append(out, ShapeRow{Shape: s, N: n})
		}
	}
	return out
}
