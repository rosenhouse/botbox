package run

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/invariant"
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
		Namespace: fakeNamespace,
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
	cr.SetNamespace(fakeNamespace)
	cr.SetName("widget")
	cr.SetResourceVersion(resourceVersion)
	store.Record(widgetKind, cr, when)
}

// recordChild records a ConfigMap the target owns nothing of, which is what
// G3 reports as an orphan.
func recordChild(store *observe.Store, when time.Time, name, resourceVersion string) *unstructured.Unstructured {
	child := &unstructured.Unstructured{Object: map[string]any{}}
	child.SetGroupVersionKind(configMapKind)
	child.SetNamespace(fakeNamespace)
	child.SetName(name)
	child.SetResourceVersion(resourceVersion)
	store.Record(configMapKind, child, when)
	return child
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
	found, err := Engine{}.Check(in)
	if err != nil {
		t.Fatalf("The checks failed to evaluate: %v", err)
	}
	return found.Violations
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

// The teardown's wait for the target to recover from the faults is a settle
// wait, and one that expired is G4's.
func TestTheChecksReadTheRecoveryCheckpointAsASettleWait(t *testing.T) {
	in := convergedRun()
	in.Timeline.Faults = []Window{{Start: at(3), End: at(4)}}
	in.Timeline.Checkpoints = append(in.Timeline.Checkpoints, Checkpoint{At: at(10), Op: Recovery, Converged: false})

	violations := checked(t, in)

	if len(violations) != 1 || !strings.Contains(violations[0].Statement, "after the last fault stopped expired") {
		t.Errorf("The checks reported %v, want the G4 of the expired wait after the last fault stopped.", violations)
	}
}

// A namespace that came clean settles G3 where the teardown stopped watching,
// which is before T_delete is up whenever the target cleans up promptly
// (DESIGN.md §6). The timeline carries that instant, not a teardown
// checkpoint, which a run that already failed never records.
func TestTheChecksTakeTheCleanNamespaceFromTheTimeline(t *testing.T) {
	for _, teardown := range []struct {
		namespace string
		clean     bool
		notes     int
	}{
		{namespace: "came clean", clean: true, notes: 0},
		{namespace: "never emptied", clean: false, notes: 1},
	} {
		t.Run(teardown.namespace, func(t *testing.T) {
			in := deletedRun(teardown.clean)

			g3 := resultOf(t, in, "G3")

			if len(g3.Violations) != 0 {
				t.Errorf("G3 reported %v, want none: nothing the target manages was left.", g3.Violations)
			}
			if len(g3.Notes) != teardown.notes {
				t.Errorf("G3 noted %v, want %d note(s): the deletion's deadline is past the run's end.",
					g3.Notes, teardown.notes)
			}
		})
	}
}

// cleanedAt is when the teardown saw the namespace empty, or zero if it never
// did.
func cleanedAt(clean bool) time.Time {
	if !clean {
		return time.Time{}
	}
	return at(12)
}

// deletedRun deleted its CR at the teardown and stopped watching once the
// namespace was clean, well before T_delete was up.
func deletedRun(clean bool) Input {
	store := history()
	recordWidget(store, at(0.1), "11", 1)
	in := Input{
		Target:  checkTarget(),
		Objects: store,
		Timeline: Timeline{
			Namespace:   fakeNamespace,
			Ops:         []AppliedOp{appliedOp(0, OpCreate, at(0))},
			Checkpoints: []Checkpoint{{At: at(2.1), Op: 0, Converged: true}},
			Quiet:       Window{Start: at(8), End: at(10)},
			Deletion:    Window{Start: at(10), End: at(12)},
			Cleaned:     cleanedAt(clean),
		},
	}
	child := recordChild(store, at(0.2), "widget-0", "12")
	deleting := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"count": int64(1)}, "status": map[string]any{"ready": int64(1)},
	}}
	deleting.SetGroupVersionKind(widgetKind)
	deleting.SetNamespace(fakeNamespace)
	deleting.SetName("widget")
	deleting.SetResourceVersion("13")
	store.RecordDeletion(widgetKind, deleting, at(10.1))
	if clean {
		store.RecordDeletion(configMapKind, child, at(11))
	}
	return in
}

