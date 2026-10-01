package invariant

import (
	"fmt"
	"time"

	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/target"
)

// Property returns the check of one declared property, evaluated where its
// `when` says: on every Observer event, at each checkpoint, or at the last
// checkpoint alone (DESIGN.md §8.1). It reports the first instant the
// property did not hold. The predicate reads the recorded objects and must
// not modify them.
func Property(declared target.Property) Check {
	return func(in Input) (Result, error) {
		out := Result{ID: declared.ID}
		points, held := in.evaluationPoints(declared.When)
		for _, checkpoint := range held {
			out.note("at the checkpoint after %s: the proxy still held a request of the target's there, which may have been about to change what %s reads",
				in.describeOp(checkpoint.Op), declared.ID)
		}
		for _, s := range in.statesAt(points) {
			managed := s.managed(in)
			crs := s.crs(in.Target.Primary)
			if len(crs) == 0 {
				crs = []observe.Version{{}} // The predicate reads no CR.
			}
			for _, cr := range crs {
				theirs := managed
				if cr.Object != nil {
					theirs = in.objectsOf(cr.UID, managed)
				}
				holds, err := declared.Eval(cr.Object, objects(theirs))
				if err != nil {
					return Result{}, fmt.Errorf("evaluating property %s: %w", declared.ID, err)
				}
				if holds {
					continue
				}
				violation := Violation{
					Statement: fmt.Sprintf("the property did not hold: %s", declared.Description),
					At:        s.at,
				}.quotingManaged(Sample(theirs))
				if cr.Object != nil {
					violation.Statement = fmt.Sprintf("the property did not hold on the CR %s: %s", cr.Name, declared.Description)
					violation = violation.quotingVersions(RecentHistory(cr.Key, upTo(in.History.History(cr.Key), s.at)))
				}
				out.violate(violation)
				return out, nil // A run ends at its first violation (DESIGN.md §5.5).
			}
		}
		return out, nil
	}
}

// evaluationPoints are the instants a property is evaluated at, in order, and
// the checkpoints it is not, where the proxy held a request of the target's.
// DESIGN.md §4 counts the teardown's checkpoint, so `checkpoint` and `end`
// keep it; the events the teardown itself caused are botbox's own doing.
func (in Input) evaluationPoints(when target.PropertyWhen) (points []time.Time, held []Checkpoint) {
	checkpoints := in.Checkpoints
	switch when {
	case target.Always:
		for _, v := range in.versions() {
			if in.tornDown(v.Time) {
				continue
			}
			points = append(points, v.Time)
		}
		return points, nil
	case target.End:
		checkpoints = checkpoints[max(len(checkpoints)-1, 0):]
	}
	// The loader reads an unset `when` as checkpoint (DESIGN.md §8.1).
	for _, checkpoint := range checkpoints {
		if checkpoint.Held {
			held = append(held, checkpoint)
			continue
		}
		points = append(points, checkpoint.Time)
	}
	return points, held
}
