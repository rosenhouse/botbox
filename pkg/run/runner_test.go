package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/launch"
	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/proxy"
	"github.com/rosenhouse/botbox/pkg/target"
)

// fakeHarness records what the Runner did to the run, in order, and answers
// without an API server.
type fakeHarness struct {
	calls []string

	converged bool
	clean     bool
	managed   map[schema.GroupVersionKind][]string
	count     int
	forced    []string
	fail      map[string]error

	// deleteCRDelay holds the CR delete open, and deletedCRAt is when it began.
	// Together they show whether the teardown was stamped before the delete.
	deleteCRDelay time.Duration
	deletedCRAt   time.Time

	// targetGone makes the harness report a target that has stopped.
	targetGone bool
	targetExit error
}

func newFakeHarness() *fakeHarness {
	return &fakeHarness{
		converged: true,
		clean:     true,
		managed:   map[schema.GroupVersionKind][]string{configMapKind: {"widget-0", "widget-1"}},
		fail:      map[string]error{},
	}
}

func (f *fakeHarness) record(call string) error {
	f.calls = append(f.calls, call)
	return f.fail[call]
}

func (f *fakeHarness) namespace() string { return "botbox-run-test" }

func (f *fakeHarness) settle(context.Context) (bool, error) {
	return f.converged, f.record("settle")
}

func (f *fakeHarness) sleep(_ context.Context, d time.Duration) error {
	return f.record("sleep " + d.String())
}

func (f *fakeHarness) restart(context.Context) error { return f.record("restart") }

func (f *fakeHarness) setFaults(specs []proxy.FaultSpec) {
	_ = f.record(fmt.Sprintf("setFaults %d", len(specs)))
}

func (f *fakeHarness) createCR(_ context.Context, obj *unstructured.Unstructured) (string, error) {
	return obj.GetName(), f.record("createCR " + obj.GetName())
}

func (f *fakeHarness) patchCR(_ context.Context, name string, patch map[string]any) error {
	return f.record(fmt.Sprintf("patchCR %s %v", name, patch))
}

// targetStatus answers as a live target unless a test says otherwise.
func (f *fakeHarness) targetStatus() launch.Status {
	if f.targetGone {
		return launch.Status{Exit: f.targetExit}
	}
	return launch.Status{Running: true}
}

func (f *fakeHarness) deleteCR(_ context.Context, name string) error {
	f.deletedCRAt = time.Now()
	time.Sleep(f.deleteCRDelay)
	return f.record("deleteCR " + name)
}

func (f *fakeHarness) awaitCRGone(_ context.Context, name string) error {
	return f.record("awaitCRGone " + name)
}

func (f *fakeHarness) managedObjects(gvk schema.GroupVersionKind) []string {
	_ = f.record("managedObjects " + kindName(gvk))
	return f.managed[gvk]
}

func (f *fakeHarness) deleteManaged(_ context.Context, gvk schema.GroupVersionKind, name string) error {
	return f.record("deleteManaged " + kindName(gvk) + " " + name)
}

func (f *fakeHarness) managedCount() int { return f.count }

func (f *fakeHarness) awaitClean(_ context.Context, within time.Duration) (bool, error) {
	return f.clean, f.record("awaitClean " + within.String())
}

func (f *fakeHarness) forceFinalizers(context.Context) ([]string, error) {
	return f.forced, f.record("forceFinalizers")
}

func (f *fakeHarness) empty(context.Context) error { return f.record("empty") }
func (f *fakeHarness) objects() *observe.Store     { return nil }
func (f *fakeHarness) stop(context.Context) error  { return f.record("stop") }

// requests grows with the run, so that a log sampled late is longer than one
// sampled at a checkpoint.
func (f *fakeHarness) requests() []proxy.Request { return make([]proxy.Request, len(f.calls)) }

func (f *fakeHarness) opCalls() []string       { return f.calls[:f.teardownStart()] }
func (f *fakeHarness) teardownCalls() []string { return f.calls[f.teardownStart():] }

// teardownStart is where the teardown begins: it clears the faults and then
// waits T_stable, which no op does (DESIGN.md §5.5).
func (f *fakeHarness) teardownStart() int {
	for i, call := range f.calls {
		if call == "setFaults 0" && i+1 < len(f.calls) && strings.HasPrefix(f.calls[i+1], "sleep ") {
			return i
		}
	}
	return len(f.calls)
}

