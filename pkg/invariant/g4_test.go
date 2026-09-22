package invariant_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/invariant"
)

func TestG4PassesWhenTheCRIsReadyWithinTheSettleTimeout(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		checkpoint(3*time.Second, invariant.Converged).
		through(8 * time.Second)

	silent(t, invariant.Convergence, in)
}

func TestG4FiresWhenTheReadyPredicateStillFails(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if violation.ID != "G4" {
		t.Errorf("The violation is %q, want G4.", violation.ID)
	}
	if !strings.Contains(violation.Statement, "op 0") {
		t.Errorf("The statement is %q, want it to name the op that changed the spec.", violation.Statement)
	}
	if want := timelineOf(widgetGVK, widgetName); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
	if len(violation.Versions) == 0 || violation.Versions[0].GVK != widgetGVK {
		t.Fatalf("The evidence holds %v, want the CR's timeline.", violation.Versions)
	}
}

func TestG4FiresOnASettleWaitThatExpiredWithNoFault(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		checkpoint(5*time.Second, invariant.Expired).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if !strings.Contains(violation.Statement, "expired") {
		t.Errorf("The statement is %q, want it to name the expired settle wait.", violation.Statement)
	}
}

func TestG4IgnoresASettleWaitAFaultReachedInto(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		fault(time.Second, 6*time.Second).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		checkpoint(5*time.Second, invariant.Expired).
		through(8 * time.Second)

	silent(t, invariant.Convergence, in)
}

func TestG4WaitsForTheWholeSettleTimeout(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		through(4 * time.Second)

	silent(t, invariant.Convergence, in)
}

func TestG4IgnoresAWindowALaterSpecChangeCutShort(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		op(invariant.OpUpdate, 3*time.Second).
		record(3100*time.Millisecond, widget("11", spec(5), generation(2), status(0, 1))).
		record(7*time.Second, widget("12", spec(5), generation(2), status(5, 2))).
		through(9 * time.Second)

	silent(t, invariant.Convergence, in)
}

