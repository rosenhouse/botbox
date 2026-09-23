package invariant

import (
	"fmt"
	"slices"
	"time"

	"github.com/rosenhouse/botbox/pkg/observe"
)

// SelfHealing is G7: an object a DeleteManaged op deleted exists again, by
// kind and name, where the settle wait after the op ends. A kind the target
// lists as not recreated is exempt.
func SelfHealing(in Input) (Result, error) {
	out := Result{ID: "G7"}
	for _, checkpoint := range in.Checkpoints {
		op, _ := in.op(checkpoint.Op) // Teardown and Recovery follow no op.
		deleted := op.Deleted
		if deleted == (observe.Key{}) || slices.Contains(in.Target.NotRecreated, deleted.GVK) {
			continue
		}
		end := checkpoint.Time
		seen := in.stateAt(end)
		if _, back := seen.version(deleted); back {
			continue
		}
		if cr, found := seen.cr(in.Target.Primary); !found || cr.DeletionTimestamp != nil {
			continue // No CR asks for the object back.
		}
		object := kindName(deleted.GVK) + " " + deleted.Name
		if changed, found := in.changedBetween(in.lastConverged(op.Time), op.Time); found {
			out.note("for %s: the run had not converged since %s, so the target may not have wanted the %s back",
				describe(op), describe(changed), object)
			continue
		}
		if in.faulted(op.Time, end) {
			out.note("for %s: a fault was active in the wait after it, so the target may have been unable to recreate the %s",
				describe(op), object)
			continue
		}
		out.violate(Violation{
			Statement: fmt.Sprintf("the %s that %s deleted never came back within the %s the run waited after it",
				object, describe(op), end.Sub(op.Time).Round(time.Millisecond)),
			At: end,
		}.quotingVersions(RecentHistory(deleted, upTo(in.History.History(deleted), end))))
	}
	return out, nil
}
