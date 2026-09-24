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
	if want := timelineOf(configMapGVK, "w-0"); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
	if len(violation.Versions) == 0 || violation.Versions[0].Name != "w-0" {
		t.Fatalf("The evidence holds %v, want the orphan's timeline.", violation.Versions)
	}
}

// A count of zero is a finding, so a check that never asked what the target
// managed leaves none (#24).
func TestG3QuotesNoStateOfItsOwn(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if violation.ManagedTotal != nil {
		t.Errorf("G3 counted %v managed objects, want none: it judges one deletion, not the state.", managed(violation))
	}
	if len(violation.Managed) > 0 {
		t.Errorf("G3 quotes the state %v, want none.", state(violation))
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
	if want := timelineOf(widgetGVK, widgetName); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
	if len(violation.Versions) == 0 || violation.Versions[0].GVK != widgetGVK {
		t.Fatalf("The evidence holds %v, want the CR's timeline.", violation.Versions)
	}
}

func TestG3WaitsForTheWholeDeleteTimeout(t *testing.T) {
	in := deletedRun().through(19 * time.Second)

	noted(t, invariant.CleanDeletion, in, "the run ended")
}

// A namespace that came clean settles the deletion before T_delete is up, so
// a run that stops observing there is judged rather than left unjudged
// (DESIGN.md §6).
func TestG3PassesOnANamespaceThatCameCleanBeforeTheDeadline(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12")).
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		cleaned(12 * time.Second).
		through(12 * time.Second)

	if notes := silent(t, invariant.CleanDeletion, in).Notes; len(notes) > 0 {
		t.Errorf("G3 noted %v, want the clean namespace to decide the deletion.", notes)
	}
}

// The namespace came clean long after this deletion's deadline, which the run
// observed, so the leftovers still count.
func TestG3FiresOnLeftoversTheNamespaceOnlyLostAfterTheDeadline(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		remove(25*time.Second, child("w-0", "13")).
		cleaned(25 * time.Second).
		through(25 * time.Second)

	fired(t, invariant.CleanDeletion, in)
}

func TestG3NotesADeletionAFaultReachedInto(t *testing.T) {
	in := deletedRun().
		fault(11*time.Second, 12*time.Second).
		through(21 * time.Second)

	noted(t, invariant.CleanDeletion, in, "a fault was active")
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

// A deleteManaged op takes the object out of the target's hands, so whether
// the target would have cleaned it is nobody's to say (DESIGN.md §5.4, D38).
func TestG3DoesNotCreditACleanupBotboxPerformed(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		deletedManaged(13*time.Second, "w-0").
		remove(13*time.Second, child("w-0", "15", orphaned)).
		cleaned(13 * time.Second).
		through(21 * time.Second)

	result := evaluate(t, invariant.CleanDeletion, in)

	if len(result.Violations) != 0 {
		t.Errorf("G3 reported %+v, and the target still had until its deadline.", result.Violations)
	}
	note := strings.Join(result.Notes, "\n")
	want := "for the deletion of w: op 1 (deleteManaged) deleted v1/ConfigMap w-0 inside its 10s window"
	if !strings.Contains(note, want) {
		t.Errorf("G3 noted %q, want it to contain %q.", note, want)
	}
}

// The window is the deletion's own. An op before the CR went, or after its
// deadline, says nothing about whether the target cleaned up (D38).
func TestG3NotesOnlyWhatBotboxTookInsideTheWindow(t *testing.T) {
	for _, taken := range []struct {
		name  string
		when  time.Duration
		notes int
	}{
		{"before the CR was deleted", 5 * time.Second, 0},
		{"at the instant the CR went", 10 * time.Second, 0},
		{"at the deadline", 20 * time.Second, 1},
		{"after the deadline", 21 * time.Second, 0},
	} {
		t.Run(taken.name, func(t *testing.T) {
			in := deletedRun().
				record(time.Second, child("w-0", "12", orphaned)).
				deletedManaged(taken.when, "w-0").
				record(taken.when+time.Second, child("w-0", "13", orphaned)).
				remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
				through(22 * time.Second)

			result := evaluate(t, invariant.CleanDeletion, in)

			if len(result.Notes) != taken.notes {
				t.Errorf("G3 noted %v, want %d: the op fell %s.", result.Notes, taken.notes, taken.name)
			}
			if len(result.Violations) != 1 {
				t.Errorf("G3 reported %d violations, want the orphan still there at the deadline.", len(result.Violations))
			}
		})
	}
}

// An object the run recreated carries the same name and a new UID. botbox
// taking that one says nothing about the CR that went before it (D38), and
// this is the shape generation reaches: it draws deleteManaged only while a
// CR is live, which a recreate makes true again.
func TestG3DoesNotNoteAnObjectTheRunRecreated(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", orphaned)).
		remove(11*time.Second, child("w-0", "13", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		record(13*time.Second, child("w-0", "15", orphaned, uid("uid-w-0-again"))).
		deletedManaged(14*time.Second, "w-0").
		remove(14*time.Second, child("w-0", "16", orphaned, uid("uid-w-0-again"))).
		cleaned(15 * time.Second).
		through(21 * time.Second)

	result := evaluate(t, invariant.CleanDeletion, in)

	if len(result.Notes) != 0 {
		t.Errorf("G3 noted %v, and botbox took the object that came after this CR.", result.Notes)
	}
	if len(result.Violations) != 0 {
		t.Errorf("G3 reported %v, and the CR's own child went on time.", result.Violations)
	}
}

// G3 notes only the objects the CR itself had when it went.
func TestG3NotesOnlyTheObjectsTheCRHad(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		op(invariant.OpRecreate, 13*time.Second).
		deletedManaged(14*time.Second, "w-9").
		through(22 * time.Second)

	result := evaluate(t, invariant.CleanDeletion, in)

	if len(result.Notes) != 0 {
		t.Errorf("G3 noted %v, and the CR never had the object botbox took.", result.Notes)
	}
}

// twoWidgetsDeleted is a run of two Widgets with a child each, whose first
// botbox deletes at 10s.
func twoWidgetsDeleted() *run {
	return newRun().withSecondWidget().
		record(0, widget("10", spec(1), status(1, 1), finalizers(cleanup)),
			secondWidget("20", spec(1), status(1, 1), finalizers(cleanup))).
		record(time.Second, child("w-0", "11"), secondChild("w2-0", "21")).
		op(invariant.OpDelete, 10*time.Second).
		record(10*time.Second, widget("12", spec(1), status(1, 1), finalizers(cleanup), deleting(10*time.Second)))
}

func TestG3LeavesTheChildrenOfAnotherCRToIt(t *testing.T) {
	in := twoWidgetsDeleted().
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	silent(t, invariant.CleanDeletion, in)
}

func TestG3FiresOnTheLeftoversOfTheCRThatWent(t *testing.T) {
	in := twoWidgetsDeleted().
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if want := "the v1/ConfigMap w-0 was still there 10s after the CR w was deleted"; !strings.HasPrefix(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to begin %q.", violation.Statement, want)
	}
}

func TestG3LeavesAChildAnotherCRStillOwnsToIt(t *testing.T) {
	in := twoWidgetsDeleted().
		record(time.Second, child("shared", "15", ownedByBoth)).
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	silent(t, invariant.CleanDeletion, in)
}

// An object that names no CR may be any CR's, so the last of them answers for
// it.
func TestG3JudgesAnObjectThatNamesNoCRWhereNoOtherCRRemains(t *testing.T) {
	in := twoWidgetsDeleted().
		record(time.Second, child("kept", "15", orphaned)).
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		opOn(invariant.OpDelete, 30*time.Second, secondName).
		record(30*time.Second, secondWidget("22", spec(1), status(1, 1), finalizers(cleanup), deleting(30*time.Second))).
		remove(31*time.Second, secondChild("w2-0", "23")).
		remove(32*time.Second, secondWidget("24", spec(1), status(1, 1), deleting(30*time.Second))).
		through(41 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if !strings.Contains(violation.Statement, "kept") || !violation.At.Equal(at(40*time.Second)) {
		t.Errorf("G3 reported %q at %v, want the object w2's deletion left, at its deadline.", violation.Statement, violation.At)
	}
}

// Each deleted CR answers for its own leftovers, even where both went.
func TestG3HoldsEachDeletedCRToItsOwnLeftovers(t *testing.T) {
	in := twoWidgetsDeleted().
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		opOn(invariant.OpDelete, 10*time.Second, secondName).
		record(10*time.Second, secondWidget("22", spec(1), status(1, 1), finalizers(cleanup), deleting(10*time.Second))).
		remove(12*time.Second, secondWidget("24", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if !strings.Contains(violation.Statement, "w2-0") {
		t.Errorf("The statement is %q, want it to name w2's child.", violation.Statement)
	}
}

// The finalizer that holds a CR does not make its children another CR's.
func TestG3FiresOnTheChildrenOfACRItsFinalizerHolds(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12")).
		through(21 * time.Second)

	if result := evaluate(t, invariant.CleanDeletion, in); len(result.Violations) != 2 {
		t.Errorf("G3 reported %v, want the finalizer and the child w-0.", statements(result))
	}
}

// An owner of another kind is not a CR, so its object names none.
func TestG3JudgesAnObjectWhoseOwnersAreNoCR(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", ownedByAPod)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if !strings.Contains(violation.Statement, "w-0") {
		t.Errorf("The statement is %q, want it to name w-0.", violation.Statement)
	}
}

func TestG3NotesNothingBotboxTookFromAnotherCR(t *testing.T) {
	in := twoWidgetsDeleted().
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		deletedManaged(13*time.Second, "w2-0").
		remove(13*time.Second, secondChild("w2-0", "25")).
		record(14*time.Second, secondChild("w2-0", "26", uid("uid-w2-0-again"))).
		through(21 * time.Second)

	if notes := silent(t, invariant.CleanDeletion, in).Notes; len(notes) > 0 {
		t.Errorf("G3 noted %v, and botbox took w2's child, not w's.", notes)
	}
}
