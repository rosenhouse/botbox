package invariant_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/invariant"
	"github.com/rosenhouse/botbox/pkg/proxy"
)

// childDeleted is the toy converged with two children, and botbox deleting
// w-0 behind its back at 10s.
func childDeleted() *run {
	return converged().
		deletedManaged(10*time.Second, "w-0").
		remove(10100*time.Millisecond, child("w-0", "15"))
}

func TestG7PassesWhenTheTargetRecreatesTheObject(t *testing.T) {
	in := childDeleted().
		record(10200*time.Millisecond, child("w-0", "16", uid("uid-w-0-again"))).
		checkpoint(12200*time.Millisecond, invariant.Converged).
		through(12200 * time.Millisecond)

	if notes := silent(t, invariant.SelfHealing, in).Notes; len(notes) > 0 {
		t.Errorf("G7 noted %v, want a verdict.", notes)
	}
}

func TestG7FiresOnAnObjectThatNeverCameBack(t *testing.T) {
	in := childDeleted().
		checkpoint(12100*time.Millisecond, invariant.Converged).
		waitBegan(10100 * time.Millisecond).
		through(12100 * time.Millisecond)

	violation := fired(t, invariant.SelfHealing, in)

	if violation.ID != "G7" {
		t.Errorf("The violation is %q, want G7.", violation.ID)
	}
	for _, want := range []string{
		"v1/ConfigMap w-0", "op 1 (deleteManaged)", "never came back within the 2s the run waited",
		"does not list v1/ConfigMap under notRecreated",
	} {
		if !strings.Contains(violation.Statement, want) {
			t.Errorf("The statement is %q, want it to say %q.", violation.Statement, want)
		}
	}
	if got := state(violation); managed(violation) != "1" || !slices.Equal(got, []string{"w-1"}) {
		t.Errorf("The violation quotes %s managed objects %v, want w-1 alone.", managed(violation), got)
	}
	if !violation.At.Equal(at(12100 * time.Millisecond)) {
		t.Errorf("G7 judged at %v, want where the wait after the op ended.", violation.At)
	}
	if want := timelineOf(configMapGVK, "w-0"); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
	if n := len(violation.Versions); n == 0 || !violation.Versions[n-1].Deleted {
		t.Errorf("The timeline holds %v, want it to end where botbox deleted w-0.", quoted(violation))
	}
}

// The wait is where the target had to have acted, not the end of the run.
func TestG7FiresOnAnObjectRecreatedOnlyAfterTheWaitEnded(t *testing.T) {
	in := childDeleted().
		checkpoint(12100*time.Millisecond, invariant.Converged).
		record(13*time.Second, child("w-0", "16", uid("uid-w-0-again"))).
		through(20 * time.Second)

	violation := fired(t, invariant.SelfHealing, in)

	if n := len(violation.Versions); n == 0 || !violation.Versions[n-1].Deleted {
		t.Errorf("The timeline holds %v, want it to end at the verdict, where w-0 was deleted.", quoted(violation))
	}
}

func TestG7ExemptsOnlyTheKindsTheTargetDoesNotRecreate(t *testing.T) {
	both := func() *run {
		r := newRunManaging(configMapGVK, secretGVK)
		r.in.Target.NotRecreated = []schema.GroupVersionKind{secretGVK}
		return r.
			op(invariant.OpCreate, 0).
			record(100*time.Millisecond, widget("11", spec(1), status(1, 1))).
			record(200*time.Millisecond, child("w-0", "12"), secret("w-0", "13")).
			checkpoint(2200*time.Millisecond, invariant.Converged)
	}
	t.Run("the kind listed", func(t *testing.T) {
		in := both().
			deletedManagedOf(10*time.Second, secretGVK, "w-0").
			remove(10100*time.Millisecond, secret("w-0", "14")).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			through(12100 * time.Millisecond)

		if notes := silent(t, invariant.SelfHealing, in).Notes; len(notes) > 0 {
			t.Errorf("G7 noted %v, want the target's declaration to settle it.", notes)
		}
	})
	t.Run("a kind not listed", func(t *testing.T) {
		in := both().
			deletedManaged(10*time.Second, "w-0").
			remove(10100*time.Millisecond, child("w-0", "14")).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			through(12100 * time.Millisecond)

		fired(t, invariant.SelfHealing, in)
	})
	t.Run("a kind of the same name in another group", func(t *testing.T) {
		r := childDeleted()
		r.in.Target.NotRecreated = []schema.GroupVersionKind{{Group: "example.com", Version: "v1", Kind: "ConfigMap"}}
		in := r.
			checkpoint(12100*time.Millisecond, invariant.Converged).
			through(12100 * time.Millisecond)

		fired(t, invariant.SelfHealing, in)
	})
}

