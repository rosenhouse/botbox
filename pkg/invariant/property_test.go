package invariant_test

import (
	"errors"
	"slices"
	"strconv"
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

// A property about the children says how many there were and quotes their
// state beside the CR's timeline, one from each kind in turn and newest first
// (#24).
func TestAPropertyQuotesTheStateItSawBesideTheTimeline(t *testing.T) {
	r := newRunManaging(configMapGVK, secretGVK).op(invariant.OpCreate, 0)
	for i := range 25 {
		r.record(time.Duration(1000+i)*time.Millisecond, secret("s-"+strconv.Itoa(i), strconv.Itoa(100+i)))
	}
	for i := range 3 {
		r.record(time.Duration(2000+i)*time.Millisecond, child("w-"+strconv.Itoa(i), strconv.Itoa(200+i)))
	}
	in := r.record(3*time.Second, widget("300", spec(40), status(40, 1))).
		checkpoint(4*time.Second, invariant.Converged).
		through(8 * time.Second)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if violation.ManagedTotal == nil || *violation.ManagedTotal != 28 {
		t.Errorf("The violation counts %v managed objects, want the 28 the property saw.", managed(violation))
	}
	if len(violation.Managed) != invariant.MaxEvidence {
		t.Fatalf("The state holds %d objects, want the bound of %d.", len(violation.Managed), invariant.MaxEvidence)
	}
	if got := versionsOf(violation.Managed, configMapGVK); len(got) != 3 {
		t.Errorf("The state holds %d of the 3 ConfigMaps: %v", len(got), state(violation))
	}
	if first := state(violation)[0]; first != "w-2" {
		t.Errorf("The state opens at %s, want w-2, the object recorded last.", first)
	}
	if want := timelineOf(widgetGVK, widgetName); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
	if got := versionsOf(violation.Versions, widgetGVK); len(got) != 1 {
		t.Errorf("The timeline holds %v, want the CR's history.", quoted(violation))
	}
}

// A property's evidence is bounded like any other, so it says how much it
// chose from (#22, #24).
func TestAPropertySaysHowMuchEvidenceItChoseFrom(t *testing.T) {
	r := newRun().op(invariant.OpCreate, 0)
	for i := range 25 {
		r.record(time.Second, child("w-"+strconv.Itoa(i), strconv.Itoa(100+i)))
	}
	in := r.record(2*time.Second, widget("14", spec(30), status(30, 1))).
		checkpoint(5*time.Second, invariant.Converged).
		through(8 * time.Second)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if len(violation.Managed) != invariant.MaxEvidence {
		t.Errorf("The property quotes %d managed objects, want the bound of %d.", len(violation.Managed), invariant.MaxEvidence)
	}
	if violation.ManagedTotal == nil || *violation.ManagedTotal != 25 {
		t.Errorf("The property says the target managed %v objects, want the 25 it saw.", managed(violation))
	}
	if len(violation.Versions) != 1 || violation.VersionsTotal != 1 {
		t.Errorf("The timeline holds %v of %d, want the CR's one version.", quoted(violation), violation.VersionsTotal)
	}
}

// A property's timeline is the CR versions nearest the violation, as every
// timeline is (#24, D35).
func TestAPropertyQuotesTheCRVersionsNearestTheViolation(t *testing.T) {
	r := newRun().op(invariant.OpCreate, 0)
	for i := range 25 {
		r.record(time.Duration(i)*100*time.Millisecond, widget(strconv.Itoa(10+i), spec(2), status(2, 1)))
	}
	in := r.checkpoint(5*time.Second, invariant.Converged).through(8 * time.Second)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if len(violation.Versions) != invariant.MaxEvidence {
		t.Fatalf("The timeline holds %d versions, want the bound of %d.", len(violation.Versions), invariant.MaxEvidence)
	}
	if last := violation.Versions[invariant.MaxEvidence-1]; last.ResourceVersion != "34" {
		t.Errorf("The timeline ends at resourceVersion %s, want the CR's latest, 34.", last.ResourceVersion)
	}
	if want := 25; violation.VersionsTotal != want {
		t.Errorf("The timeline says it chose from %d versions, want the %d the CR has.", violation.VersionsTotal, want)
	}
}

// A report of what the run looked like where the property failed cannot quote
// what came after it (#24).
func TestAPropertyQuotesNoVersionRecordedAfterTheViolation(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		checkpoint(3*time.Second, invariant.Converged).
		record(4*time.Second, widget("11", spec(2), status(2, 1))).
		through(8 * time.Second)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	for _, v := range violation.Versions {
		if v.Time.After(violation.At) {
			t.Errorf("The timeline quotes %s at %s, after the violation at %s.", v.Name, v.Time, violation.At)
		}
	}
	if violation.VersionsTotal != 1 {
		t.Errorf("The timeline says it chose from %d versions, want the 1 recorded by then.", violation.VersionsTotal)
	}
}

// A run the Observer never recorded has no CR and no timeline, and the
// property that failed is still reported (#24).
func TestAPropertyFiresOnARunWithNoHistory(t *testing.T) {
	in := invariant.Input{
		Target:      toyTarget(),
		Checkpoints: []invariant.Checkpoint{{Op: 0, Time: at(5 * time.Second), Settle: invariant.Expired}},
		End:         at(5 * time.Second),
	}
	in.Target.Properties = []target.Property{property(target.Checkpoint, crExists)}

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if len(violation.Versions) > 0 {
		t.Errorf("The timeline holds %v, want none: the run recorded nothing.", quoted(violation))
	}
}

// A property whose CR is gone has no timeline to quote, and the state is still
// the finding (#24).
func TestAPropertyThatFoundNoCRQuotesTheStateAlone(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, child("w-0", "11")).
		checkpoint(4*time.Second, invariant.Converged).
		through(8 * time.Second)
	in.Target.Properties = []target.Property{property(target.Checkpoint, crExists)}

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if len(violation.Versions) > 0 {
		t.Errorf("The timeline holds %v, want none: the run has no CR.", quoted(violation))
	}
	if got := state(violation); len(got) != 1 || got[0] != "w-0" {
		t.Errorf("The state holds %v, want the child the property read.", got)
	}
}

