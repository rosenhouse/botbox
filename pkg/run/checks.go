package run

import (
	"fmt"
	"strings"
	"time"

	"github.com/rosenhouse/botbox/pkg/invariant"
)

// Engine is the invariant engine as a Checker (DESIGN.md §5.6): the generic
// invariants of §6 and the properties the target declares.
type Engine struct{}

var _ Checker = Engine{}

// Check returns every violation the engine found, in its order of checks. A
// run ends at the first of them (DESIGN.md §5.5).
func (Engine) Check(in Input) ([]Violation, error) {
	results, err := Evaluate(in)
	if err != nil {
		return nil, err
	}
	var found []Violation
	for _, result := range results {
		for _, violation := range result.Violations {
			found = append(found, Violation{
				ID:        violation.ID,
				Statement: violation.Statement,
				Evidence:  evidence(violation),
			})
		}
	}
	return found, nil
}

// Evaluate runs every check over the run so far and returns what each one
// found. The bug matrix reads the results; a run reads the violations.
func Evaluate(in Input) ([]invariant.Result, error) {
	return invariant.Evaluate(invariant.Input{
		Target:      in.Target,
		Requests:    in.Requests,
		History:     in.Objects,
		Ops:         engineOps(in.Timeline.Ops),
		Checkpoints: engineCheckpoints(in.Timeline.Checkpoints),
		Faults:      engineFaults(in.Timeline.Faults),
		Teardown:    in.Timeline.Deletion.Start,
		// The deletion window is the last of the run the Observer watched.
		// It is zero until the teardown closes it, and the engine then
		// evaluates at the last checkpoint.
		End: in.Timeline.Deletion.End,
	})
}

// engineOps carries each op's index, which is what a checkpoint names and not
// the op's position in the timeline.
func engineOps(applied []AppliedOp) []invariant.Op {
	ops := make([]invariant.Op, len(applied))
	for i, op := range applied {
		ops[i] = invariant.Op{Index: op.Op.Index, Type: invariant.OpType(op.Op.Type), Time: op.At}
	}
	return ops
}

func engineCheckpoints(checkpoints []Checkpoint) []invariant.Checkpoint {
	engine := make([]invariant.Checkpoint, len(checkpoints))
	for i, checkpoint := range checkpoints {
		engine[i] = invariant.Checkpoint{Op: checkpoint.Op, Time: checkpoint.At, Settle: settleResult(checkpoint)}
	}
	return engine
}

// settleResult reads a checkpoint's Converged. The teardown's checkpoint
// follows no settle wait: its Converged says the namespace came clean, which
// is what G3 judges and never an expired wait.
func settleResult(checkpoint Checkpoint) invariant.SettleResult {
	switch {
	case checkpoint.Op == Teardown:
		return invariant.NoSettle
	case checkpoint.Converged:
		return invariant.Converged
	default:
		return invariant.Expired
	}
}

func engineFaults(windows []Window) []invariant.FaultWindow {
	faults := make([]invariant.FaultWindow, len(windows))
	for i, window := range windows {
		faults[i] = invariant.FaultWindow{Start: window.Start, End: window.End}
	}
	return faults
}

// evidence is the line the CLI prints under a violation. The whole request
// log and version history stay in the run directory (DESIGN.md §5.7).
func evidence(violation invariant.Violation) string {
	quoted := []string{"at " + violation.At.Format(time.RFC3339Nano)}
	if requests := violation.Requests; len(requests) > 0 {
		quoted = append(quoted, fmt.Sprintf("%d requests, the first %s %s %d",
			len(requests), requests[0].Verb, requests[0].Path, requests[0].Status))
	}
	if versions := violation.Versions; len(versions) > 0 {
		quoted = append(quoted, fmt.Sprintf("%d versions, the first %s %s",
			len(versions), kindName(versions[0].GVK), versions[0].Name))
	}
	return strings.Join(quoted, "; ")
}