// A wait that expired is still where the target had to have acted.
func TestG7FiresWhereTheWaitExpired(t *testing.T) {
	in := childDeleted().
		checkpoint(15*time.Second, invariant.Expired).
		through(15 * time.Second)

	fired(t, invariant.SelfHealing, in)
}

// An object that is back satisfies G7 whatever else happened.
func TestG7PassesAnObjectThatCameBack(t *testing.T) {
	for _, c := range []struct {
		name string
		in   invariant.Input
	}{
		{"after a fault", childDeleted().
			fault(10050*time.Millisecond, 11*time.Second).
			record(11500*time.Millisecond, child("w-0", "16", uid("uid-w-0-again"))).
			checkpoint(13500*time.Millisecond, invariant.Converged).
			through(13500 * time.Millisecond)},
		{"after an update", converged().
			op(invariant.OpUpdate, 10*time.Second).
			record(10050*time.Millisecond, widget("20", spec(2), generation(2), status(2, 1), finalizers(cleanup))).
			deletedManaged(10100*time.Millisecond, "w-0").
			remove(10150*time.Millisecond, child("w-0", "21")).
			record(10200*time.Millisecond, child("w-0", "22", uid("uid-w-0-again"))).
			record(10300*time.Millisecond, widget("23", spec(2), generation(2), status(2, 2), finalizers(cleanup))).
			checkpoint(12300*time.Millisecond, invariant.Converged).
			through(12300 * time.Millisecond)},
		{"after a restart", converged().
			op(invariant.OpRestart, 9*time.Second).
			deletedManaged(10*time.Second, "w-0").
			remove(10100*time.Millisecond, child("w-0", "15")).
			record(11*time.Second, child("w-0", "16", uid("uid-w-0-again"))).
			checkpoint(13*time.Second, invariant.Converged).
			through(13 * time.Second)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if notes := silent(t, invariant.SelfHealing, c.in).Notes; len(notes) > 0 {
				t.Errorf("G7 noted %v, want the object's return to settle it.", notes)
			}
		})
	}
}

// botbox cannot tell when a restarted target is back. One still starting, or
// waiting out its predecessor's lease, recreates nothing.
func TestG7NotesAnObjectDeletedBeforeARestartedTargetWasBack(t *testing.T) {
	const wantNote = "G7 is not evaluated for op 2 (deleteManaged): the target had requested no resource outside leader election between op 1 (restart) and it"
	deletedAfter := func(r *run) invariant.Input {
		return r.
			deletedManaged(10*time.Second, "w-0").
			remove(10100*time.Millisecond, child("w-0", "15")).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			through(12100 * time.Millisecond)
	}
	restarted := func() *run { return converged().op(invariant.OpRestart, 9*time.Second) }
	for _, c := range []struct {
		name string
		in   invariant.Input
		want string
	}{
		{"with no request", deletedAfter(restarted()), wantNote},
		{"with lease requests alone", deletedAfter(restarted().requests(9100*time.Millisecond, 500*time.Millisecond, 4, lease("update"))), wantNote},
		{"with lease candidate requests alone", deletedAfter(restarted().
			request(9100*time.Millisecond, leaseCandidate("list")).
			request(9200*time.Millisecond, leaseCandidate("create"))), wantNote},
		{"with discovery reads alone", deletedAfter(restarted().request(9100*time.Millisecond, nonResource("/api"))), wantNote},
		{"with a request before the restart alone", deletedAfter(restarted().request(8500*time.Millisecond, get("w-1"))), wantNote},
		{"with a request in the wait alone", deletedAfter(restarted().request(10500*time.Millisecond, watch())), wantNote},
		{"with a request before the last restart alone", deletedAfter(converged().
			op(invariant.OpRestart, 8*time.Second).
			request(8500*time.Millisecond, get("w-1")).
			op(invariant.OpRestart, 9*time.Second)),
			"G7 is not evaluated for op 3 (deleteManaged): the target had requested no resource outside leader election between op 2 (restart) and it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			noted(t, invariant.SelfHealing, c.in, c.want)
		})
	}
}

func TestG7JudgesAnObjectDeletedOnceARestartedTargetWasBack(t *testing.T) {
	for _, c := range []struct {
		name    string
		request proxy.Request
	}{
		{"with a watch", watch()},
		{"with a resource named leases in another group", leasesElsewhere()},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := converged().
				op(invariant.OpRestart, 9*time.Second).
				request(9500*time.Millisecond, c.request).
				deletedManaged(10*time.Second, "w-0").
				remove(10100*time.Millisecond, child("w-0", "15")).
				checkpoint(12100*time.Millisecond, invariant.Converged).
				through(12100 * time.Millisecond)

			fired(t, invariant.SelfHealing, in)
		})
	}
}

