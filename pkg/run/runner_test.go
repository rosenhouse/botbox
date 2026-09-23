package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/cluster"
	"github.com/rosenhouse/botbox/pkg/invariant"
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
	// held are the faults the proxy holds. added counts the faults it was
	// given, which is the ID of the next.
	held  []proxy.FaultID
	added int
	// faulting makes the harness answer as a proxy that applies every fault
	// the moment it is given it, and retires none. faultingInWait has it
	// first apply them once a wait begins, as the target's requests do.
	faulting       bool
	faultingInWait bool
	applied        map[proxy.FaultID]time.Time
	// store is the version history the checks and the report read.
	store *observe.Store
	// logged are the requests the harness answers with before the run's own,
	// so that a test can drive the log past what a violation quotes.
	logged []proxy.Request
	// applying is what the harness says the proxy did with each fault, by
	// ID. Its zero value is a fault that matched nothing.
	applying []proxy.FaultWindow

	managed map[schema.GroupVersionKind][]string
	count   int
	forced  []string
	fail    map[string]error
	// unresolved is what the collector reports once the harness has stopped.
	unresolved []cluster.Unresolved

	// deleteCRDelay holds the CR delete open, and deletedCRAt is when it began.
	// Together they show whether the teardown was stamped before the delete.
	deleteCRDelay time.Duration
	deletedCRAt   time.Time

	// targetGone makes the harness report a target that has stopped, and
	// stopsAfter is the call it stops at.
	targetGone bool
	stopsAfter string
	targetExit error

	// owed is what each settle wait was told the target owed, and
	// applyingInWait replaces applying once a wait begins.
	owed           []time.Time
	applyingInWait []proxy.FaultWindow
	// retiresInWait has the proxy retire the first fault this long into a
	// wait, which then runs in real time until the target owes nothing.
	retiresInWait time.Duration
	// cancel ends the run's context at the call cancelsAfter names.
	cancel       context.CancelFunc
	cancelsAfter string
}

func newFakeHarness() *fakeHarness {
	return &fakeHarness{
		converged: true,
		clean:     true,
		store:     observe.NewStore(observe.Options{Namespace: fakeNamespace, Manages: []schema.GroupVersionKind{configMapKind}}),
		managed:   map[schema.GroupVersionKind][]string{configMapKind: {"widget-0", "widget-1"}},
		fail:      map[string]error{},
		applied:   map[proxy.FaultID]time.Time{},
	}
}

func (f *fakeHarness) record(call string) error {
	f.calls = append(f.calls, call)
	if call == f.stopsAfter {
		f.targetGone = true
	}
	if call == f.cancelsAfter {
		f.cancel()
	}
	return f.fail[call]
}

const fakeNamespace = "botbox-run-test"

func (f *fakeHarness) namespace() string { return fakeNamespace }

func (f *fakeHarness) settle(ctx context.Context, owed func() time.Time) (bool, error) {
	if f.applyingInWait != nil {
		f.applying = f.applyingInWait
	}
	if f.retiresInWait > 0 {
		time.Sleep(f.retiresInWait)
		f.applying[0].Retired = time.Now()
		time.Sleep(time.Until(owed()))
	}
	f.owed = append(f.owed, owed())
	if f.faultingInWait {
		f.apply()
	}
	if err := errors.Join(f.record("settle"), ctx.Err()); err != nil {
		return false, err
	}
	return f.converged, nil
}

func (f *fakeHarness) sleep(_ context.Context, d time.Duration) error {
	return f.record("sleep " + d.String())
}

func (f *fakeHarness) restart(context.Context) error { return f.record("restart") }

func (f *fakeHarness) addFault(proxy.FaultSpec) proxy.FaultID {
	id := proxy.FaultID(f.added)
	f.added++
	f.held = append(f.held, id)
	if f.faulting {
		f.apply()
	}
	_ = f.record(fmt.Sprintf("addFault %d", id))
	return id
}

func (f *fakeHarness) removeFault(id proxy.FaultID) {
	f.held = slices.DeleteFunc(f.held, func(held proxy.FaultID) bool { return held == id })
	_ = f.record(fmt.Sprintf("removeFault %d", id))
}

func (f *fakeHarness) clearFaults() {
	f.held = nil
	_ = f.record("clearFaults")
}

// apply has the proxy apply each fault it holds, from now if it has not yet.
func (f *fakeHarness) apply() {
	for _, id := range f.held {
		if _, applied := f.applied[id]; !applied {
			f.applied[id] = time.Now()
		}
	}
}

