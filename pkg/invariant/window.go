package invariant

import (
	"fmt"
	"time"
)

// maxEvidence caps how much of a request log or a version history one
// violation carries into the report (DESIGN.md §5.7).
const maxEvidence = 20

// quiet is the T_stable that follows an op's settle wait, in which DESIGN.md
// §6 requires the run to have gone still. G1 and G2 both check it.
type quiet struct {
	op         Op
	start, end time.Time
}

func (w quiet) String() string {
	return fmt.Sprintf("the quiet window after op %d (%s)", w.op.Index, w.op.Type)
}

// quietWindows returns the quiet window of every op the run observed to its
// end under an unchanged spec and no fault.
func (in Input) quietWindows() []quiet {
	var windows []quiet
	stable := in.timeouts().Stable
	for i, op := range in.Ops {
		settled, ok := in.settleEnd(op.Index)
		if !ok {
			continue
		}
		w := quiet{op: op, start: settled, end: settled.Add(stable)}
		if !in.observed(w.end) || in.faulted(op.Time, w.end) || in.tornDown(w.end) {
			continue
		}
		if i+1 < len(in.Ops) && in.Ops[i+1].Time.Before(w.end) {
			continue // Another op reopened the window, so it is that op's.
		}
		windows = append(windows, w)
	}
	return windows
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

// excerpt caps a list of evidence at its first maxEvidence entries.
func excerpt[T any](evidence []T) []T {
	if len(evidence) > maxEvidence {
		return evidence[:maxEvidence]
	}
	return evidence
}

// recent caps a timeline of evidence at the maxEvidence entries nearest the
// violation, which are its last.
func recent[T any](evidence []T) []T {
	if len(evidence) > maxEvidence {
		return evidence[len(evidence)-maxEvidence:]
	}
	return evidence
}