// An index that resolved to nothing deleted nothing, which the Runner notes.
func TestG7IgnoresAnOpThatDeletedNothing(t *testing.T) {
	in := converged().
		op(invariant.OpDeleteManaged, 10*time.Second).
		checkpoint(12*time.Second, invariant.Converged).
		through(12 * time.Second)

	if notes := silent(t, invariant.SelfHealing, in).Notes; len(notes) > 0 {
		t.Errorf("G7 noted %v, want nothing: the op deleted nothing.", notes)
	}
}

// A run that stopped inside the wait never reached the instant G7 judges.
func TestG7IgnoresAnOpWhoseWaitNeverEnded(t *testing.T) {
	in := childDeleted().through(12 * time.Second)

	silent(t, invariant.SelfHealing, in)
}

// A fault that ends while botbox deletes the object can still hide the deletion
// from the target.
func TestG7NotesAnObjectAFaultMayHaveKeptAway(t *testing.T) {
	const wantNote = "G7 is not evaluated for op 1 (deleteManaged): a fault was active during it or the wait after it"
	for _, c := range []struct {
		name string
		in   invariant.Input
	}{
		{"in the wait", childDeleted().
			fault(10050*time.Millisecond, 11*time.Second).
			checkpoint(16*time.Second, invariant.Expired).
			through(16 * time.Second)},
		{"during the op alone", childDeleted().
			fault(9*time.Second, 10050*time.Millisecond).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			waitBegan(10100 * time.Millisecond).
			through(12100 * time.Millisecond)},
	} {
		t.Run(c.name, func(t *testing.T) {
			noted(t, invariant.SelfHealing, c.in, wantNote)
		})
	}
}

// The update scales the toy down to one child before it settles, so the
// target meant to delete w-1 itself.
func TestG7NotesAnObjectDeletedBeforeTheRunConverged(t *testing.T) {
	in := converged().
		op(invariant.OpUpdate, 10*time.Second).
		record(10050*time.Millisecond, widget("20", spec(1), generation(2), status(2, 1), finalizers(cleanup))).
		deletedManaged(10100*time.Millisecond, "w-1").
		remove(10150*time.Millisecond, child("w-1", "21")).
		record(10200*time.Millisecond, widget("22", spec(1), generation(2), status(1, 2), finalizers(cleanup))).
		checkpoint(12200*time.Millisecond, invariant.Converged).
		through(12200 * time.Millisecond)

	noted(t, invariant.SelfHealing, in, "G7 is not evaluated for op 2 (deleteManaged): the run had not converged since op 1 (update)")
}

// botbox deleted the CR without waiting, and the Observer saw it go only after
// the op had deleted the object.
func TestG7ReadsTheCRWhereTheWaitEnds(t *testing.T) {
	in := converged().
		record(300*time.Millisecond, child("kept", "13", orphaned)).
		op(invariant.OpDelete, 5*time.Second).
		deletedManaged(5050*time.Millisecond, "kept").
		record(5100*time.Millisecond, widget("15", spec(2), status(2, 1), deleting(5*time.Second), finalizers(cleanup))).
		remove(5150*time.Millisecond, child("kept", "16", orphaned)).
		checkpoint(7150*time.Millisecond, invariant.Converged).
		through(7150 * time.Millisecond)

	if notes := silent(t, invariant.SelfHealing, in).Notes; len(notes) > 0 {
		t.Errorf("G7 noted %v, want nothing: the CR was going when the wait ended.", notes)
	}
}

// A target owes nothing to a CR that botbox deleted.
func TestG7IgnoresAnObjectDeletedWhileTheCRWasGoing(t *testing.T) {
	for _, c := range []struct {
		name string
		cr   func(*run) *run
	}{
		{"gone", func(r *run) *run { return r.remove(5500*time.Millisecond, deletedWidget("15")) }},
		{"under deletion", func(r *run) *run { return r.record(5500*time.Millisecond, deletedWidget("15", finalizers(cleanup))) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := converged().
				record(300*time.Millisecond, child("kept", "13", orphaned)).
				op(invariant.OpDelete, 5*time.Second)
			in := c.cr(r).
				checkpoint(7500*time.Millisecond, invariant.Converged).
				deletedManaged(10*time.Second, "kept").
				remove(10100*time.Millisecond, child("kept", "16", orphaned)).
				checkpoint(12100*time.Millisecond, invariant.Converged).
				through(12100 * time.Millisecond)

			if notes := silent(t, invariant.SelfHealing, in).Notes; len(notes) > 0 {
				t.Errorf("G7 noted %v, want nothing: there was no CR to restore the object for.", notes)
			}
		})
	}
}