// faultWindow answers as the proxy does. A test either says what the proxy
// did with each fault, or has it apply every fault as it is given.
func (f *fakeHarness) faultWindow(id proxy.FaultID) proxy.FaultWindow {
	var window proxy.FaultWindow
	if !slices.Contains(f.held, id) {
		return window
	}
	if int(id) < len(f.applying) {
		window = f.applying[id]
	}
	if first, applied := f.applied[id]; applied && window.First.IsZero() {
		window.First = first
	}
	return window
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
func (f *fakeHarness) objects() *observe.Store     { return f.store }
func (f *fakeHarness) stop(context.Context) error  { return f.record("stop") }

func (f *fakeHarness) unresolvedOwners() []cluster.Unresolved {
	if !slices.Contains(f.calls, "stop") {
		return nil
	}
	return f.unresolved
}

// requests grows with the run, so that a log sampled late is longer than one
// sampled at a checkpoint. Each entry the run adds names the call it followed.
func (f *fakeHarness) requests() []proxy.Request {
	log := slices.Clone(f.logged)
	for _, call := range f.calls {
		log = append(log, proxy.Request{Path: call})
	}
	return log
}

// recordCR records a version of the CR, as the Observer would.
func (f *fakeHarness) recordCR(name, resourceVersion string) {
	object := widget(name)
	object.SetNamespace(fakeNamespace)
	object.SetResourceVersion(resourceVersion)
	f.store.Record(widgetKind, object, time.Now())
}

// recordChild records a managed object of the CR, as the Observer would.
func (f *fakeHarness) recordChild(name, resourceVersion string) {
	object := &unstructured.Unstructured{Object: map[string]any{}}
	object.SetGroupVersionKind(configMapKind)
	object.SetNamespace(fakeNamespace)
	object.SetName(name)
	object.SetResourceVersion(resourceVersion)
	f.store.Record(configMapKind, object, time.Now())
}

func (f *fakeHarness) opCalls() []string       { return f.calls[:f.teardownStart()] }
func (f *fakeHarness) teardownCalls() []string { return f.calls[f.teardownStart():] }

// teardownStart is where the teardown begins: it clears the faults.
func (f *fakeHarness) teardownStart() int {
	if i := slices.Index(f.calls, "clearFaults"); i >= 0 {
		return i
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
	h.faulting = true
	sequence := sequenceOf(Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}}, Op{Type: OpSettle})

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{1, Recovery}) {
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
	if result.Violation == nil || result.Violation.ID != first.ID {
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

// A G4 the expired wait raised carries the evidence a check's G4 would: the
// CR's version history and the request log as the expiry left it
// (DESIGN.md §5.7).
func TestTheG4OfAnExpiredWaitCarriesTheEvidence(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	for i := range 25 {
		h.recordCR("widget", strconv.Itoa(10+i))
		h.logged = append(h.logged, proxy.Request{Path: "before-" + strconv.Itoa(i)})
	}

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Fatalf("The run reported %+v, want a G4 violation.", result.Violation)
	}
	if wait := result.Timeline.Ops[0].Settled; wait == nil || !result.Violation.At.Equal(wait.Window.End) {
		t.Errorf("The violation is stamped %v, want the instant the settle wait expired.", result.Violation.At)
	}
	versions := result.Violation.Versions
	if len(versions) != 20 {
		t.Fatalf("The violation carries %d versions of the CR, want the 20 a report quotes.", len(versions))
	}
	if last := versions[19].ResourceVersion; last != "34" {
		t.Errorf("The versions end at resourceVersion %s, want the CR's latest, 34.", last)
	}
	requests := result.Violation.Requests
	if len(requests) != 20 {
		t.Fatalf("The violation carries %d requests, want the 20 a report quotes.", len(requests))
	}
	if last := requests[19].Path; last != "settle" {
		t.Errorf("The requests end at %q, want the settle the wait expired in.", last)
	}
	if first := requests[0].Path; first != "before-7" {
		t.Errorf("The requests open at %q, want the 20 nearest the expiry, from before-7.", first)
	}
	// A report cannot say what the bound left out unless the violation says how
	// much it chose from (#22).
	if want := 25; result.Violation.VersionsTotal != want {
		t.Errorf("The violation says it chose from %d versions, want the %d the run recorded.", result.Violation.VersionsTotal, want)
	}
	if total := result.Violation.RequestsTotal; total <= len(requests) {
		t.Errorf("The violation says it chose from %d requests and quotes %d of them.", total, len(requests))
	}
}

// A message that prints a violation wants the finding. The evidence behind one
// runs to thousands of characters, and the recordings hold it.
func TestAViolationPrintsAsOneLine(t *testing.T) {
	carrying := Violation{
		ID: "G4", Statement: "the target converges", Evidence: "the settle wait expired",
		Versions: []observe.Version{{ResourceVersion: "12"}},
	}
	bare := Violation{ID: "G1", Statement: "the target falls quiet"}

	if got, want := fmt.Sprintf("%+v", carrying), "G4 the target converges; the settle wait expired"; got != want {
		t.Errorf("A violation prints as %q, want %q.", got, want)
	}
	if got, want := fmt.Sprintf("%v", bare), "G1 the target falls quiet"; got != want {
		t.Errorf("A violation carrying no evidence prints as %q, want %q.", got, want)
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

func TestRunExcusesEveryWaitAFaultWasActiveThrough(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.faulting = true
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Op: nth(3)}}},
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpSettle},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	// The fault excused every op. The target never recovered once the
	// teardown cleared it.
	if result.Violation == nil || !strings.Contains(result.Violation.Statement, "after the last fault stopped") {
		t.Errorf("The run reported %+v, want only the G4 of the wait after the last fault stopped.", result.Violation)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{1, 2, Recovery}) {
		t.Errorf("The run checkpointed at the ops %v.", got)
	}
}