func TestAPropertyHoldsForEveryCR(t *testing.T) {
	in := newRun().withSecondWidget().
		op(invariant.OpCreate, 0).
		record(100*time.Millisecond, widget("10", spec(1), status(1, 1)), secondWidget("20", spec(3), status(3, 1))).
		record(200*time.Millisecond, child("w-0", "11"), secondChild("w2-0", "21")).
		checkpoint(2*time.Second, invariant.Converged).
		through(2 * time.Second)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if want := timelineOf(widgetGVK, secondName); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q, the CR the property failed on.", violation.VersionsOf, want)
	}
	if want := "the property did not hold on the CR w2: "; !strings.HasPrefix(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to begin %q.", violation.Statement, want)
	}
}

// A CR's property reads the objects that name it and those that name no CR.
func TestAPropertyReadsTheObjectsOfItsCR(t *testing.T) {
	in := newRun().withSecondWidget().
		op(invariant.OpCreate, 0).
		record(100*time.Millisecond, widget("10", spec(2), status(2, 1)), secondWidget("20", spec(2), status(2, 1))).
		record(200*time.Millisecond, child("w-0", "11"), secondChild("w2-0", "21"), secondChild("w2-1", "22")).
		checkpoint(2*time.Second, invariant.Converged).
		through(2 * time.Second)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if want := timelineOf(widgetGVK, widgetName); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q, whose one child the property counted.", violation.VersionsOf, want)
	}
	if got := state(violation); !slices.Equal(got, []string{"w-0"}) {
		t.Errorf("The violation quotes %v, want w's own child.", got)
	}
}

// crExists is a property of the CR itself, which a run that has none breaks.
func crExists(cr *unstructured.Unstructured, _ []*unstructured.Unstructured) (bool, error) {
	return cr != nil, nil
}
