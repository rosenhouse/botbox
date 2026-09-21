package invariant

import (
	"fmt"
	"time"
)

// MaxEvidence caps how much of a request log or a version history one
// violation carries into the report (DESIGN.md §5.7).
const MaxEvidence = 20

// quiet is a stretch in which DESIGN.md §6 requires the run to have gone
// still: the T_stable that follows an op's settle wait, and the T_stable the
// teardown waits before it deletes. G1 and G2 both check it.
type quiet struct {
	what       string
	start, end time.Time
}

func (w quiet) String() string { return w.what }

// quietWindows returns every window the run observed to its end under an
// unchanged spec and no fault: one per op that settled, and the teardown's.
func (in Input) quietWindows() []quiet {
	var windows []quiet
	stable := in.timeouts().Stable
	for i, op := range in.Ops {
		settled, ok := in.settleEnd(op.Index)
		if !ok {
			continue
		}
		w := quiet{
			what:  fmt.Sprintf("the quiet window after op %d (%s)", op.Index, op.Type),
			start: settled,
			end:   settled.Add(stable),
		}
		if !in.observed(w.end) || in.faulted(op.Time, w.end) || in.tornDown(w.end) {
			continue
		}
		if i+1 < len(in.Ops) && in.Ops[i+1].Time.Before(w.end) {
			continue // Another op reopened the window, so it is that op's.
		}
		windows = append(windows, w)
	}
	if w, ok := in.teardownWindow(); ok {
		windows = append(windows, w)
	}
	return windows
}

// teardownWindow is the T_stable the teardown waits before it deletes
// anything, which §5.5 step 4 makes a quiet window of its own: a run whose
// last op never settles has no other. The teardown clears every fault as the
// window opens, so a fault it cleared did not reach into it.
func (in Input) teardownWindow() (quiet, bool) {
	w := quiet{what: "the quiet window the teardown waited before deleting", start: in.Quiet, end: in.Teardown}
	if w.start.IsZero() || w.end.IsZero() || !in.observed(w.end) || in.faulted(w.start, w.end) {
		return quiet{}, false
	}
	return w, true
}

// settleEnd is when the op's settle wait ended, on convergence or on T_settle,
// which is where §6's quiet window opens. An op the Runner did not settle
// after, or whose wait the run did not reach the end of, has no window.
func (in Input) settleEnd(op int) (time.Time, bool) {
	for _, checkpoint := range in.Checkpoints {
		if checkpoint.Op == op && checkpoint.Settle != NoSettle {
			return checkpoint.Time, true
		}
	}
	return time.Time{}, false
}

// tornDown reports whether botbox had started emptying the namespace by t, so
// that what happened then is no longer the target's doing.
func (in Input) tornDown(t time.Time) bool {
	return !in.Teardown.IsZero() && t.After(in.Teardown)
}

// excerpt caps a list of evidence at its first MaxEvidence entries.
func excerpt[T any](evidence []T) []T {
	if len(evidence) > MaxEvidence {
		return evidence[:MaxEvidence]
	}
	return evidence
}

// Recent caps a timeline of evidence at the MaxEvidence entries nearest the
// violation, which are its last. The Runner bounds the evidence of a violation
// it raises itself the same way (DESIGN.md §5.7).
func Recent[T any](evidence []T) []T {
	if len(evidence) > MaxEvidence {
		return evidence[len(evidence)-MaxEvidence:]
	}
	return evidence
}