// A fault excuses the target over the window the proxy applied it in, so a
// fault whose own trigger has run out excuses nothing after it
// (DESIGN.md §5.2, D36).
func TestRunStopsExcusingTheTargetWhereTheProxyRetiredTheFault(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	applied, retired := time.Now().Add(-10*time.Second), time.Now().Add(-9*time.Second)
	h.applying = []proxy.FaultWindow{{First: applied, Retired: retired}}
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 1}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Errorf("The run reported %v, want a G4: the proxy had stopped applying the fault.", result.Violation)
	}
	if got := result.Timeline.Faults; len(got) != 1 || !got[0].Start.Equal(applied) || !got[0].End.Equal(retired) {
		t.Errorf("The fault's window is %+v, want the %v to %v the proxy applied it in.", got, applied, retired)
	}
}

// Two fault ops can inject equal specs, and dropping the one that ran out
// leaves the other active with its own window.
func TestRunKeepsTheFaultEqualToOneThatRanOut(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	first, retired, second := time.Now().Add(-10*time.Second), time.Now().Add(-9*time.Second), time.Now().Add(-8*time.Second)
	h.applying = []proxy.FaultWindow{{First: first}}
	h.applyingInWait = []proxy.FaultWindow{{First: first, Retired: retired}, {First: second}}
	fault := Fault{Match: Match{Verb: "create", Resource: "configmaps"}, Action: Action{Error: 500}, Until: Trigger{Count: 12}}
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &fault},
		Op{Type: OpFault, Fault: &fault},
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpSettle},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if got, settled := result.Timeline.Faults[1], result.Timeline.Ops[3].Settled; !got.Start.Equal(second) || !got.End.After(settled.Window.End) {
		t.Errorf("The second fault's window is %+v, want its own from %v, open past the last settle wait.", got, second)
	}
	if result.Violation == nil || !strings.Contains(result.Violation.Statement, "after the last fault stopped") {
		t.Errorf("The run reported %v, want only the G4 of the wait after the last fault stopped.", result.Violation)
	}
}

// A fault that stopped as the wait ended leaves the target time it has not
// had yet, so the Runner leaves the wait to the teardown's recovery.
func TestRunExcusesAWaitThatEndedWhileTheTargetWasOwedRecovery(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.applying = []proxy.FaultWindow{{First: time.Now().Add(-time.Second), Retired: time.Now()}}
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 1}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || !strings.Contains(result.Violation.Statement, "after the last fault stopped") {
		t.Errorf("The run reported %v, want only the G4 of the wait after the last fault stopped.", result.Violation)
	}
}

// The proxy first applies a fault when the target makes a request it matches,
// which may be after the wait began.
func TestRunExcusesAWaitAFaultFirstReachedPartWayThrough(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.faultingInWait = true
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 30}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if applied, wait := result.Timeline.Faults[0].Start, result.Timeline.Ops[1].Settled.Window; !applied.After(wait.Start) {
		t.Fatalf("The proxy first applied the fault at %v, want it after the wait began at %v.", applied, wait.Start)
	}
	if result.Violation == nil || !strings.Contains(result.Violation.Statement, "after the last fault stopped") {
		t.Errorf("The run reported %v, want only the G4 of the wait after the last fault stopped.", result.Violation)
	}
}

