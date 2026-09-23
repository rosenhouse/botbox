package invariant_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/invariant"
)

func TestG2PassesWhenNothingMovesInTheQuietWindow(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1)), child("w-0", "11")).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, get("w-0")).
		through(14 * time.Second)

	silent(t, invariant.NoChurn, in)
}

// The window opens where the settle wait ended, which for a target that
// converges is well inside T_settle (DESIGN.md §6).
func TestG2FiresOnAResourceVersionThatMoves(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		settled(2*time.Second, invariant.Converged).
		record(3*time.Second, widget("11", spec(1), status(1, 1))).
		through(14 * time.Second)

	violation := fired(t, invariant.NoChurn, in)

	if violation.ID != "G2" {
		t.Errorf("The violation is %q, want G2.", violation.ID)
	}
	if len(violation.Versions) != 1 || violation.Versions[0].ResourceVersion != "11" {
		t.Fatalf("The evidence holds %v, want the Widget at resourceVersion 11.", violation.Versions)
	}
}

func TestG2FiresOnAManagedObjectThatAppears(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, child("w-0", "11")).
		settled(2*time.Second, invariant.Converged).
		record(3*time.Second, child("w-0-xk92f", "20")).
		through(14 * time.Second)

	violation := fired(t, invariant.NoChurn, in)

	if len(violation.Versions) != 1 || violation.Versions[0].Name != "w-0-xk92f" {
		t.Fatalf("The evidence holds %v, want the ConfigMap that appeared.", violation.Versions)
	}
}

func TestG2FiresOnAStatusWriteThatMovesNoResourceVersion(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		settled(2*time.Second, invariant.Converged).
		requests(2500*time.Millisecond, 500*time.Millisecond, 3, statusPatch()).
		through(14 * time.Second)

	violation := fired(t, invariant.NoChurn, in)

	if len(violation.Requests) != 3 {
		t.Fatalf("The evidence holds %d requests, want the 3 status writes.", len(violation.Requests))
	}
	if !strings.Contains(violation.Statement, "status") {
		t.Errorf("The statement is %q, want it to name the status writes.", violation.Statement)
	}
}

// A timer that rewrites an unchanged status is what thresholds.quiet admits.
func TestG2PassesStatusWritesAtTheQuietThreshold(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		settled(2*time.Second, invariant.Converged).
		requests(2500*time.Millisecond, time.Second, 2, statusPatch()).
		through(14 * time.Second)
	in.Target.Thresholds.Quiet = 2

	silent(t, invariant.NoChurn, in)
}

func TestG2FiresOnStatusWritesPastTheQuietThreshold(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		settled(2*time.Second, invariant.Converged).
		requests(2500*time.Millisecond, 500*time.Millisecond, 3, statusPatch()).
		through(14 * time.Second)
	in.Target.Thresholds.Quiet = 2

	violation := fired(t, invariant.NoChurn, in)

	if want := "made 3 status writes"; !strings.Contains(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to say it %s.", violation.Statement, want)
	}
	if want := "thresholds.quiet allows 2"; !strings.Contains(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to name the threshold: %q.", violation.Statement, want)
	}
	if want := at(3500 * time.Millisecond); !violation.At.Equal(want) {
		t.Errorf("The violation is at %v, want the write that went past the threshold, at %v.", violation.At, want)
	}
}

func TestG2ReadsANegativeQuietAsZero(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		settled(2*time.Second, invariant.Converged).
		through(14 * time.Second)
	in.Target.Thresholds.Quiet = -1

	silent(t, invariant.NoChurn, in)
}

// A write that changed something is churn however many the target declares.
func TestG2IgnoresTheQuietThresholdForAResourceVersionThatMoves(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		settled(2*time.Second, invariant.Converged).
		record(3*time.Second, widget("11", spec(1), status(1, 1))).
		through(14 * time.Second)
	in.Target.Thresholds.Quiet = 5

	violation := fired(t, invariant.NoChurn, in)

	if len(violation.Versions) != 1 || violation.Versions[0].ResourceVersion != "11" {
		t.Fatalf("The evidence holds %v, want the Widget at resourceVersion 11.", violation.Versions)
	}
}

func TestG2IgnoresChangesBeforeTheQuietWindow(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(0, 1))).
		record(1500*time.Millisecond, widget("11", spec(1), status(1, 1))).
		record(1800*time.Millisecond, child("w-0", "12")).
		request(1800*time.Millisecond, statusPatch()).
		settled(2*time.Second, invariant.Converged).
		through(14 * time.Second)

	silent(t, invariant.NoChurn, in)
}

func TestG2IgnoresAnObjectBotboxCreated(t *testing.T) {
	fixture := child("shared", "10", orphaned)
	in := newRun().
		fixture(fixture).
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		record(3*time.Second, fixture).
		through(14 * time.Second)

	silent(t, invariant.NoChurn, in)
}

func TestG2IgnoresTheChangesTheTeardownMade(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1)), child("w-0", "11")).
		checkpoint(2*time.Second, invariant.Converged).
		teardown(3*time.Second).
		remove(3500*time.Millisecond, child("w-0", "12")).
		request(3500*time.Millisecond, statusPatch()).
		through(14 * time.Second)

	silent(t, invariant.NoChurn, in)
}
