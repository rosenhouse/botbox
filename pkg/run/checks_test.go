package run

import (
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/proxy"
	"github.com/rosenhouse/botbox/pkg/target"
)

// runStart is the instant every check test measures from.
var runStart = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

// at is when in the run something happened, in seconds.
func at(seconds float64) time.Time {
	return runStart.Add(time.Duration(seconds * float64(time.Second)))
}

// checkTarget is the toy as the checks read it: a Widget ready when its
// status.ready matches its spec.count (DESIGN.md §9).
func checkTarget() *target.Target {
	return &target.Target{
		Name:     "toy-widget",
		Primary:  widgetKind,
		Manages:  []schema.GroupVersionKind{configMapKind},
		Timeouts: testTimeouts,
		Ready: func(cr *unstructured.Unstructured) (bool, error) {
			ready, found, err := unstructured.NestedInt64(cr.Object, "status", "ready")
			count, _, _ := unstructured.NestedInt64(cr.Object, "spec", "count")
			return found && ready == count, err
		},
	}
}

// history is the run's object history, which the tests record into.
func history() *observe.Store {
	store := observe.NewStore(observe.Options{
		Namespace: "botbox-run-test",
		Primary:   widgetKind,
		Manages:   []schema.GroupVersionKind{configMapKind},
	})
	store.MarkBotboxCreated(widgetKind, "widget") // botbox creates the CR, so it is never managed.
	return store
}

// recordWidget records the CR with a status.ready of ready.
func recordWidget(store *observe.Store, when time.Time, resourceVersion string, ready int64) {
	cr := &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{"count": int64(1)},
		"status": map[string]any{"ready": ready},
	}}
	cr.SetGroupVersionKind(widgetKind)
	cr.SetName("widget")
	cr.SetResourceVersion(resourceVersion)
	store.Record(widgetKind, cr, when)
}

// recordChild records a ConfigMap the target owns nothing of, which is what
// G3 reports as an orphan.
func recordChild(store *observe.Store, when time.Time, name, resourceVersion string) {
	child := &unstructured.Unstructured{Object: map[string]any{}}
	child.SetGroupVersionKind(configMapKind)
	child.SetName(name)
	child.SetResourceVersion(resourceVersion)
	store.Record(configMapKind, child, when)
}

func appliedOp(index int, opType OpType, when time.Time) AppliedOp {
	return AppliedOp{Op: Op{Index: index, Type: opType}, At: when}
}

// convergedRun created a ready CR and checkpointed on it.
func convergedRun() Input {
	store := history()
	recordWidget(store, at(0.1), "11", 1)
	return Input{
		Target:  checkTarget(),
		Objects: store,
		Timeline: Timeline{
			Ops:         []AppliedOp{appliedOp(0, OpCreate, at(0))},
			Checkpoints: []Checkpoint{{At: at(2.1), Op: 0, Converged: true}},
		},
	}
}

func checked(t *testing.T, in Input) []Violation {
	t.Helper()
	violations, err := Engine{}.Check(in)
	if err != nil {
		t.Fatalf("The checks failed to evaluate: %v", err)
	}
	return violations
}

func ids(violations []Violation) []string {
	found := make([]string, len(violations))
	for i, violation := range violations {
		found[i] = violation.ID
	}
	return found
}

// The teardown's checkpoint follows no settle wait: its Converged says the
// namespace came clean, which is G3's business and not an expired wait.
func TestTheChecksReadTheTeardownCheckpointAsNoSettleWait(t *testing.T) {
	in := convergedRun()
	in.Timeline.Checkpoints = append(in.Timeline.Checkpoints, Checkpoint{At: at(20), Op: Teardown, Converged: false})
	in.Timeline.Deletion = Window{Start: at(10), End: at(20)}

	violations := checked(t, in)

	if len(violations) != 0 {
		t.Errorf("The checks reported %v, want none: a namespace that did not come clean is no expired settle wait.",
			ids(violations))
	}
}

// The engine looks a checkpoint's op up by index, so the adapter carries the
// index the timeline gives rather than the op's position in it.
func TestTheChecksCarryTheOpIndexesTheTimelineGives(t *testing.T) {
	store := history()
	recordWidget(store, at(0.1), "11", 0)
	in := Input{
		Target:  checkTarget(),
		Objects: store,
		Timeline: Timeline{
			Ops:         []AppliedOp{appliedOp(3, OpCreate, at(0)), appliedOp(4, OpUpdate, at(3))},
			Checkpoints: []Checkpoint{{At: at(8), Op: 4, Converged: false}},
		},
	}

	violations := checked(t, in)

	if len(violations) == 0 {
		t.Fatalf("The checks reported nothing, want the expired settle wait after op 4.")
	}
	for _, violation := range violations {
		if !strings.Contains(violation.Statement, "op 4 (update)") {
			t.Errorf("A violation says %q, want it to name op 4, the index the timeline gave.", violation.Statement)
		}
	}
}