// A fault active as a wait began excuses the wait only while the target is
// still owed time to recover from it when the wait ends.
func TestRunRecordsG4WhenAWaitOutlastsTheRecoveryTheFaultOwed(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.applying = []proxy.FaultWindow{{First: time.Now()}}
	h.retiresInWait = time.Millisecond
	short := *toyTarget
	short.Timeouts.Settle = 10 * time.Millisecond
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 30}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	result, err := runSequence(t.Context(), &short, sequence, Options{Check: &fakeChecker{}}, h)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if fault, wait := result.Timeline.Faults[0], result.Timeline.Ops[1].Settled.Window; fault.Start.After(wait.Start) || !fault.End.After(wait.Start) {
		t.Fatalf("The fault was active from %v to %v, want it active as the wait began at %v.", fault.Start, fault.End, wait.Start)
	}
	if result.Violation == nil || !strings.Contains(result.Violation.Statement, "after op 1 (create)") {
		t.Errorf("The run reported %v, want the G4 of the create's wait.", result.Violation)
	}
}

// A fault op whose fault matches no request changes nothing, so it leaves the
// run as judged as one with no fault op at all (DESIGN.md §6, D36).
func TestRunJudgesARunWhoseFaultMatchedNothing(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Match: Match{Resource: "secrets"}, Action: Action{Error: 500}, Until: Trigger{Count: 1}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Errorf("The run reported %v, want a G4: the proxy never applied the fault.", result.Violation)
	}
	if got := result.Timeline.Faults; len(got) != 1 || !got[0].Start.IsZero() {
		t.Errorf("The fault's window is %+v, want no window: the proxy applied nothing.", got)
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
	if got := h.opCalls(); !slices.Contains(got, "removeFault 0") {
		t.Errorf("The run did %v, want the expired fault removed.", got)
	}
}

// A settle wait runs until the target has had as long as the faults lasted,
// and T_settle more, to recover from them. The fault here stops during the
// wait.
func TestRunTellsTheSettleWaitWhatRecoveryTheFaultsAreOwed(t *testing.T) {
	h := newFakeHarness()
	applied, retired := time.Now().Add(-3*time.Second), time.Now().Add(-time.Second)
	h.applying = []proxy.FaultWindow{{First: applied}}
	h.applyingInWait = []proxy.FaultWindow{{First: applied, Retired: retired}}
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 1}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	_, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := retired.Add(retired.Sub(applied) + testTimeouts.Settle)
	if len(h.owed) == 0 || !h.owed[0].Equal(want) {
		t.Errorf("The settle wait was told the target owed %v, want %v.", h.owed, want)
	}
}

// A fault still active when the ops end is one no settle wait gave the target
// time to recover from, so the teardown gives it one before its quiet window.
func TestTheTeardownWaitsForTheTargetToRecoverFromAFaultItCleared(t *testing.T) {
	h := newFakeHarness()
	h.faulting = true
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 30}}},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := []string{
		"clearFaults",
		"settle",
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
	recovery := result.Timeline.Recovery
	if recovery == nil || !recovery.Converged {
		t.Fatalf("The run recorded the recovery %+v, want a wait that converged.", recovery)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{0, 2, Recovery, Teardown}) {
		t.Errorf("The run checkpointed at %v, want one where the recovery ended.", got)
	}
	if quiet := result.Timeline.Quiet.Start; quiet.Before(recovery.Window.End) {
		t.Errorf("The teardown's quiet window opens at %v, before the recovery ended at %v.", quiet, recovery.Window.End)
	}
}

func TestTheTeardownWaitsForNoRecoveryTheTargetIsNotOwed(t *testing.T) {
	for _, test := range []struct {
		name     string
		applying []proxy.FaultWindow
	}{
		{name: "a fault that matched nothing"},
		{name: "a fault the target converged after",
			applying: []proxy.FaultWindow{{First: time.Now().Add(-3 * time.Second), Retired: time.Now().Add(-2 * time.Second)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newFakeHarness()
			h.applying = test.applying
			sequence := sequenceOf(
				Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 1}}},
				Op{Type: OpCreate, Obj: widget("widget")},
			)

			result, err := runFake(t, h, nil, sequence)

			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			if got := h.teardownCalls(); slices.Contains(got, "settle") || result.Timeline.Recovery != nil {
				t.Errorf("The teardown did %v, want no wait: the target owed nothing.", got)
			}
		})
	}
}

// A run that ended early is judged no further, so the teardown gives it no
// time to recover.
func TestTheTeardownWaitsForNoRecoveryAfterTheRunEnded(t *testing.T) {
	for _, test := range []struct {
		name  string
		check *fakeChecker
		fail  map[string]error
	}{
		{name: "at a violation", check: &fakeChecker{violations: [][]Violation{{{ID: "G2"}}}}},
		{name: "at a harness error", check: &fakeChecker{}, fail: map[string]error{"restart": errors.New("the target will not die")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newFakeHarness()
			h.faulting = true
			if test.fail != nil {
				h.fail = test.fail
			}
			sequence := sequenceOf(
				Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}},
				Op{Type: OpCreate, Obj: widget("widget")},
				Op{Type: OpRestart},
			)

			result, _ := runFake(t, h, test.check, sequence)

			if got := h.teardownCalls(); slices.Contains(got, "settle") || result.Timeline.Recovery != nil {
				t.Errorf("The teardown did %v, want no wait: the run had ended.", got)
			}
		})
	}
}

