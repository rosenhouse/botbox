package invariant

import (
	"fmt"
	"slices"
	"time"

	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/proxy"
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
		if restart, starting := in.stillStarting(op); starting {
			out.note("for %s: the target had requested no resource outside leader election between %s and it, so it may not yet have been running to recreate the %s",
				describe(op), describe(restart), object)
			continue
		}
		out.violate(Violation{
			Statement: fmt.Sprintf("the %s that %s deleted never came back within the %s the run waited after it, and the target does not list %s under notRecreated",
				object, describe(op), end.Sub(checkpoint.Began).Round(time.Millisecond), kindName(deleted.GVK)),
			At: end,
		}.quotingVersions(RecentHistory(deleted, upTo(in.History.History(deleted), end))).
			quotingManaged(Sample(seen.managed(in))))
	}
	return out, nil
}

// stillStarting returns the last Restart before the op if the target requested
// nothing between the two that shows it running, since botbox has no other sign
// that a restarted target is back.
func (in Input) stillStarting(op Op) (Op, bool) {
	var restart *Op
	for i, earlier := range in.Ops {
		if earlier.Type == OpRestart && earlier.Time.Before(op.Time) {
			restart = &in.Ops[i]
		}
	}
	if restart == nil {
		return Op{}, false
	}
	running := slices.ContainsFunc(in.Requests, func(r proxy.Request) bool {
		return r.Start.After(restart.Time) && r.Start.Before(op.Time) && showsRunning(r)
	})
	return *restart, !running
}

// showsRunning reports whether a request shows the target past starting up. A
// process waiting to lead requests only leader election and paths that name no
// resource, such as discovery.
func showsRunning(r proxy.Request) bool { return r.Resource != "" && !leaderElection(r) }
