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
		points, unjudged := in.evaluationPoints(declared)
		for _, why := range unjudged {
			out.note("%s", why)
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
// why it is not evaluated at the other checkpoints.
// DESIGN.md §4 counts the teardown's checkpoint, so `checkpoint` and `end`
// keep it; the events the teardown itself caused are botbox's own doing.
func (in Input) evaluationPoints(declared target.Property) (points []time.Time, unjudged []string) {
	checkpoints := in.Checkpoints
	switch declared.When {
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
		if why := in.unjudgeable(checkpoint, declared.ID); why != "" {
			unjudged = append(unjudged, fmt.Sprintf("at the checkpoint after %s: %s", in.describeOp(checkpoint.Op), why))
			continue
		}
		points = append(points, checkpoint.Time)
	}
	return points, unjudged
}

// unjudgeable says why the property is not evaluated at the checkpoint, or is
// empty where it is: what the property reads may be about to change, or the
// target may not yet have acted on it.
func (in Input) unjudgeable(checkpoint Checkpoint, property string) string {
	if checkpoint.Held {
		return fmt.Sprintf("the proxy held a request of the target's there, or released one in the last %s (timeouts.stable), which may still change what %s reads",
			in.timeouts().Stable, property)
	}
	if down := in.notBack(checkpoint.Time); down != "" {
		return fmt.Sprintf("the target %s, so it may not yet have acted on what %s reads", down, property)
	}
	return ""
}