func TestRunRecordsG4WhenTheRecoveryExpires(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.faulting = true
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	violation, recovery := result.Violation, result.Timeline.Recovery
	if violation == nil || violation.ID != "G4" || !strings.Contains(violation.Statement, "after the last fault stopped") {
		t.Fatalf("The run reported %v, want the G4 of the wait after the last fault stopped.", violation)
	}
	if recovery == nil || recovery.Converged || !violation.At.Equal(recovery.Window.End) {
		t.Errorf("The violation is stamped %v, want the end of a recovery that expired: %+v.", violation.At, recovery)
	}
	if want := fmt.Sprintf("in %v the target", recovery.Window.End.Sub(recovery.Window.Start).Round(time.Millisecond)); !strings.Contains(violation.Evidence, want) {
		t.Errorf("The evidence is %q, want it to say how long the wait ran: %q.", violation.Evidence, want)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{1, Recovery}) {
		t.Errorf("The run checkpointed at %v, want the recovery's last and no deletion judged.", got)
	}
}

// The recovery is judged, as an op's wait is, so the caller's deadline ends
// it. The teardown that follows does not answer to that deadline.
func TestTheRecoveryEndsWithTheCallersContext(t *testing.T) {
	h := newFakeHarness()
	h.faulting = true
	ctx, cancel := context.WithCancel(t.Context())
	h.cancel, h.cancelsAfter = cancel, "clearFaults"
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}},
		Op{Type: OpSettle},
	)

	result, err := runSequence(ctx, toyTarget, sequence, Options{Check: &fakeChecker{}}, h)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("The run returned %v, want the caller's context ended.", err)
	}
	if !slices.Contains(h.calls, "stop") {
		t.Errorf("The run did %v, want it torn down anyway.", h.calls)
	}
	if got := checkpointsAt(result.Timeline); slices.Contains(got, Teardown) {
		t.Errorf("The run checkpointed at %v, want no deletion judged after the recovery failed.", got)
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

// A generated deleteManaged can outrun what the target manages, and the run is
// worth finishing. The skip is a note, never silence (DESIGN.md §6).
func TestRunSkipsADeleteManagedThatResolvesToNothing(t *testing.T) {
	h := newFakeHarness()
	op := Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(7)}

	result, err := runFake(t, h, nil, sequenceOf(op))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if got := h.opCalls(); slices.ContainsFunc(got, func(call string) bool {
		return strings.HasPrefix(call, "deleteManaged")
	}) {
		t.Errorf("The run did %v, want nothing deleted: the index resolves to nothing.", got)
	}
	if len(result.Notes) != 1 || !strings.Contains(result.Notes[0], "op 0 (deleteManaged)") {
		t.Errorf("The run reported the notes %v, want the skipped op named.", result.Notes)
	}
}

func TestRunReportsADeleteManagedOfAKindTheTargetDoesNotManage(t *testing.T) {
	h := newFakeHarness()
	op := Op{Type: OpDeleteManaged, Kind: "v1/Secret", Nth: nth(0)}

	_, err := runFake(t, h, nil, sequenceOf(op))

	if err == nil {
		t.Fatalf("The run succeeded, want a harness error.")
	}
	if !strings.Contains(err.Error(), op.Kind) {
		t.Errorf("The run returned %q, want the kind named.", err)
	}
	if !slices.Contains(h.calls, "stop") {
		t.Errorf("The run did %v, want it torn down anyway.", h.calls)
	}
}