// fakeChecker answers each checkpoint from violations and notes, and records
// what it was given.
type fakeChecker struct {
	violations [][]Violation
	notes      [][]string
	err        error
	inputs     []Input
}

func (c *fakeChecker) Check(in Input) (Findings, error) {
	c.inputs = append(c.inputs, in)
	if c.err != nil {
		return Findings{}, c.err
	}
	var found Findings
	n := len(c.inputs) - 1
	if n < len(c.violations) {
		found.Violations = c.violations[n]
	}
	if n < len(c.notes) {
		found.Notes = c.notes[n]
	}
	return found, nil
}

var toyTarget = &target.Target{
	Name:     "toy-widget",
	Primary:  widgetKind,
	Manages:  []schema.GroupVersionKind{configMapKind},
	Timeouts: testTimeouts,
}

func widget(name string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"count": float64(3)}}}
	object.SetGroupVersionKind(widgetKind)
	object.SetName(name)
	return object
}

// sequenceOf numbers the ops, as the format requires.
func sequenceOf(ops ...Op) Sequence {
	for i := range ops {
		ops[i].Index = i
	}
	sequence := Sequence{Seed: 1, Target: toyTarget.Name, Ops: ops}
	return sequence
}

func nth(i int) *int { return &i }

// runFake executes the sequence against a fake harness.
func runFake(t *testing.T, h *fakeHarness, check Checker, sequence Sequence) (Result, error) {
	t.Helper()
	if check == nil {
		check = &fakeChecker{}
	}
	return runSequence(t.Context(), toyTarget, sequence, Options{Check: check}, h)
}

func TestRunAppliesTheOpsInOrder(t *testing.T) {
	h := newFakeHarness()
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}},
		Op{Type: OpSettle},
		Op{Type: OpRestart},
		Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(1)},
		Op{Type: OpRecreate, Obj: widget("widget")},
		Op{Type: OpDelete},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %v, want no violation.", result.Violation)
	}
	want := []string{
		"createCR widget", "settle",
		"patchCR widget map[spec:map[count:5]]", "settle",
		"settle",
		"restart",
		"managedObjects v1/ConfigMap", "deleteManaged v1/ConfigMap widget-1", "settle",
		"deleteCR widget", "awaitCRGone widget", "createCR widget", "settle",
		"deleteCR widget", "settle",
	}
	if got := h.opCalls(); !slices.Equal(got, want) {
		t.Errorf("The run did\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestRunSkipsTheSettleAfterANoSettleOp(t *testing.T) {
	h := newFakeHarness()
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget"), NoSettle: true},
		Op{Type: OpDelete, NoSettle: true},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if got := h.opCalls(); slices.Contains(got, "settle") {
		t.Errorf("The run did %v, want no settle wait after an op that says noSettle.", got)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{Teardown}) {
		t.Errorf("The run checkpointed at the ops %v, want the teardown's alone: no settle ends, no checkpoint.", got)
	}
}

// The checkpoints of DESIGN.md §4: one where a settle wait ends, converged or
// not, and one after the teardown deletion window.
func TestRunCheckpointsWhereTheDesignSaysSo(t *testing.T) {
	h := newFakeHarness()
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}, NoSettle: true},
		Op{Type: OpRestart},
		Op{Type: OpSettle},
		Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(0)},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := []int{0, 3, 4, Teardown}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, want) {
		t.Errorf("The run checkpointed at the ops %v, want %v.", got, want)
	}
}

func TestRunCheckpointsWhenTheSettleExpires(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	sequence := sequenceOf(Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}}, Op{Type: OpSettle})

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{1, Teardown}) {
		t.Errorf("The run checkpointed at the ops %v, want one where the expired settle ended.", got)
	}
	if wait := result.Timeline.Ops[1].Settled; wait == nil || wait.Converged {
		t.Errorf("The settle op recorded %+v, want a wait that did not converge.", wait)
	}
}

