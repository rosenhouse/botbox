package invariant

import (
	"fmt"
	"slices"
	"time"

	"github.com/rosenhouse/botbox/pkg/observe"
)

// Convergence is G4: within T_settle after any spec change, and within
// T_settle after faults stop, the target's Ready predicate holds
// (DESIGN.md §6). A settle wait that expired while no fault was active is a
// violation of its own (§5.5).
func Convergence(in Input) (Result, error) {
	out := Result{ID: "G4"}
	for _, from := range in.convergeAnchors() {
		deadline := from.at.Add(in.timeouts().Settle)
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
		if ready && err == nil {
			continue
		}
		managed := seen.managed(in)
		out.violate(Violation{
			Statement: fmt.Sprintf("the CR %s was not ready %s after %s%s",
				cr.Name, in.timeouts().Settle, from.what, quoted(err)),
			At:       deadline,
			Versions: Readiness(upTo(in.History.History(cr.Key), deadline), managed),
			Managed:  counted(managed),
		})
	}
	out.reportExpiredWaits(in)
	return out, nil
}

// anchor is a moment the target must have converged within T_settle of.
type anchor struct {
	at   time.Time
	what string
}

func (in Input) convergeAnchors() []anchor {
	var anchors []anchor
	for _, op := range in.Ops {
		if op.Type.changesSpec() {
			anchors = append(anchors, anchor{at: op.Time, what: describe(op)})
		}
	}
	for _, fault := range in.Faults {
		if !fault.End.IsZero() {
			anchors = append(anchors, anchor{at: fault.End, what: "the fault stopped"})
		}
	}
	slices.SortFunc(anchors, func(a, b anchor) int { return a.at.Compare(b.at) })
	return anchors
}

// respecified reports whether a later op changed the spec inside the window,
// which hands the window to that op.
func (in Input) respecified(from, to time.Time) bool {
	return slices.ContainsFunc(in.Ops, func(op Op) bool {
		return op.Type.changesSpec() && op.Time.After(from) && op.Time.Before(to)
	})
}

// reportExpiredWaits records the settle waits that ran out while the target
// had no fault to blame (DESIGN.md §5.5).
func (out *Result) reportExpiredWaits(in Input) {
	for _, checkpoint := range in.Checkpoints {
		if checkpoint.Settle != Expired {
			continue
		}
		started := in.waitStart(checkpoint)
		if in.faulted(started, checkpoint.Time) {
			continue
		}
		managed := in.stateAt(checkpoint.Time).managed(in)
		out.violate(Violation{
			Statement: fmt.Sprintf("the settle wait after %s expired with no fault active",
				in.describeOp(checkpoint.Op)),
			At:       checkpoint.Time,
			Versions: Readiness(in.versionsIn(started, checkpoint.Time), managed),
			Managed:  counted(managed),
		})
	}
}

// waitStart is when the settle wait a checkpoint ends began: the op it
// follows, or T_settle back when the Input carries no such op.
func (in Input) waitStart(checkpoint Checkpoint) time.Time {
	if op, found := in.op(checkpoint.Op); found {
		return op.Time
	}
	return checkpoint.Time.Add(-in.timeouts().Settle)
}

func (in Input) describeOp(index int) string {
	if op, found := in.op(index); found {
		return describe(op)
	}
	return "teardown"
}

// upTo drops the versions recorded after the verdict, which a report of what
// the run looked like at that instant cannot quote.
func upTo(versions []observe.Version, t time.Time) []observe.Version {
	return slices.DeleteFunc(slices.Clone(versions), func(v observe.Version) bool { return v.Time.After(t) })
}

// counted is the count a violation carries, which is never nil where a check
// asked: a run in which the target managed nothing is the finding.
func counted(managed []observe.Version) *int {
	n := len(managed)
	return &n
}

func describe(op Op) string { return fmt.Sprintf("op %d (%s)", op.Index, op.Type) }

// quoted renders a predicate's error for the report (DESIGN.md §8.4).
func quoted(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}
