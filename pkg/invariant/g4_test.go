package invariant_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

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