func TestRunEndsAtTheFirstViolation(t *testing.T) {
	h := newFakeHarness()
	first := Violation{ID: "G2", Statement: "the target churns"}
	check := &fakeChecker{violations: [][]Violation{
		nil,
		{first, {ID: "G1"}},
		{{ID: "G6"}},
	}}
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpSettle},
		Op{Type: OpSettle},
		Op{Type: OpDelete},
	)

	result, err := runFake(t, h, check, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || *result.Violation != first {
		t.Errorf("The run reported %+v, want the first violation %+v.", result.Violation, first)
	}
	if got := h.opCalls(); slices.Contains(got, "deleteCR widget") {
		t.Errorf("The run did %v, want it to stop at the first violation.", got)
	}
	if len(check.inputs) != 2 {
		t.Errorf("The checks ran %d times, want them to stop at the first violation.", len(check.inputs))
	}
}

func TestRunRecordsG4WhenASettleExpiresWithNoFaultActive(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}, Op{Type: OpDelete})

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Fatalf("The run reported %+v, want a G4 violation.", result.Violation)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{0}) {
		t.Errorf("The run checkpointed at the ops %v, want one where the expired wait ended.", got)
	}
	if got := h.opCalls(); slices.Contains(got, "deleteCR widget") {
		t.Errorf("The run did %v, want it to end at the G4 violation.", got)
	}
}

// The G4 the settle wait found comes before what the checks find at the same
// checkpoint, because a run ends at its first violation.
func TestRunKeepsTheViolationTheExpiredSettleFoundFirst(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	check := &fakeChecker{violations: [][]Violation{{{ID: "P1"}}}}

	result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Errorf("The run reported %+v, want the G4 the expired wait found first.", result.Violation)
	}
}

func TestRunLeavesTheExpiredSettleToTheChecksWhileAFaultIsActive(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Op: nth(3)}}},
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpSettle},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %+v, want no violation: the fault was active throughout.", result.Violation)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{1, 2, Teardown}) {
		t.Errorf("The run checkpointed at the ops %v.", got)
	}
}

func TestRunRecordsG4OnceTheFaultHasExpired(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Op: nth(1)}}},
		Op{Type: OpSettle},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Errorf("The run reported %+v, want a G4 violation: the fault ended at op 1.", result.Violation)
	}
	if got := h.opCalls(); !slices.Contains(got, "setFaults 0") {
		t.Errorf("The run did %v, want the expired fault cleared.", got)
	}
}

func TestRunResolvesDeleteManagedAtExecutionTime(t *testing.T) {
	h := newFakeHarness()
	h.managed[configMapKind] = []string{"widget-0", "widget-1", "widget-2"}
	sequence := sequenceOf(Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(2)})

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if got := h.opCalls(); !slices.Contains(got, "deleteManaged v1/ConfigMap widget-2") {
		t.Errorf("The run did %v, want the third managed ConfigMap deleted.", got)
	}
	if got := result.Timeline.Ops[0].Resolved; got != "widget-2" {
		t.Errorf("The op recorded %q as the object it chose, want widget-2.", got)
	}
}

func TestRunReportsADeleteManagedThatResolvesToNothing(t *testing.T) {
	for _, test := range []struct {
		name     string
		op       Op
		wantCall string
	}{
		{
			name: "an index past the managed objects",
			op:   Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(7)},
		},
		{
			name: "a kind the target does not manage",
			op:   Op{Type: OpDeleteManaged, Kind: "v1/Secret", Nth: nth(0)},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newFakeHarness()

			_, err := runFake(t, h, nil, sequenceOf(test.op))

			if err == nil {
				t.Fatalf("The run succeeded, want a harness error.")
			}
			if !strings.Contains(err.Error(), test.op.Kind) {
				t.Errorf("The run returned %q, want the kind named.", err)
			}
			if !slices.Contains(h.calls, "stop") {
				t.Errorf("The run did %v, want it torn down anyway.", h.calls)
			}
		})
	}
}

func TestRunTearsDownInTheOrderTheDesignGives(t *testing.T) {
	h := newFakeHarness()
	h.forced = []string{"v1/ConfigMap widget-0"}
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")})

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := []string{
		"setFaults 0",
		"sleep " + testTimeouts.Stable.String(),
		"deleteCR widget",
		"awaitClean " + (testTimeouts.Delete + deletionMargin).String(),
		"forceFinalizers",
		"empty",
		"stop",
	}
	if got := h.teardownCalls(); !slices.Equal(got, want) {
		t.Errorf("The teardown did\n\t%v\nwant\n\t%v", got, want)
	}
	if !slices.Equal(result.Timeline.Forced, h.forced) {
		t.Errorf("The run recorded the forced finalizers %v, want %v: a forced removal invalidates G3.",
			result.Timeline.Forced, h.forced)
	}
}