func TestG4RequiresConvergenceAfterAFaultStops(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		fault(time.Second, 4*time.Second).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		through(10 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if !strings.Contains(violation.Statement, "fault") {
		t.Errorf("The statement is %q, want it to name the fault the target had to recover from.", violation.Statement)
	}
}

func TestG4IgnoresADeadlineWhoseCRIsNotBackYet(t *testing.T) {
	in := newRun().
		record(0, widget("10", spec(2), status(2, 1))).
		op(invariant.OpRecreate, 10*time.Second).
		remove(11*time.Second, widget("11", spec(2), status(2, 1))).
		record(16*time.Second, widget("20", spec(2), status(2, 1), uid("uid-w2"))).
		through(22 * time.Second)

	silent(t, invariant.Convergence, in)
}

func TestG4QuotesTheVersionsNearestTheViolation(t *testing.T) {
	r := newRun().op(invariant.OpCreate, 0)
	for i := range 25 {
		r.record(time.Duration(i)*100*time.Millisecond, widget(strconv.Itoa(10+i), spec(3), status(0, 1)))
	}
	in := r.through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if len(violation.Versions) != 20 {
		t.Fatalf("The evidence holds %d versions, want the 20 a report carries.", len(violation.Versions))
	}
	if last := violation.Versions[19]; last.ResourceVersion != "34" {
		t.Errorf("The evidence ends at resourceVersion %s, want the CR's latest, 34.", last.ResourceVersion)
	}
}

func TestG4NamesTheOpByItsIndexRatherThanItsPosition(t *testing.T) {
	in := invariant.Input{
		Target:      toyTarget(),
		Ops:         []invariant.Op{{Index: 5, Type: invariant.OpUpdate, Time: at(0)}},
		Checkpoints: []invariant.Checkpoint{{Op: 5, Time: at(5 * time.Second), Settle: invariant.Expired}},
		End:         at(5 * time.Second),
	}

	violation := fired(t, invariant.Convergence, in)

	if !strings.Contains(violation.Statement, "op 5") {
		t.Errorf("The statement is %q, want it to name op 5.", violation.Statement)
	}
}

func TestG4MeasuresTheSettleWaitFromTheOpItsCheckpointNames(t *testing.T) {
	in := invariant.Input{
		Target:      toyTarget(),
		Ops:         []invariant.Op{{Index: 1, Type: invariant.OpUpdate, Time: at(10 * time.Second)}},
		Checkpoints: []invariant.Checkpoint{{Op: 1, Time: at(15200 * time.Millisecond), Settle: invariant.Expired}},
		Faults:      []invariant.FaultWindow{{Start: at(9500 * time.Millisecond), End: at(10100 * time.Millisecond)}},
		End:         at(15200 * time.Millisecond),
	}

	silent(t, invariant.Convergence, in)
}

// The Observer runs one informer per kind, so the history can arrive out of
// order. The checks read it as a timeline.
func TestG4ReadsAHistoryTheObserverRecordedOutOfOrder(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(6*time.Second, child("w-0", "11")).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		through(8 * time.Second)

	fired(t, invariant.Convergence, in)
}

// The teardown's own delete bumps metadata.generation while
// status.observedGeneration lags, so the CR botbox is deleting reads as not
// ready (DESIGN.md §5.5, step 4).
func TestG4IgnoresAWindowTheTeardownReachedInto(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		teardown(4*time.Second).
		record(4100*time.Millisecond, widget("11", spec(2), status(2, 1), generation(2),
			finalizers(cleanup), deleting(4*time.Second))).
		through(6 * time.Second)

	silent(t, invariant.Convergence, in)
}

func TestG4FiresOnAWindowThatClosedBeforeTheTeardown(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		teardown(6 * time.Second).
		through(8 * time.Second)

	fired(t, invariant.Convergence, in)
}

// TestG4IgnoresACRUnderDeletion pins §6: a CR whose deletion a mid-sequence op
// requested need not be ready, because the finalizer it is waiting on is G3's
// business. Its generation moves while observedGeneration lags, so without the
// rule the delete itself reads as a convergence failure.
func TestG4IgnoresACRUnderDeletion(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		op(invariant.OpDelete, 2*time.Second).
		record(2100*time.Millisecond, widget("11", spec(2), status(2, 1), generation(2),
			finalizers(cleanup), deleting(2*time.Second))).
		teardown(20 * time.Second).
		through(22 * time.Second)

	silent(t, invariant.Convergence, in)
}

// G4 counts what the target managed at the verdict (#13).
func TestG4SaysHowManyObjectsTheTargetManaged(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if violation.ManagedTotal == nil {
		t.Fatalf("The violation counts no managed objects, want the count at the verdict.")
	}
	if *violation.ManagedTotal != 0 {
		t.Errorf("The violation says the target managed %d objects, want 0.", *violation.ManagedTotal)
	}
}

// The count is the target's holding at the deadline: one child was gone by
// then and another arrived after it.
func TestG4CountsTheManagedObjectsLiveAtTheDeadline(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(3), status(0, 1)), child("w-0", "11"), child("w-1", "12")).
		remove(2*time.Second, child("w-1", "13")).
		record(6*time.Second, child("w-2", "14")).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if violation.ManagedTotal == nil || *violation.ManagedTotal != 1 {
		t.Fatalf("The violation counts %v managed objects, want the 1 live at the deadline.", managed(violation))
	}
	if got := state(violation); len(got) != 1 || got[0] != "w-0" {
		t.Errorf("The state holds %v, want the child live at the deadline.", got)
	}
}

func TestG4CountsTheManagedObjectsAtAnExpiredWait(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1)), child("w-0", "11")).
		checkpoint(5*time.Second, invariant.Expired).
		record(6*time.Second, child("w-1", "12")).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if violation.ManagedTotal == nil || *violation.ManagedTotal != 1 {
		t.Fatalf("The violation counts %v managed objects, want the 1 the wait expired with.", managed(violation))
	}
	if got := state(violation); len(got) != 1 || got[0] != "w-0" {
		t.Errorf("The state holds %v, want the child the wait expired with.", got)
	}
}