func TestTheChecksIgnoreWhatAFaultReachedInto(t *testing.T) {
	store := history()
	recordWidget(store, at(0.1), "11", 0)
	in := Input{
		Target:  checkTarget(),
		Objects: store,
		Timeline: Timeline{
			Ops:         []AppliedOp{appliedOp(0, OpCreate, at(0))},
			Checkpoints: []Checkpoint{{At: at(5), Op: 0, Converged: false}},
			Faults:      []Window{{Start: at(0), End: at(6)}},
		},
	}

	violations := checked(t, in)

	if len(violations) != 0 {
		t.Errorf("The checks reported %v, want none: the fault was active throughout.", ids(violations))
	}
}

// G3 judges the deletion window the teardown opened, which closes after the
// run's last checkpoint.
func TestTheChecksJudgeTheTeardownDeletionWindow(t *testing.T) {
	store := history()
	recordWidget(store, at(0.1), "11", 1)
	recordChild(store, at(0.2), "widget-0", "12")
	deleting := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"count": int64(1)}, "status": map[string]any{"ready": int64(1)},
	}}
	deleting.SetGroupVersionKind(widgetKind)
	deleting.SetName("widget")
	deleting.SetResourceVersion("13")
	store.RecordDeletion(widgetKind, deleting, at(10.1))
	in := Input{
		Target:  checkTarget(),
		Objects: store,
		Timeline: Timeline{
			Ops:         []AppliedOp{appliedOp(0, OpCreate, at(0))},
			Checkpoints: []Checkpoint{{At: at(2.1), Op: 0, Converged: true}},
			Deletion:    Window{Start: at(10), End: at(21)},
		},
	}

	violations := checked(t, in)

	if len(violations) != 1 || violations[0].ID != "G3" {
		t.Fatalf("The checks reported %v, want G3: the orphan outlived the deletion window.", ids(violations))
	}
	if !strings.Contains(violations[0].Statement, "widget-0") {
		t.Errorf("G3 says %q, want it to name the object left behind.", violations[0].Statement)
	}
}

// No window reaches past the teardown, because what changes then is botbox's
// own doing (DESIGN.md §6).
func TestTheChecksStopEveryWindowAtTheTeardown(t *testing.T) {
	in := convergedRun()
	in.Timeline.Deletion = Window{Start: at(3), End: at(13)}
	in.Requests = []proxy.Request{{Start: at(6), Verb: "get", Resource: "configmaps", Name: "widget-0", Status: 200}}

	violations := checked(t, in)

	if len(violations) != 0 {
		t.Errorf("The checks reported %v, want none: the teardown had begun.", ids(violations))
	}
}

func TestTheChecksQuoteWhatTheRunDid(t *testing.T) {
	store := history()
	recordWidget(store, at(0.1), "11", 0)
	in := Input{
		Target:  checkTarget(),
		Objects: store,
		Timeline: Timeline{
			Ops:         []AppliedOp{appliedOp(0, OpCreate, at(0))},
			Checkpoints: []Checkpoint{{At: at(5), Op: 0, Converged: false}},
		},
	}

	violations := checked(t, in)

	if len(violations) == 0 {
		t.Fatalf("The checks reported nothing, want the CR that never became ready.")
	}
	first := violations[0]
	if first.ID != "G4" {
		t.Errorf("The first violation is %s, want G4.", first.ID)
	}
	if !strings.Contains(first.Statement, "widget") {
		t.Errorf("G4 says %q, want it to name the CR.", first.Statement)
	}
	if !strings.Contains(first.Evidence, "toy.botbox/v1/Widget widget") {
		t.Errorf("G4's evidence is %q, want the object version it read.", first.Evidence)
	}
}

// A property that cannot be evaluated is a configuration error, never a
// finding (DESIGN.md §8.4).
func TestTheChecksReportAPropertyThatCannotBeEvaluated(t *testing.T) {
	in := convergedRun()
	in.Target.Properties = []target.Property{{
		ID: "P1",
		Eval: func(*unstructured.Unstructured, []*unstructured.Unstructured) (bool, error) {
			return false, errors.New("no such field: spec.nonesuch")
		},
	}}

	violations, err := Engine{}.Check(in)

	if err == nil {
		t.Fatalf("The checks reported %v, want the evaluation error.", ids(violations))
	}
	if !strings.Contains(err.Error(), "P1") {
		t.Errorf("The checks returned %q, want the property named.", err)
	}
}

// The matrix reads the engine's results themselves, one per check.
func TestEvaluateReturnsOneResultPerCheck(t *testing.T) {
	in := convergedRun()
	in.Target.Properties = []target.Property{{
		ID:   "P1",
		Eval: func(*unstructured.Unstructured, []*unstructured.Unstructured) (bool, error) { return true, nil },
	}}

	results, err := Evaluate(in)

	if err != nil {
		t.Fatalf("Evaluating the run failed: %v", err)
	}
	var got []string
	for _, result := range results {
		got = append(got, result.ID)
	}
	want := []string{"G1", "G2", "G3", "G4", "G5", "G6", "P1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("The engine returned the results %v, want %v.", got, want)
	}
}