// The teardown's checkpoint lands after the deletion window and before any
// finalizer is forced off, which is the window G3 judges.
func TestRunCheckpointsAfterTheDeletionWindow(t *testing.T) {
	h := newFakeHarness()
	check := &fakeChecker{}
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")})

	result, err := runFake(t, h, check, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	window := result.Timeline.Deletion
	if window.Start.IsZero() || window.End.Before(window.Start) {
		t.Errorf("The deletion window is %+v, want one the teardown opened and closed.", window)
	}
	last := result.Timeline.Checkpoints[len(result.Timeline.Checkpoints)-1]
	if last.Op != Teardown || last.At.Before(window.End) {
		t.Errorf("The last checkpoint is %+v, want one after the deletion window closed at %v.", last, window.End)
	}
	if forced := slices.Index(h.calls, "forceFinalizers"); forced < slices.Index(h.calls, "awaitClean "+(testTimeouts.Delete+deletionMargin).String()) {
		t.Errorf("The teardown forced finalizers before the deletion window: %v", h.calls)
	}
	if got := len(check.inputs); got != 2 {
		t.Errorf("The checks ran %d times, want one per settle wait and one after the deletion window.", got)
	}
}

func TestRunEndsOverTheHarnessObjectLimit(t *testing.T) {
	h := newFakeHarness()
	h.count = defaultMaxManaged + 1
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")})

	_, err := runFake(t, h, nil, sequence)

	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(defaultMaxManaged)) {
		t.Fatalf("The run returned %v, want a harness limit naming %d.", err, defaultMaxManaged)
	}
	if !slices.Contains(h.calls, "stop") {
		t.Errorf("The run did %v, want it torn down anyway.", h.calls)
	}
}

func TestRunGivesEachCheckTheRunSoFar(t *testing.T) {
	h := newFakeHarness()
	check := &fakeChecker{}
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}, Op{Type: OpSettle})

	result, err := runFake(t, h, check, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if len(check.inputs) != 3 {
		t.Fatalf("The checks ran %d times, want 3.", len(check.inputs))
	}
	if check.inputs[0].Target != toyTarget {
		t.Errorf("The check was given the target %v, want the run's.", check.inputs[0].Target)
	}
	if got := len(check.inputs[0].Timeline.Ops); got != 1 {
		t.Errorf("The first check was given %d ops, want the one applied so far.", got)
	}
	if got := len(check.inputs[0].Timeline.Checkpoints); got != 1 {
		t.Errorf("The first check was given %d checkpoints, want the one it is running at.", got)
	}
	if got := len(check.inputs[2].Timeline.Ops); got != 2 {
		t.Errorf("The last check was given %d ops, want both.", got)
	}
	if got := len(result.Timeline.Checkpoints); got != 3 {
		t.Errorf("The run recorded %d checkpoints, want 3.", got)
	}
}

func TestRunNeedsACRBeforeAnOpThatActsOnOne(t *testing.T) {
	for _, opType := range []OpType{OpUpdate, OpDelete} {
		t.Run(string(opType), func(t *testing.T) {
			h := newFakeHarness()
			op := Op{Type: opType, Index: 0}
			if opType == OpUpdate {
				op.Patch = map[string]any{"spec": map[string]any{}}
			}

			_, err := runFake(t, h, nil, Sequence{Seed: 1, Target: toyTarget.Name, Ops: []Op{op}})

			if err == nil || !strings.Contains(err.Error(), "no CR") {
				t.Errorf("The run returned %v, want an error: the sequence creates no CR.", err)
			}
		})
	}
}

func TestRunReportsAFailedOp(t *testing.T) {
	h := newFakeHarness()
	h.fail["restart"] = errors.New("the target will not die")

	_, err := runFake(t, h, nil, sequenceOf(Op{Type: OpRestart}))

	if err == nil || !strings.Contains(err.Error(), "the target will not die") {
		t.Errorf("The run returned %v, want the failure of op 0.", err)
	}
	if !slices.Contains(h.calls, "stop") {
		t.Errorf("The run did %v, want it torn down anyway.", h.calls)
	}
}