func TestRunTearsDownInTheOrderTheDesignGives(t *testing.T) {
	h := newFakeHarness()
	h.forced = []string{"v1/ConfigMap widget-0", "toy.botbox/v1/Widget widget"}
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")})

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := []string{
		"clearFaults",
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
		t.Errorf("The run recorded the forced finalizers %v, want %v.", result.Timeline.Forced, h.forced)
	}
	// A namespace botbox emptied by hand is not one the target cleaned, and no
	// check judges what the teardown did (DESIGN.md §5.5, D37).
	note := strings.Join(result.Notes, "\n")
	for _, forced := range h.forced {
		if !strings.Contains(note, forced) {
			t.Errorf("The run notes are %q, want them to name the finalizer forced off %s.", note, forced)
		}
	}
	if !strings.Contains(note, "did not empty on its own") {
		t.Errorf("The run notes are %q, want them to say the namespace did not empty on its own.", note)
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

	if err := WriteRunSequence(dir, sequence); err != nil {
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
	h.faulting = true
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

// G3 reads whether the teardown saw the namespace empty (DESIGN.md §6, D34). A
// run that already found a violation records no teardown checkpoint, so the
// clean cannot ride on one.
func TestTheTeardownRecordsACleanNamespaceEvenAfterAViolation(t *testing.T) {
	// The delay separates the window's ends, so that a stamp taken from the
	// wrong one fails by a visible margin rather than by nanoseconds.
	h := newFakeHarness()
	h.deleteCRDelay = 20 * time.Millisecond
	check := &fakeChecker{violations: [][]Violation{{{ID: "G2"}}}}

	result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil {
		t.Fatal("The run reported no violation, so it does not exercise the case.")
	}
	if result.Timeline.Cleaned.IsZero() {
		t.Errorf("The teardown saw the namespace clean and the timeline does not say so, so G3 cannot judge it.")
	}
	if got, want := result.Timeline.Cleaned, result.Timeline.Deletion.End; !got.Equal(want) {
		t.Errorf("The timeline reports it clean at %v, want the %v the deletion window closed at.", got, want)
	}
}

// A namespace the teardown never saw empty leaves no instant to report.
func TestTheTeardownReportsNoCleanWhenTheNamespaceStaysDirty(t *testing.T) {
	h := newFakeHarness()
	h.clean = false

	result, err := runFake(t, h, &fakeChecker{}, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if !result.Timeline.Cleaned.IsZero() {
		t.Errorf("The timeline reports the namespace clean at %v, and the teardown never saw it empty.",
			result.Timeline.Cleaned)
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

// A child the collector keeps would fail G3 with no reason given.
func TestRunNotesEachOwnerTheCollectorCouldNotResolve(t *testing.T) {
	h := newFakeHarness()
	h.unresolved = []cluster.Unresolved{
		{
			DependentKind: configMapKind, DependentName: "widget-cfg",
			OwnerKind: schema.GroupVersionKind{Group: "toy.botbox", Version: "v1alpha9", Kind: "Widget"}, OwnerName: "widget",
			Unserved: true,
		},
		{
			DependentKind: schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, DependentName: "widget-tls",
			OwnerKind: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, OwnerName: "issuer",
		},
	}
	check := &fakeChecker{notes: [][]string{nil, {"G3 is not evaluated for the deletion of widget"}}}

	result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := []string{
		"botbox's garbage collector never deletes v1/ConfigMap widget-cfg, because the API server does not serve toy.botbox/v1alpha9/Widget, the kind of its owner widget",
		"botbox's garbage collector never deletes v1/Secret widget-tls, because it does not watch apps/v1/Deployment, the kind of its owner issuer",
		"G3 is not evaluated for the deletion of widget",
	}
	if !slices.Equal(result.Notes, want) {
		t.Errorf("The run carried the notes\n\t%q\nwant\n\t%q", result.Notes, want)
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

// A target that stopped takes the run with it, so the ops behind it never
// reach the cluster: they would run against nothing.
func TestTheRunnerAppliesNoOpToATargetThatStopped(t *testing.T) {
	h := &fakeHarness{converged: true, clean: true, stopsAfter: "createCR widget", targetExit: errors.New("exit status 1")}
	dir := t.TempDir()
	written := "toy-widget: listen tcp 127.0.0.1:9440: bind: address already in use"
	if err := os.WriteFile(filepath.Join(dir, targetLogFile), []byte(written+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget"), NoSettle: true},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}},
	)

	result, err := runSequence(t.Context(), toyTarget, sequence, Options{Check: &fakeChecker{}, Dir: dir}, h)

	if err == nil {
		t.Fatal("The run reported no error although the target had stopped.")
	}
	for _, want := range []string{"op 1 (update)", "no longer running", "exit status 1", written} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("The error is %q, which does not mention %q.", err, want)
		}
	}
	if got, want := h.opCalls(), []string{"createCR widget"}; !slices.Equal(got, want) {
		t.Errorf("The run did %v, want %v: an op is never applied to a target that stopped.", got, want)
	}
	if len(result.Timeline.Ops) != 1 {
		t.Errorf("The run recorded %d ops, want the one it applied while the target ran.", len(result.Timeline.Ops))
	}
	if result.Violation != nil {
		t.Errorf("The run reported %+v against the target; a target that stopped is the harness's failure.", result.Violation)
	}
}

// The teardown judges the deletion it asked for. A target that stopped cleaned
// nothing up, so the window is not its to answer for.
func TestTheTeardownJudgesNoDeletionWithATargetThatStopped(t *testing.T) {
	h := &fakeHarness{converged: true, stopsAfter: "deleteCR widget", targetExit: errors.New("exit status 1")}
	g3 := Violation{ID: "G3", Statement: "the CR widget still carried its finalizers"}
	check := &fakeChecker{violations: [][]Violation{nil, {g3}}}

	result, err := runSequence(t.Context(), toyTarget, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}),
		Options{Check: check, Dir: t.TempDir()}, h)

	if err == nil {
		t.Fatal("The run reported no error although the target had stopped.")
	}
	for _, want := range []string{"teardown", "no longer running", "exit status 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("The error is %q, which does not mention %q.", err, want)
		}
	}
	if result.Violation != nil {
		t.Errorf("The run reported %+v; a target that stopped answers for no deletion.", result.Violation)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{0}) {
		t.Errorf("The run checkpointed at %v, want the ops it judged while the target ran.", got)
	}
}

// A run a harness error ended did not run all its ops, so the deletion is not
// the target's to answer for.
func TestTheTeardownJudgesNoDeletionAfterAHarnessError(t *testing.T) {
	h := newFakeHarness()
	h.fail["restart"] = errors.New("the target will not die")
	g3 := Violation{ID: "G3", Statement: "the CR widget still carried its finalizers"}
	check := &fakeChecker{violations: [][]Violation{nil, {g3}}}

	result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}, Op{Type: OpRestart}))

	if err == nil || !strings.Contains(err.Error(), "op 1 (restart)") {
		t.Errorf("The run returned %v, want the failure of op 1.", err)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %+v; a run that failed answers for no deletion.", result.Violation)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{0}) {
		t.Errorf("The run checkpointed at %v, want the ops it judged before the failure.", got)
	}
	if len(check.inputs) != 1 {
		t.Errorf("The checks ran %d times, want once: the teardown judges nothing after a failure.", len(check.inputs))
	}
}

// TestASettleExpiryWithADeadTargetIsAHarnessError pins the difference between
// the target failing and the harness failing. A target that is gone cannot
// converge, so reporting G4 would accuse a controller of a fault that is ours:
// a port collision reads exactly this way (DESIGN.md §5.1).
func TestASettleExpiryWithADeadTargetIsAHarnessError(t *testing.T) {
	h := &fakeHarness{clean: true, stopsAfter: "settle", targetExit: errors.New("exit status 1")}
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")})
	// A target that refuses its own flags says so on the way out, and that
	// line is what the reader acts on.
	dir := t.TempDir()
	written := "toy-widget: --bug=12: want a bug ID from 0 to 11"
	if err := os.WriteFile(filepath.Join(dir, targetLogFile), []byte("starting\n"+written+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := runSequence(t.Context(), toyTarget, sequence, Options{Check: &fakeChecker{}, Dir: dir}, h)
	if err == nil {
		t.Fatal("The run reported no error although the target had stopped.")
	}
	for _, want := range []string{"no longer running", "exit status 1", "target.log", written} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("The error is %q, which does not mention %q.", err, want)
		}
	}
	if result.Violation != nil {
		t.Errorf("The run reported %+v against the target; a dead target is the harness's failure.", result.Violation)
	}
}

// A launcher holding no process knows no exit status, and the error says what
// it knows.
func TestTheStoppedTargetsErrorWithoutAnExitStatus(t *testing.T) {
	h := &fakeHarness{clean: true, targetGone: true}

	_, err := runSequence(t.Context(), toyTarget, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}),
		Options{Check: &fakeChecker{}, Dir: t.TempDir()}, h)

	if err == nil {
		t.Fatal("The run reported no error although the target had stopped.")
	}
	if !strings.Contains(err.Error(), "no longer running") || strings.Contains(err.Error(), "<nil>") {
		t.Errorf("The error is %q, want it to say what the launcher knows.", err)
	}
}

