package invariant

import (
	"fmt"
	"time"
)

// maxEvidence caps how much of a request log or a version history one
// violation carries into the report (DESIGN.md §5.7).
const maxEvidence = 20

// quiet is the window [T_settle, T_settle + T_stable] after an op, in which
// DESIGN.md §6 requires the run to have converged and gone still. G1 and G2
// both check it.
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
	timeouts := in.timeouts()
	for i, op := range in.Ops {
		w := quiet{
			op:    op,
			start: op.Time.Add(timeouts.Settle),
			end:   op.Time.Add(timeouts.Settle + timeouts.Stable),
		}
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