func TestRunValidatesTheSequenceAgainstTheTarget(t *testing.T) {
	for _, test := range []struct {
		name     string
		sequence Sequence
		check    Checker
		want     string
	}{
		{
			name:     "a sequence for another target",
			sequence: Sequence{Target: "cert-manager"},
			check:    &fakeChecker{},
			want:     "cert-manager",
		},
		{
			name:     "a malformed op",
			sequence: Sequence{Target: toyTarget.Name, Ops: []Op{{Index: 3, Type: OpSettle}}},
			check:    &fakeChecker{},
			want:     "position",
		},
		{
			name:     "no checker",
			sequence: Sequence{Target: toyTarget.Name},
			want:     "check",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateRun(toyTarget, test.sequence, Options{Dir: "out", Check: test.check})

			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("validateRun returned %v, want an error naming %q.", err, test.want)
			}
		})
	}
}

func TestRunWritesTheSequenceToTheRunDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run-1")
	sequence := readGolden(t)

	if err := writeRunSequence(dir, sequence); err != nil {
		t.Fatalf("Writing the run's sequence failed: %v", err)
	}

	written, err := os.ReadFile(filepath.Join(dir, sequenceFile))
	if err != nil {
		t.Fatalf("Reading back the sequence failed: %v", err)
	}
	want, err := sequence.Marshal()
	if err != nil {
		t.Fatalf("Encoding the sequence failed: %v", err)
	}
	if string(written) != string(want) {
		t.Errorf("The run directory holds\n%s\nwant\n%s", written, want)
	}
}

func checkpointsAt(timeline Timeline) []int {
	var ops []int
	for _, checkpoint := range timeline.Checkpoints {
		ops = append(ops, checkpoint.Op)
	}
	return ops
}

// The Runner knows when each fault op injected its spec and when the fault was
// cleared, which is the window the checks ignore what happened in.
func TestRunRecordsTheWindowEachFaultWasActiveIn(t *testing.T) {
	h := newFakeHarness()
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Op: nth(2)}}},
		Op{Type: OpFault, Fault: &Fault{Action: Action{Drop: true}}},
		Op{Type: OpSettle},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	faults := result.Timeline.Faults
	if len(faults) != 2 {
		t.Fatalf("The run recorded %d fault windows, want one per fault op.", len(faults))
	}
	ops := result.Timeline.Ops
	if !between(faults[0].Start, ops[0].At, ops[1].At) || !between(faults[0].End, ops[1].At, ops[2].At) {
		t.Errorf("The first fault was active %+v, want from op 0 until op 2 cleared it.", faults[0])
	}
	if !between(faults[1].Start, ops[1].At, ops[2].At) || !faults[1].End.After(ops[2].At) {
		t.Errorf("The second fault was active %+v, want from op 1 until the teardown cleared it.", faults[1])
	}
}

// between reports whether the run reached when in [from, to].
func between(when, from, to time.Time) bool { return !when.Before(from) && !when.After(to) }

// The recorded run is what the teardown left: the bug matrix and a report read
// the whole of it, not the part the last checkpoint saw.
func TestRunRecordsTheWholeRunTheTeardownLeftBehind(t *testing.T) {
	h := newFakeHarness()

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if got, want := len(result.Recorded.Requests), len(h.calls); got != want {
		t.Errorf("The run recorded %d requests, want the %d of the whole run: it was sampled after the teardown.",
			got, want)
	}
	if last := result.Recorded.Timeline.Checkpoints; len(last) == 0 || last[len(last)-1].Op != Teardown {
		t.Errorf("The recorded run holds the checkpoints %v, want the teardown's among them.", checkpointsAt(result.Recorded.Timeline))
	}
	if result.Recorded.Target != toyTarget {
		t.Errorf("The recorded run names the target %v, want the run's.", result.Recorded.Target)
	}
}

// §5.5 step 4 waits T_stable before it deletes anything, which §6 judges as
// the run's last quiet window.
func TestRunStampsTheQuietWindowTheTeardownWaited(t *testing.T) {
	h := newFakeHarness()

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	quiet, settled := result.Timeline.Quiet, result.Timeline.Ops[0].Settled
	if !quiet.Start.After(settled.Window.End) {
		t.Errorf("The teardown's window opens at %v, want it after the settle wait ended at %v.",
			quiet.Start, settled.Window.End)
	}
	if quiet.End != result.Timeline.Deletion.Start {
		t.Errorf("The teardown's window closes at %v, want it where the deletion opens, at %v.",
			quiet.End, result.Timeline.Deletion.Start)
	}
}

