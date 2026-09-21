package invariant

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/observe"
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

// Readiness is what a violation of the CR's readiness quotes: the state of the
// objects the target managed at the verdict, and the versions nearest it
// beside them. Neither side takes more than half the bound where the other can
// use the rest (D39).
func Readiness(history, managed []observe.Version) []observe.Version {
	children := sample(managed)
	// A managed object's state is quoted once. Where the history holds that
	// same version, the state is the row to keep, and the object's earlier
	// versions stay.
	var rest []observe.Version
	for _, v := range history {
		if !slices.ContainsFunc(children, func(child observe.Version) bool {
			return child.Key == v.Key && child.ResourceVersion == v.ResourceVersion
		}) {
			rest = append(rest, v)
		}
	}
	keepRest, keepChildren := fairShare(len(rest), len(children))
	quoted := append(rest[len(rest)-keepRest:], children[:keepChildren]...)
	slices.SortStableFunc(quoted, byTime)
	return quoted
}

// byTime orders evidence the way the report's table reads it, down the run.
func byTime(a, b observe.Version) int { return a.Time.Compare(b.Time) }

// fairShare splits the evidence bound between two lists, giving each at most
// half of it where the other can use the rest.
func fairShare(history, children int) (int, int) {
	half := MaxEvidence / 2
	switch {
	case history+children <= MaxEvidence:
		return history, children
	case history <= half:
		return history, MaxEvidence - history
	case children <= half:
		return MaxEvidence - children, children
	default:
		return half, MaxEvidence - half
	}
}

// sample orders the objects the target managed for quoting: one kind at a time,
// and within a kind the ones recorded nearest the verdict first (D39).
func sample(managed []observe.Version) []observe.Version {
	kinds := map[schema.GroupVersionKind][]observe.Version{}
	for _, v := range managed {
		kinds[v.GVK] = append(kinds[v.GVK], v)
	}
	order := slices.SortedFunc(maps.Keys(kinds), func(a, b schema.GroupVersionKind) int {
		return strings.Compare(kindName(a), kindName(b))
	})
	for _, gvk := range order {
		slices.SortStableFunc(kinds[gvk], func(a, b observe.Version) int { return b.Time.Compare(a.Time) })
	}
	var sampled []observe.Version
	for i := 0; len(sampled) < len(managed); i++ {
		for _, gvk := range order {
			if i < len(kinds[gvk]) {
				sampled = append(sampled, kinds[gvk][i])
			}
		}
	}
	return sampled
}
