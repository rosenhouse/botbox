package invariant

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/target"
)

// Convergence is G4: within T_settle after any spec change, and by what Owed
// gives after faults stop, the target's Ready predicate holds (DESIGN.md §6).
// A settle wait that expired while no fault excused it is a violation of its
// own (§5.5).
func Convergence(in Input) (Result, error) {
	out := Result{ID: "G4"}
	for _, from := range in.convergeAnchors() {
		deadline := from.deadline
		if !in.observed(deadline) || in.faulted(from.at, deadline) || in.tornDown(deadline) || in.respecified(from.at, deadline) {
			continue
		}
		seen := in.stateAt(deadline)
		cr, found := seen.cr(in.Target.Primary)
		if !found {
			continue // The run has no CR to be ready: it deleted it.
		}
		if cr.DeletionTimestamp != nil {
			continue // A CR under deletion need not be ready; G3 judges it (§6).
		}
		ready, err := in.Target.Ready(cr.Object)
		if errors.Is(err, target.ErrNotBool) {
			return out, err
		}
		if ready && err == nil {
			continue
		}
		violation := Violation{
			Statement: fmt.Sprintf("the CR %s was not ready %s after %s%s%s",
				cr.Name, deadline.Sub(from.at).Round(time.Millisecond), from.what, quoted(err), in.repeated(from.at, deadline)),
			At: deadline,
		}.quotingVersions(RecentHistory(cr.Key, upTo(in.History.History(cr.Key), deadline))).
			quotingManaged(Sample(seen.managed(in)))
		violation.Ready = in.readiness(cr, err)
		out.violate(violation)
	}
	return out, out.reportExpiredWaits(in)
}

// anchor is a moment the target must have converged by a deadline after.
type anchor struct {
	at, deadline time.Time
	what         string
}

func (in Input) convergeAnchors() []anchor {
	var anchors []anchor
	for _, op := range in.Ops {
		if op.Type.changesSpec() {
			anchors = append(anchors, anchor{at: op.Time, deadline: in.readyBy(op.Time), what: describe(op)})
		}
	}
	for _, fault := range in.Faults {
		if !fault.End.IsZero() {
			anchors = append(anchors, anchor{at: fault.End, deadline: in.readyBy(fault.End), what: "the fault stopped"})
		}
	}
	slices.SortFunc(anchors, func(a, b anchor) int { return a.at.Compare(b.at) })
	return anchors
}

// readyBy is when the target must be ready after at: T_settle later, or when
// Owed says if that is later. A settle wait that converged before then shows
// the target had recovered.
func (in Input) readyBy(at time.Time) time.Time {
	owed := in.Owed(at)
	if converged := in.nextConverged(at); !converged.IsZero() && converged.Before(owed) {
		owed = converged
	}
	return later(at.Add(in.timeouts().Settle), owed)
}

// respecified reports whether a later op changed the spec inside the window,
// which hands the window to that op.
func (in Input) respecified(from, to time.Time) bool {
	return slices.ContainsFunc(in.Ops, func(op Op) bool {
		return op.Type.changesSpec() && op.Time.After(from) && op.Time.Before(to)
	})
}

// reportExpiredWaits records the settle waits that ran out while nothing
// excused them.
func (out *Result) reportExpiredWaits(in Input) error {
	for _, checkpoint := range in.Checkpoints {
		if checkpoint.Settle != Expired || in.Excused(checkpoint) {
			continue
		}
		violation, err := in.ExpiredWait(checkpoint)
		if err != nil {
			return err
		}
		out.violate(violation)
	}
	return nil
}

func (in Input) describeOp(index int) string {
	if op, found := in.op(index); found {
		return describe(op)
	}
	if index == Recovery {
		return "the last fault stopped"
	}
	return "teardown"
}

// upTo drops the versions recorded after the verdict, which a report of what
// the run looked like at that instant cannot quote.
func upTo(versions []observe.Version, t time.Time) []observe.Version {
	return slices.DeleteFunc(slices.Clone(versions), func(v observe.Version) bool { return v.Time.After(t) })
}

func describe(op Op) string { return fmt.Sprintf("op %d (%s)", op.Index, op.Type) }

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// quoted renders a predicate's error for the report (DESIGN.md §8.4).
func quoted(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}