// The checks name what they could not judge, and the run carries it out, so
// that a reader can tell a skipped check from a passing one (DESIGN.md §6).
func TestRunCarriesTheNotesTheLastCheckpointLeft(t *testing.T) {
	h := newFakeHarness()
	check := &fakeChecker{notes: [][]string{{"G5 is not evaluated for op 0"}, {"G3 is not evaluated for the deletion of widget"}}}

	result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if want := check.notes[1]; !slices.Equal(result.Notes, want) {
		t.Errorf("The run carried the notes %v, want %v: each checkpoint reads the whole run so far.",
			result.Notes, want)
	}
}

// A check that cannot be evaluated is a configuration error, so the run ends
// as a harness error rather than as a finding (DESIGN.md §11).
func TestRunEndsWhenACheckCannotBeEvaluated(t *testing.T) {
	h := newFakeHarness()
	check := &fakeChecker{err: errors.New("no such field: spec.nonesuch")}

	result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err == nil || !strings.Contains(err.Error(), "spec.nonesuch") {
		t.Fatalf("The run returned %v, want the evaluation error.", err)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %+v, want no finding: the check did not evaluate.", result.Violation)
	}
	if !slices.Contains(h.calls, "stop") {
		t.Errorf("The run did %v, want it torn down anyway.", h.calls)
	}
}

// G3's window opens when the Observer records the deletion, a moment after the
// teardown asked for it, so the teardown holds the namespace open past
// T_delete.
func TestRunHoldsTheDeletionWindowOpenPastTDelete(t *testing.T) {
	h := newFakeHarness()
	h.clean = false

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := "awaitClean " + (testTimeouts.Delete + deletionMargin).String()
	if got := h.teardownCalls(); !slices.Contains(got, want) {
		t.Errorf("The teardown did %v, want %q.", got, want)
	}
	if window := result.Timeline.Deletion; !window.End.After(window.Start) {
		t.Errorf("The deletion window is %+v, want one the teardown held open.", window)
	}
}

// TestTheTeardownIsStampedBeforeItChangesAnything pins Timeline.Deletion.Start
// to the instant before the CR delete. The invariants take it as the moment
// botbox became the one changing the namespace (DESIGN.md §6), so a stamp taken
// after the delete returns leaves a window in which the CR botbox is deleting is
// judged against the target.
func TestTheTeardownIsStampedBeforeItChangesAnything(t *testing.T) {
	h := &fakeHarness{converged: true, clean: true, deleteCRDelay: 50 * time.Millisecond}
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")})

	result, err := runSequence(t.Context(), toyTarget, sequence, Options{Check: &fakeChecker{}}, h)
	if err != nil {
		t.Fatalf("the run returned an error: %v", err)
	}
	if h.deletedCRAt.IsZero() {
		t.Fatal("the teardown never deleted the CR.")
	}
	if start := result.Timeline.Deletion.Start; start.After(h.deletedCRAt) {
		t.Errorf("The teardown is stamped %v after the CR delete began; it must be stamped before it.",
			start.Sub(h.deletedCRAt))
	}
}

// TestASettleExpiryWithADeadTargetIsAHarnessError pins the difference between
// the target failing and the harness failing. A target that is gone cannot
// converge, so reporting G4 would accuse a controller of a fault that is ours:
// a port collision reads exactly this way (DESIGN.md §5.1).
func TestASettleExpiryWithADeadTargetIsAHarnessError(t *testing.T) {
	h := &fakeHarness{clean: true, targetGone: true, targetExit: errors.New("exit status 1")}
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")})

	result, err := runSequence(t.Context(), toyTarget, sequence, Options{Check: &fakeChecker{}, Dir: "out"}, h)
	if err == nil {
		t.Fatal("The run reported no error although the target had stopped.")
	}
	for _, want := range []string{"no longer running", "exit status 1", "target.log"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("The error is %q, which does not mention %q.", err, want)
		}
	}
	if result.Violation != nil {
		t.Errorf("The run reported %+v against the target; a dead target is the harness's failure.", result.Violation)
	}
}
