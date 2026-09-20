package invariant_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rosenhouse/botbox/pkg/invariant"
	"github.com/rosenhouse/botbox/pkg/target"
)

func property(when target.PropertyWhen, eval target.PropertyFunc) target.Property {
	return target.Property{
		ID:          "P1",
		Description: "status.ready never exceeds the number of ConfigMaps present.",
		Eval:        eval,
		When:        when,
	}
}

// claimed is a run whose CR reports two children it does not have from 1s to
// 3s, which only an event-by-event check sees.
func claimed(when target.PropertyWhen) invariant.Input {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		record(3*time.Second, child("w-0", "11"), child("w-1", "12")).
		checkpoint(4*time.Second, invariant.Converged).
		through(8 * time.Second)
	in.Target.Properties = []target.Property{property(when, readyCountsChildren)}
	return in
}

func TestPropertyPassesAtACheckpointTheTargetHealedBefore(t *testing.T) {
	in := claimed(target.Checkpoint)

	silent(t, invariant.Property(in.Target.Properties[0]), in)
}

func TestPropertyFiresOnAnIntermediateStateWhenItIsEvaluatedAlways(t *testing.T) {
	in := claimed(target.Always)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if violation.ID != "P1" {
		t.Errorf("The violation is %q, want P1.", violation.ID)
	}
	if !violation.At.Equal(at(time.Second)) {
		t.Errorf("The violation is timestamped %v, want the event at 1s.", violation.At)
	}
	if !strings.Contains(violation.Statement, "never exceeds") {
		t.Errorf("The statement is %q, want it to carry the property's description.", violation.Statement)
	}
	if len(violation.Versions) != 1 || violation.Versions[0].GVK != widgetGVK {
		t.Fatalf("The evidence holds %v, want the state the property read.", violation.Versions)
	}
}

func TestPropertyFiresAtACheckpointThatStillBreaksIt(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		checkpoint(4*time.Second, invariant.Converged).
		through(8 * time.Second)
	in.Target.Properties = []target.Property{property(target.Checkpoint, readyCountsChildren)}

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if !violation.At.Equal(at(4 * time.Second)) {
		t.Errorf("The violation is timestamped %v, want the checkpoint at 4s.", violation.At)
	}
}

func TestPropertyEvaluatedAtTheEndReadsTheLastCheckpointOnly(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		checkpoint(4*time.Second, invariant.Converged).
		record(5*time.Second, child("w-0", "11"), child("w-1", "12")).
		checkpoint(6*time.Second, invariant.Converged).
		through(8 * time.Second)
	in.Target.Properties = []target.Property{property(target.End, readyCountsChildren)}

	silent(t, invariant.Property(in.Target.Properties[0]), in)
}

func TestPropertyReadsTheManagedObjectsOnly(t *testing.T) {
	fixture := child("shared", "9", orphaned)
	var seen []string
	in := newRun().
		fixture(fixture).
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1)), fixture, child("w-0", "11")).
		checkpoint(4*time.Second, invariant.Converged).
		through(8 * time.Second)
	in.Target.Properties = []target.Property{property(target.Checkpoint,
		func(_ *unstructured.Unstructured, managed []*unstructured.Unstructured) (bool, error) {
			for _, object := range managed {
				seen = append(seen, object.GetName())
			}
			return true, nil
		})}

	silent(t, invariant.Property(in.Target.Properties[0]), in)

	if len(seen) != 1 || seen[0] != "w-0" {
		t.Fatalf("The property read %v, want the managed ConfigMap alone.", seen)
	}
}

func TestPropertyReturnsAnEvaluationErrorAsAConfigurationError(t *testing.T) {
	in := claimed(target.Checkpoint)
	broken := errors.New("no such key: spec")
	in.Target.Properties = []target.Property{property(target.Checkpoint,
		func(*unstructured.Unstructured, []*unstructured.Unstructured) (bool, error) { return false, broken })}

	result, err := invariant.Property(in.Target.Properties[0])(in)

	if !errors.Is(err, broken) {
		t.Fatalf("Property returned %v, want the evaluation error.", err)
	}
	if len(result.Violations) > 0 {
		t.Errorf("Property reported %v, want no finding for a configuration error.", statements(result))
	}
}

func TestEvaluateSurvivesARunWithNoHistory(t *testing.T) {
	in := invariant.Input{
		Target:      toyTarget(),
		Ops:         []invariant.Op{{Index: 0, Type: invariant.OpCreate, Time: at(0)}},
		Checkpoints: []invariant.Checkpoint{{Op: 0, Time: at(5 * time.Second), Settle: invariant.Expired}},
		End:         at(5 * time.Second),
	}

	results, err := invariant.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate returned an error: %v", err)
	}
	if fired := firingIDs(results); len(fired) != 1 || fired[0] != "G4" {
		t.Fatalf("Evaluate reported %v, want the expired settle wait alone.", fired)
	}
}

// The teardown deletes the children before the CR's finalizer clears, so a
// property evaluated on every event sees a CR that outlived them
// (DESIGN.md §5.5, step 4).
func TestPropertyEvaluatedAlwaysIgnoresTheTeardownsOwnEvents(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1)), child("w-0", "11"), child("w-1", "12")).
		teardown(4*time.Second).
		remove(4100*time.Millisecond, child("w-0", "13"), child("w-1", "14")).
		through(6 * time.Second)
	in.Target.Properties = []target.Property{property(target.Always, readyCountsChildren)}

	silent(t, invariant.Property(in.Target.Properties[0]), in)
}

func TestPropertyEvaluatedAlwaysFiresOnAnEventTheTeardownCameAfter(t *testing.T) {
	in := claimed(target.Always)
	in.Teardown = at(6 * time.Second)

	fired(t, invariant.Property(in.Target.Properties[0]), in)
}