// The harness quotes what the target said on the way out, so what it quotes
// has to be a whole line: klog puts the level and the message at the front,
// and a Go panic puts the stack after it.
func TestWhatTheTargetSaidOnTheWayOut(t *testing.T) {
	// The sizes are literal, so that a wider maxTail fails this rather than
	// scaling the log with it.
	const pastTheTail = 5000
	for _, log := range []struct {
		name  string
		wrote string
		want  string
	}{
		{"one line and no newline", "toy-widget: --bug=12: want a bug ID from 0 to 11",
			"toy-widget: --bug=12: want a bug ID from 0 to 11"},
		{"a trailing blank line", "the message\n   \n", "the message"},
		{"a panic before its stack",
			"starting\npanic: runtime error: index out of range\n\ngoroutine 1 [running]:\nmain.main()\n\t/src/main.go:57 +0x1d5\n",
			"panic: runtime error: index out of range"},
		{"several lines and no panic", "starting\nlistening on :8080\nE0921 fatal: reconcile failed\n",
			"E0921 fatal: reconcile failed"},
		{"a tail that begins mid-line", strings.Repeat("y", pastTheTail) + "\nE0921 fatal: reconcile failed\n",
			"E0921 fatal: reconcile failed"},
		{"a last line longer than the tail", "E0921 fatal: " + strings.Repeat("x", pastTheTail) + "\n", ""},
		{"nothing at all", "", ""},
		{"only newlines", "\n\n\n", ""},
	} {
		t.Run(log.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), targetLogFile)
			if err := os.WriteFile(path, []byte(log.wrote), 0o644); err != nil {
				t.Fatal(err)
			}

			if got := whyItStopped(path); got != log.want {
				t.Errorf("A log of %d bytes reads as %q, want %q.", len(log.wrote), got, log.want)
			}
		})
	}
	if got := whyItStopped(filepath.Join(t.TempDir(), "no-such-log")); got != "" {
		t.Errorf("A log botbox never wrote reads as %q.", got)
	}
}

