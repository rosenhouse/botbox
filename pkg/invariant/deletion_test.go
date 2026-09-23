package invariant_test

import (
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/invariant"
)

// beingDeleted is a converged Widget that botbox deletes at 10s, recorded under
// deletion at 10.1s. Its deletion deadline is 20.1s.
func beingDeleted() *run {
	return converged().
		op(invariant.OpDelete, 10*time.Second).
		record(10100*time.Millisecond, deletedWidget("15", finalizers(cleanup)))
}

func TestG4LeavesToG3AWaitThatEndedOnACRPastItsDeletionDeadline(t *testing.T) {
	for name, in := range map[string]invariant.Input{
		"the CR is still there": beingDeleted().
			checkpoint(22100*time.Millisecond, invariant.Expired).
			through(25 * time.Second),
		"the CR went after its deadline": beingDeleted().
			remove(21*time.Second, deletedWidget("16")).
			checkpoint(22100*time.Millisecond, invariant.Expired).
			through(25 * time.Second),
		"a later wait on a CR still there": beingDeleted().
			checkpoint(22100*time.Millisecond, invariant.Expired).
			op(invariant.OpSettle, 25*time.Second).
			checkpoint(30*time.Second, invariant.Expired).
			through(32 * time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			silent(t, invariant.Convergence, in)
		})
	}
}

func TestG4JudgesAWaitThatEndedOnACRThatWentByItsDeletionDeadline(t *testing.T) {
	for name, in := range map[string]invariant.Input{
		"in time, and a child kept changing": beingDeleted().
			remove(12*time.Second, deletedWidget("16")).
			record(13500*time.Millisecond, child("w-2", "17")).
			record(14500*time.Millisecond, child("w-2", "18")).
			checkpoint(15*time.Second, invariant.Expired).
			through(17 * time.Second),
		"late, but before the wait began": beingDeleted().
			checkpoint(22100*time.Millisecond, invariant.Expired).
			remove(23*time.Second, deletedWidget("16")).
			op(invariant.OpSettle, 25*time.Second).
			record(28*time.Second, child("w-2", "17")).
			record(29*time.Second, child("w-2", "18")).
			checkpoint(30*time.Second, invariant.Expired).
			through(32 * time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			violation := fired(t, invariant.Convergence, in)

			requireStatement(t, violation, "no CR was left to be ready, but the namespace never held still")
		})
	}
}

func TestDeletionOwedIsTStablePastTheDeletionsEnd(t *testing.T) {
	for _, test := range []struct {
		name string
		run  *run
		at   time.Duration
		// want is zero where no deletion is owed.
		want time.Duration
	}{
		{name: "no deletion", run: converged(), at: 12 * time.Second},
		{name: "a deletion recorded after the instant asked about", run: beingDeleted(), at: 10 * time.Second},
		{name: "a CR still being deleted", run: beingDeleted(), at: 12 * time.Second, want: 22100 * time.Millisecond},
		{name: "a CR that went before its deadline", run: beingDeleted().remove(13*time.Second, deletedWidget("16")),
			at: 14 * time.Second, want: 15 * time.Second},
		{name: "a CR that went after the instant asked about", run: beingDeleted().remove(13*time.Second, deletedWidget("16")),
			at: 12 * time.Second, want: 22100 * time.Millisecond},
		{name: "a CR that went after its deadline", run: beingDeleted().remove(21*time.Second, deletedWidget("16")),
			at: 25 * time.Second, want: 22100 * time.Millisecond},
		{name: "a CR deleted twice", run: beingDeleted().remove(13*time.Second, deletedWidget("16")).
			op(invariant.OpCreate, 15*time.Second).
			record(15100*time.Millisecond, widget("20", uid("uid-w-again"), spec(2), finalizers(cleanup))).
			op(invariant.OpDelete, 16*time.Second).
			record(16100*time.Millisecond, widget("21", uid("uid-w-again"), spec(2), finalizers(cleanup), deleting(16*time.Second))),
			at: 17 * time.Second, want: 28100 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := test.run.through(40 * time.Second)

			got := in.DeletionOwed(at(test.at))

			if test.want == 0 && !got.IsZero() {
				t.Errorf("The run owes a deletion until %v, want nothing.", got.Sub(epoch))
			}
			if test.want != 0 && !got.Equal(at(test.want)) {
				t.Errorf("The run owes a deletion until %v, want %v.", got.Sub(epoch), test.want)
			}
		})
	}
}