// A child that exists and is wrong is what the verdict is about, so G4 quotes
// the state of the objects the target managed beside the CR's timeline (#24).
func TestG4QuotesTheStateOfEveryManagedObject(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(0, 1)), child("w-0", "11", data("wrong"))).
		record(2*time.Second, widget("12", spec(2), status(0, 1))).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if got := state(violation); len(got) != 1 || got[0] != "w-0" {
		t.Errorf("The state holds %v, want the managed object the verdict is about.", got)
	}
	if got := versionsOf(violation.Versions, widgetGVK); len(got) != 2 {
		t.Errorf("The timeline holds %v, want the CR's history.", quoted(violation))
	}
	if len(versionsOf(violation.Versions, configMapGVK)) > 0 {
		t.Errorf("The timeline holds %v, want the CR alone: the children are a table of their own.", quoted(violation))
	}
}

// The timeline and the state each have a bound of their own, so neither
// crowds the other out (#24, D39).
func TestG4BoundsTheTimelineAndTheStateApart(t *testing.T) {
	for _, c := range []struct {
		crVersions, children  int
		wantCRs, wantChildren int
	}{
		{crVersions: 41, children: 35, wantCRs: 20, wantChildren: 20},
		{crVersions: 40, children: 5, wantCRs: 20, wantChildren: 5},
		{crVersions: 5, children: 30, wantCRs: 5, wantChildren: 20},
		{crVersions: 3, children: 2, wantCRs: 3, wantChildren: 2},
	} {
		t.Run(fmt.Sprintf("%d versions of the CR and %d children", c.crVersions, c.children), func(t *testing.T) {
			r := newRun().op(invariant.OpCreate, 0)
			for i := range c.crVersions {
				r.record(time.Duration(i)*100*time.Millisecond, widget(strconv.Itoa(10+i), spec(30), status(0, 1)))
			}
			for i := range c.children {
				r.record(3*time.Second, child("w-"+strconv.Itoa(i), strconv.Itoa(100+i)))
			}
			in := r.through(8 * time.Second)

			violation := fired(t, invariant.Convergence, in)

			crs := violation.Versions
			if len(crs) != c.wantCRs {
				t.Errorf("The timeline holds %d versions of the CR, want %d.", len(crs), c.wantCRs)
			}
			if violation.VersionsTotal != c.crVersions {
				t.Errorf("The timeline says it chose from %d versions, want the %d the CR has.", violation.VersionsTotal, c.crVersions)
			}
			if latest := strconv.Itoa(9 + c.crVersions); len(crs) == 0 || crs[len(crs)-1].ResourceVersion != latest {
				t.Errorf("The timeline holds %v, want it to end at the CR's latest, %s.", quoted(violation), latest)
			}
			if got := state(violation); len(got) != c.wantChildren {
				t.Errorf("The state holds %d children, want %d.", len(got), c.wantChildren)
			}
			if violation.ManagedTotal == nil || *violation.ManagedTotal != c.children {
				t.Errorf("The violation says the target managed %v objects, want %d.", managed(violation), c.children)
			}
		})
	}
}

// A child that did not change inside the wait is still the state the wait
// expired on (#20).
func TestG4QuotesAChildThatNeverChangedDuringAnExpiredWait(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1)), child("w-0", "11")).
		checkpoint(2*time.Second, invariant.Converged).
		op(invariant.OpUpdate, 10*time.Second).
		record(11*time.Second, widget("12", spec(2), generation(2), status(2, 2))).
		checkpoint(15*time.Second, invariant.Expired).
		through(18 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if got := state(violation); len(got) != 1 || got[0] != "w-0" {
		t.Errorf("The state holds %v, want the child the wait expired with.", got)
	}
}

