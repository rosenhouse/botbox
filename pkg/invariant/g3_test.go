package invariant_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/invariant"
)

const cleanup = "widget.botbox/cleanup"

// deletedRun is a run whose CR botbox deletes at 10s, with the children the
// options build.
func deletedRun() *run {
	return newRun().
		record(0, widget("10", spec(1), status(1, 1), finalizers(cleanup))).
		op(invariant.OpDelete, 10*time.Second).
		record(10*time.Second, widget("11", spec(1), status(1, 1), finalizers(cleanup), deleting(10*time.Second)))
}

func TestG3PassesWhenTheChildrenAndTheFinalizerGo(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12")).
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	silent(t, invariant.CleanDeletion, in)
}

func TestG3FiresOnAnOrphanTheCollectorCannotReach(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if violation.ID != "G3" {
		t.Errorf("The violation is %q, want G3.", violation.ID)
	}
	if !strings.Contains(violation.Statement, "w-0") || !strings.Contains(violation.Statement, "ownerReference") {
		t.Errorf("The statement is %q, want it to name the orphan w-0.", violation.Statement)
	}
	if len(violation.Versions) == 0 || violation.Versions[0].Name != "w-0" {
		t.Fatalf("The evidence holds %v, want the orphan's timeline.", violation.Versions)
	}
}

func TestG3FiresOnAChildTheTargetKeptOwning(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if !strings.Contains(violation.Statement, "w-0") {
		t.Errorf("The statement is %q, want it to name the child that remained.", violation.Statement)
	}
}

func TestG3FiresOnAFinalizerThatNeverClears(t *testing.T) {
	in := deletedRun().through(21 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if !strings.Contains(violation.Statement, cleanup) {
		t.Errorf("The statement is %q, want it to name the finalizer.", violation.Statement)
	}
	if len(violation.Versions) == 0 || violation.Versions[0].GVK != widgetGVK {
		t.Fatalf("The evidence holds %v, want the CR's timeline.", violation.Versions)
	}
}

func TestG3WaitsForTheWholeDeleteTimeout(t *testing.T) {
	in := deletedRun().through(19 * time.Second)

	silent(t, invariant.CleanDeletion, in)
}

func TestG3IgnoresADeletionAFaultReachedInto(t *testing.T) {
	in := deletedRun().
		fault(11*time.Second, 12*time.Second).
		through(21 * time.Second)

	silent(t, invariant.CleanDeletion, in)
}

func TestG3IgnoresACRTheRunRecreated(t *testing.T) {
	in := deletedRun().
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		op(invariant.OpRecreate, 13*time.Second).
		record(13*time.Second, widget("20", spec(1), status(1, 1), uid("uid-w2"), finalizers(cleanup))).
		through(30 * time.Second)

	silent(t, invariant.CleanDeletion, in)
}

func TestG3IgnoresTheObjectsOfACRTheRunRecreated(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12")).
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		op(invariant.OpRecreate, 13*time.Second).
		record(13*time.Second, widget("20", spec(1), status(1, 1), uid("uid-w2"), finalizers(cleanup))).
		record(13500*time.Millisecond, child("w-0", "21", uid("uid-w-0-again"))).
		through(30 * time.Second)

	silent(t, invariant.CleanDeletion, in)
}

func TestG3StillFiresOnAnOrphanTheRunRecreatedTheCRPast(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		op(invariant.OpRecreate, 13*time.Second).
		record(13*time.Second, widget("20", spec(1), status(1, 1), uid("uid-w2"), finalizers(cleanup))).
		through(30 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if !strings.Contains(violation.Statement, "w-0") {
		t.Errorf("The statement is %q, want it to name the orphan the first CR left.", violation.Statement)
	}
}