// A deleteManaged op inside the deletion window takes the object out of the
// target's hands, so G3 credits the target with no cleanup it did not do
// (DESIGN.md §5.4, D38). The Runner is the only one that knows which object
// the op resolved to.
func TestTheChecksDoNotCreditACleanupBotboxPerformed(t *testing.T) {
	in := deletedRun(true)
	in.Timeline.Ops = append(in.Timeline.Ops, AppliedOp{
		Op: Op{Index: 1, Type: OpDeleteManaged, Kind: "v1/ConfigMap"}, At: at(11), Resolved: "widget-0",
	})

	g3 := resultOf(t, in, "G3")

	if len(g3.Violations) != 0 {
		t.Errorf("G3 reported %v, and the target still had until its deadline.", g3.Violations)
	}
	if len(g3.Notes) != 1 || !strings.Contains(g3.Notes[0], "widget-0") {
		t.Errorf("G3 noted %v, want one note naming the child botbox deleted.", g3.Notes)
	}
}

// An op whose index resolved to nothing deleted nothing, so it names no
// object for G3 to read (DESIGN.md §5.4).
func TestTheChecksCarryNoObjectForAnOpThatResolvedToNothing(t *testing.T) {
	timeline := Timeline{
		Namespace: fakeNamespace,
		Ops: []AppliedOp{{
			Op: Op{Index: 0, Type: OpDeleteManaged, Kind: "v1/ConfigMap"}, At: at(1),
		}},
	}

	ops := engineOps(checkTarget(), timeline)

	if got := ops[0].Deleted; got != (observe.Key{}) {
		t.Errorf("The op names the object %+v, and its index resolved to nothing.", got)
	}
}

// The Runner sees the checks through one Checker, so the engine hands it the
// notes alongside the violations.
func TestTheEngineCarriesTheNotesOut(t *testing.T) {
	found, err := Engine{}.Check(deletedRun(false))

	if err != nil {
		t.Fatalf("The checks failed to evaluate: %v", err)
	}
	if len(found.Notes) != 1 || !strings.Contains(found.Notes[0], "G3") {
		t.Errorf("The engine reported the notes %v, want G3's.", found.Notes)
	}
}

// resultOf returns what one check found over the whole run.
func resultOf(t *testing.T, in Input, id string) invariant.Result {
	t.Helper()
	results, err := Evaluate(in)
	if err != nil {
		t.Fatalf("The checks failed to evaluate: %v", err)
	}
	for _, result := range results {
		if result.ID == id {
			return result
		}
	}
	t.Fatalf("The engine ran %d checks, none of them %s.", len(results), id)
	return invariant.Result{}
}

