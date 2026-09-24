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
		if took, _ := in.versionAt(deleted, op.Time); !in.askedFor(took, seen) {
			continue // No CR asks for the object back.
		}
		object := kindName(deleted.GVK) + " " + deleted.Name
		if changed, found := in.changedBetween(in.lastConverged(op.Time), op.Time); found {
			out.note("for %s: the run had not converged since %s, so the target may not have wanted the %s back",
				describe(op), describe(changed), object)
			continue
		}
		if in.faulted(op.Time, end) || end.Before(in.Owed(op.Time)) {
			out.note("for %s: a fault was active during it or the wait after it, or the target was still owed time to recover from one where the wait ended, so it may have been unable to recreate the %s",
				describe(op), object)
			continue
		}
		if start, starting := in.stillStarting(op); starting {
			out.note("for %s: the target had requested no resource outside leader election between %s and it, so it may not yet have been running to recreate the %s",
				describe(op), start, object)
			continue
		}
		if in.stopped(op.Time, end) {
			out.note("for %s: the target exited, or waited to restart, during it or the wait after it, so it may not have been running to recreate the %s",
				describe(op), object)
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

// stillStarting names the target's last start before the op, a Restart op or
// the restart after an exit, if the target requested nothing between the two
// that shows it running, since botbox has no other sign that it is back.
func (in Input) stillStarting(op Op) (string, bool) {
	var start time.Time
	var named string
	for _, earlier := range in.Ops {
		if earlier.Type == OpRestart && earlier.Time.Before(op.Time) {
			start, named = earlier.Time, describe(earlier)
		}
	}
	for _, exit := range in.Exits {
		if exit.Restart.Before(op.Time) && exit.Restart.After(start) {
			start, named = exit.Restart, "the restart after its exit during "+describe(in.opBy(exit.At))
		}
	}
	if named == "" {
		return "", false
	}
	running := slices.ContainsFunc(in.Requests, func(r proxy.Request) bool {
		return r.Start.After(start) && r.Start.Before(op.Time) && showsRunning(r)
	})
	return named, !running
}

// stopped reports whether the target exited, or waited to restart, at some
// point in [from, to].
func (in Input) stopped(from, to time.Time) bool {
	return slices.ContainsFunc(in.Exits, func(exit Exit) bool {
		return !exit.At.After(to) && !exit.Restart.Before(from)
	})
}

// showsRunning reports whether a request shows the target past starting up. A
// process waiting to lead requests only leader election and paths that name no
// resource, such as discovery.
func showsRunning(r proxy.Request) bool { return r.Resource != "" && !leaderElection(r) }
