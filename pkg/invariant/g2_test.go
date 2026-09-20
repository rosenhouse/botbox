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
		checkpoint(2*time.Second, invariant.Converged).
		request(6*time.Second, get("w-0")).
		through(8 * time.Second)

	silent(t, invariant.NoChurn, in)
}

func TestG2FiresOnAResourceVersionThatMoves(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		record(6*time.Second, widget("11", spec(1), status(1, 1))).
		through(8 * time.Second)

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
		record(6*time.Second, child("w-0-xk92f", "20")).
		through(8 * time.Second)

	violation := fired(t, invariant.NoChurn, in)

	if len(violation.Versions) != 1 || violation.Versions[0].Name != "w-0-xk92f" {
		t.Fatalf("The evidence holds %v, want the ConfigMap that appeared.", violation.Versions)
	}
}

func TestG2FiresOnAStatusWriteThatMovesNoResourceVersion(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		requests(5500*time.Millisecond, 500*time.Millisecond, 3, statusPatch()).
		through(8 * time.Second)

	violation := fired(t, invariant.NoChurn, in)

	if len(violation.Requests) != 3 {
		t.Fatalf("The evidence holds %d requests, want the 3 status writes.", len(violation.Requests))
	}
	if !strings.Contains(violation.Statement, "status") {
		t.Errorf("The statement is %q, want it to name the status writes.", violation.Statement)
	}
}

func TestG2IgnoresChangesBeforeTheQuietWindow(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(0, 1))).
		record(2*time.Second, widget("11", spec(1), status(1, 1))).
		record(3*time.Second, child("w-0", "12")).
		request(3*time.Second, statusPatch()).
		through(8 * time.Second)

	silent(t, invariant.NoChurn, in)
}

func TestG2IgnoresAnObjectBotboxCreated(t *testing.T) {
	fixture := child("shared", "10", orphaned)
	in := newRun().
		fixture(fixture).
		op(invariant.OpCreate, 0).
		record(6*time.Second, fixture).
		through(8 * time.Second)

	silent(t, invariant.NoChurn, in)
}

func TestG2IgnoresTheChangesTheTeardownMade(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1)), child("w-0", "11")).
		teardown(5500*time.Millisecond).
		remove(6*time.Second, child("w-0", "12")).
		request(6*time.Second, statusPatch()).
		through(8 * time.Second)

	silent(t, invariant.NoChurn, in)
}