// A target manages several kinds, and the bound takes them one kind at a time
// so that none is lost whole (#20, D39).
func TestG4QuotesEveryManagedKindItCan(t *testing.T) {
	r := newRunManaging(configMapGVK, secretGVK).op(invariant.OpCreate, 0)
	for i := range 40 {
		r.record(time.Duration(i)*10*time.Millisecond, widget(strconv.Itoa(10+i), spec(30), status(0, 1)))
	}
	for i := range 25 {
		r.record(time.Second, child("w-"+strconv.Itoa(i), strconv.Itoa(100+i)))
	}
	for i := range 5 {
		r.record(time.Second, secret("s-"+strconv.Itoa(i), strconv.Itoa(200+i)))
	}
	in := r.through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	for _, gvk := range []schema.GroupVersionKind{configMapGVK, secretGVK} {
		if got := versionsOf(violation.Managed, gvk); len(got) == 0 {
			t.Errorf("The state holds no %s, and the target managed some: %v", kindOf(gvk), state(violation))
		}
	}
}

// The children quoted are the ones nearest the verdict, because a target that
// manages more than the bound has to lose some (#20, D39).
func TestG4QuotesTheChildrenNearestTheVerdict(t *testing.T) {
	r := newRun().op(invariant.OpCreate, 0)
	r.record(time.Second, widget("10", spec(30), status(0, 1)))
	for i := range 30 {
		r.record(time.Duration(100+i)*10*time.Millisecond, child("w-"+strconv.Itoa(i), strconv.Itoa(100+i)))
	}
	in := r.through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	children := state(violation)
	if len(children) == 0 {
		t.Fatalf("The state holds no children.")
	}
	if children[0] != "w-29" {
		t.Errorf("The state opens at %s, want w-29, the child nearest the verdict.", children[0])
	}
}

// An expired wait quotes the CR's history nearest it, as every readiness
// verdict does, and the children as a state beside it (#24).
func TestG4QuotesTheCRsHistoryAtAnExpiredWait(t *testing.T) {
	r := newRun().op(invariant.OpCreate, 0)
	for i := range 25 {
		r.record(time.Duration(1000+i)*time.Millisecond, widget(strconv.Itoa(10+i), spec(2), status(2, 1)))
	}
	for i := range 3 {
		r.record(time.Duration(2000+i)*time.Millisecond, child("w-"+strconv.Itoa(i), strconv.Itoa(100+i)))
	}
	in := r.checkpoint(5*time.Second, invariant.Expired).through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if got := versionsOf(violation.Versions, configMapGVK); len(got) > 0 {
		t.Errorf("The timeline holds %v, want the CR's history alone.", quoted(violation))
	}
	if len(violation.Versions) != invariant.MaxEvidence {
		t.Fatalf("The timeline holds %d versions, want the bound of %d.", len(violation.Versions), invariant.MaxEvidence)
	}
	if want := 25; violation.VersionsTotal != want {
		t.Errorf("The timeline says it chose from %d versions, want the %d the CR has.", violation.VersionsTotal, want)
	}
	if last := violation.Versions[invariant.MaxEvidence-1]; last.ResourceVersion != "34" {
		t.Errorf("The timeline ends at resourceVersion %s, want the CR's latest, 34.", last.ResourceVersion)
	}
	if want := timelineOf(widgetGVK, widgetName); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
	if got := state(violation); len(got) != 3 || got[0] != "w-2" {
		t.Errorf("The state holds %v, want the 3 children, the newest first.", got)
	}
}

// A report of what the run looked like at the verdict cannot quote what came
// after it (#20).
func TestG4QuotesNoVersionRecordedAfterTheVerdict(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(0, 1))).
		record(7*time.Second, widget("11", spec(2), status(2, 1))).
		through(9 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	for _, v := range violation.Versions {
		if v.Time.After(violation.At) {
			t.Errorf("The evidence quotes %s at %s, after the verdict at %s.", v.Name, v.Time, violation.At)
		}
	}
}