// The T_stable the teardown waits before it deletes is the run's last quiet
// window (DESIGN.md §5.5 step 4).
func TestTheChecksJudgeTheQuietWindowTheTeardownWaited(t *testing.T) {
	in := convergedRun()
	in.Timeline.Quiet = Window{Start: at(8), End: at(10)}
	in.Timeline.Deletion = Window{Start: at(10), End: at(21)}
	in.Requests = []proxy.Request{{Start: at(9), Verb: "get", Resource: "configmaps", Name: "widget-0", Status: 200}}

	violations := checked(t, in)

	if len(violations) != 1 || violations[0].ID != "G1" {
		t.Fatalf("The checks reported %v, want G1: the target was still working in the teardown's window.", ids(violations))
	}
	// A report quotes the requests themselves, not only the count in the
	// statement (DESIGN.md §5.7).
	if len(violations[0].Requests) == 0 {
		t.Errorf("G1 carried out no requests, and it counted the ones it found.")
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
	deleting.SetNamespace(fakeNamespace)
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
	// The instant is a field of its own, which the report renders once (#24).
	if !first.At.Equal(at(5)) {
		t.Errorf("G4 is stamped %v, want the checkpoint it judged at.", first.At)
	}
	if strings.Contains(first.Evidence, "at 2") {
		t.Errorf("G4's evidence repeats the instant: %q", first.Evidence)
	}
	// A report quotes the versions themselves, not only this summary of them
	// (DESIGN.md §5.7).
	if len(first.Versions) == 0 {
		t.Errorf("G4 carried out no versions, and its evidence quotes one.")
	}
	if want := "toy.botbox/v1/Widget widget"; first.VersionsOf != want {
		t.Errorf("G4 says its timeline is of %q, want %q.", first.VersionsOf, want)
	}
}

func TestTheChecksNameWhatARestartChanged(t *testing.T) {
	for _, c := range []struct {
		changed []string
		clause  string
	}{
		{[]string{"a"}, `data.a was "0", is "1"`},
		{[]string{"a", "b"}, `data.a was "0", is "1" (1 of 2 differences)`},
	} {
		t.Run(c.clause, func(t *testing.T) {
			store := history()
			recordWidget(store, at(0.1), "11", 1)
			recordData(store, at(0.2), "12", map[string]any{"a": "0", "b": "0"})
			after := map[string]any{"a": "0", "b": "0"}
			for _, key := range c.changed {
				after[key] = "1"
			}
			recordData(store, at(3.5), "22", after)
			in := Input{
				Target:  checkTarget(),
				Objects: store,
				Timeline: Timeline{
					Ops: []AppliedOp{appliedOp(0, OpCreate, at(0)), appliedOp(1, OpRestart, at(3)), appliedOp(2, OpSettle, at(3.1))},
					Checkpoints: []Checkpoint{
						{At: at(2.1), Op: 0, Converged: true},
						{At: at(5.1), Op: 2, Converged: true},
					},
				},
			}

			violations := checked(t, in)

			if ids := ids(violations); len(ids) != 1 || ids[0] != "G5" {
				t.Fatalf("The checks reported %v, want G5 alone.", ids)
			}
			restart := violations[0]
			if len(restart.Differences) != len(c.changed) || restart.DifferencesTotal != len(c.changed) {
				t.Errorf("G5 carried out %+v of %d differences, want %d.", restart.Differences, restart.DifferencesTotal, len(c.changed))
			}
			if want := "the state converged after op 0 (create) and the one after op 2 (settle)"; restart.Compared != want {
				t.Errorf("G5 carried out that it compared %q, want %q.", restart.Compared, want)
			}
			if !strings.HasPrefix(restart.Evidence, c.clause+"; ") {
				t.Errorf("G5's evidence is %q, want it to open with %q.", restart.Evidence, c.clause)
			}
		})
	}
}

// recordData records the ConfigMap widget-0 holding the data.
func recordData(store *observe.Store, when time.Time, resourceVersion string, data map[string]any) {
	child := &unstructured.Unstructured{Object: map[string]any{"data": data}}
	child.SetGroupVersionKind(configMapKind)
	child.SetNamespace(fakeNamespace)
	child.SetName("widget-0")
	child.SetResourceVersion(resourceVersion)
	store.Record(configMapKind, child, when)
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

	found, err := Engine{}.Check(in)

	if err == nil {
		t.Fatalf("The checks reported %v, want the evaluation error.", ids(found.Violations))
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

// The count is over the kinds the target declares, and zero is the finding (#13).
func TestTheChecksSayHowManyObjectsTheTargetManaged(t *testing.T) {
	for _, c := range []struct {
		children int
		want     string
	}{
		{0, "the target managed 0 objects of the kinds it declares"},
		{1, "the target managed 1 object of the kinds it declares"},
		// The state is bounded, and the count is what it was chosen from.
		{25, "the target managed 25 objects of the kinds it declares"},
	} {
		t.Run(c.want, func(t *testing.T) {
			store := history()
			recordWidget(store, at(0.1), "11", 0)
			for i := range c.children {
				recordChild(store, at(0.2), fmt.Sprintf("widget-%d", i), "12")
			}
			in := Input{
				Target:  checkTarget(),
				Objects: store,
				Timeline: Timeline{
					Ops:         []AppliedOp{appliedOp(0, OpCreate, at(0))},
					Checkpoints: []Checkpoint{{At: at(5), Op: 0, Converged: false}},
				},
			}

			violations := checked(t, in)

			if len(violations) == 0 || violations[0].ID != "G4" {
				t.Fatalf("The checks reported %v, want G4 first: the CR never became ready.", ids(violations))
			}
			first := violations[0]
			if !strings.Contains(first.Evidence, c.want) {
				t.Errorf("G4's evidence is %q, want it to say %q.", first.Evidence, c.want)
			}
			if got := strings.Count(first.Evidence, "the target managed"); got != 1 {
				t.Errorf("G4's evidence says what the target managed %d times: %q", got, first.Evidence)
			}
			if first.ManagedTotal == nil || *first.ManagedTotal != c.children {
				t.Errorf("G4 carried out no count of %d, and its evidence quotes one.", c.children)
			}
			if want := min(c.children, invariant.MaxEvidence); len(first.Managed) != want {
				t.Errorf("G4 carried out %d managed objects, want %d.", len(first.Managed), want)
			}
		})
	}
}

// A report says what the check's own bound left out, so the totals have to
// cross out of the engine with the excerpts (#22).
func TestTheChecksCarryOutHowMuchEvidenceTheyChoseFrom(t *testing.T) {
	versions := history()
	for i := range 25 {
		recordWidget(versions, at(0.1+float64(i)*0.1), strconv.Itoa(11+i), 0)
	}
	noisy := convergedRun()
	// The run has to be observed past the quiet window for G1 to judge it.
	noisy.Timeline.Checkpoints = append(noisy.Timeline.Checkpoints, Checkpoint{At: at(4.5), Op: 0, Converged: true})
	for i := range 25 {
		noisy.Requests = append(noisy.Requests, proxy.Request{
			Verb: "get", Resource: "configmaps", Namespace: fakeNamespace,
			Path: fmt.Sprintf("/api/v1/path-%d", i), Start: at(2.2 + float64(i)*0.01), Status: 200,
		})
	}

	for _, c := range []struct {
		want, line string
		in         Input
		of         func(Violation) (int, int)
	}{
		{"G4", "20 of 25 versions", Input{
			Target:  checkTarget(),
			Objects: versions,
			Timeline: Timeline{
				Ops:         []AppliedOp{appliedOp(0, OpCreate, at(0))},
				Checkpoints: []Checkpoint{{At: at(5), Op: 0, Converged: false}},
			},
		}, func(v Violation) (int, int) { return len(v.Versions), v.VersionsTotal }},
		{"G1", "20 of 25 requests", noisy, func(v Violation) (int, int) { return len(v.Requests), v.RequestsTotal }},
	} {
		t.Run(c.want, func(t *testing.T) {
			violations := checked(t, c.in)

			if len(violations) == 0 || violations[0].ID != c.want {
				t.Fatalf("The checks reported %v, want %s first.", ids(violations), c.want)
			}
			quoted, total := c.of(violations[0])
			if quoted != invariant.MaxEvidence {
				t.Errorf("%s quotes %d entries, want the bound of %d.", c.want, quoted, invariant.MaxEvidence)
			}
			if total != 25 {
				t.Errorf("%s says it chose from %d entries, want the 25 the run holds.", c.want, total)
			}
			// The line the CLI prints says the same, or the report states two
			// numbers for one excerpt (#22).
			if !strings.Contains(violations[0].Evidence, c.line) {
				t.Errorf("%s's evidence is %q, want it to say %q.", c.want, violations[0].Evidence, c.line)
			}
		})
	}
}