// A target that wrote nothing leaves the reader its log, and one that wrote
// control bytes leaves them quoted: the line goes into an error CI prints.
func TestTheStoppedTargetsErrorWithoutALineAndWithControlBytes(t *testing.T) {
	for _, log := range []struct {
		name  string
		wrote string
		want  string
		says  bool
	}{
		{name: "an empty log", wrote: "", want: "its output is in"},
		{name: "an escape sequence", wrote: "fatal: \x1b[31mbad flag\x1b[0m\n", want: `\x1b[31mbad flag`, says: true},
	} {
		t.Run(log.name, func(t *testing.T) {
			h := &fakeHarness{clean: true, targetGone: true, targetExit: errors.New("exit status 1")}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, targetLogFile), []byte(log.wrote), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := runSequence(t.Context(), toyTarget, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}),
				Options{Check: &fakeChecker{}, Dir: dir}, h)

			if err == nil {
				t.Fatal("The run reported no error although the target had stopped.")
			}
			if !strings.Contains(err.Error(), log.want) {
				t.Errorf("The error is %q, which does not carry %q.", err, log.want)
			}
			if said := strings.Contains(err.Error(), "it wrote"); said != log.says {
				t.Errorf("The error is %q, and the log held %q.", err, log.wrote)
			}
		})
	}
}

// The G4 an expired wait raised counts what the target managed, as a check's
// G4 does (#13).
func TestTheG4OfAnExpiredWaitCountsTheManagedObjects(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	for i := range 3 {
		h.recordChild(fmt.Sprintf("widget-%d", i), "12")
	}

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Fatalf("The run reported %v, want a G4 violation.", result.Violation)
	}
	if want := "the target managed 3 objects of the kinds it declares"; !strings.Contains(result.Violation.Evidence, want) {
		t.Errorf("The G4's evidence is %q, want it to say %q.", result.Violation.Evidence, want)
	}
	if result.Violation.ManagedTotal == nil || *result.Violation.ManagedTotal != 3 {
		t.Errorf("The G4 carried out no count of 3, and its evidence quotes one.")
	}
}

// The G4 an expired wait raised quotes the children as a table of their own,
// bounded and newest first, since the object a readiness finding is about is
// usually one of them (#24).
func TestTheG4OfAnExpiredWaitQuotesTheManagedObjects(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.recordCR("widget", "11")
	for i := range 25 {
		h.recordChild(fmt.Sprintf("widget-%d", i), strconv.Itoa(12+i))
	}

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Fatalf("The run reported %v, want a G4 violation.", result.Violation)
	}
	managed := result.Violation.Managed
	if len(managed) != invariant.MaxEvidence {
		t.Fatalf("The violation quotes %d managed objects, want the bound of %d.", len(managed), invariant.MaxEvidence)
	}
	if managed[0].Name != "widget-24" {
		t.Errorf("The state opens at %s, want widget-24, the child recorded last.", managed[0].Name)
	}
	if result.Violation.ManagedTotal == nil || *result.Violation.ManagedTotal != 25 {
		t.Errorf("The violation counts %v managed objects, want 25.", result.Violation.ManagedTotal)
	}
	if want := kindName(widgetKind) + " widget"; result.Violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", result.Violation.VersionsOf, want)
	}
	versions := result.Violation.Versions
	if !slices.ContainsFunc(versions, func(v observe.Version) bool { return v.GVK == widgetKind }) {
		t.Errorf("The timeline holds %d versions and not the CR the statement names.", len(versions))
	}
	if slices.ContainsFunc(versions, func(v observe.Version) bool { return v.GVK == configMapKind }) {
		t.Errorf("The timeline holds %v, want the CR's history alone.", versions)
	}
}
