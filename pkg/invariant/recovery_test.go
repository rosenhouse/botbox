package invariant_test

import (
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/invariant"
)

func TestOwedIsAsLongAfterTheFaultsAsTheyLastedAndTSettleMore(t *testing.T) {
	for _, test := range []struct {
		name string
		run  *run
		at   time.Duration
		// want is zero where the target owes nothing.
		want time.Duration
	}{
		{name: "no fault", run: newRun(), at: 10 * time.Second},
		{name: "a fault still active", run: newRun().fault(time.Second, 20*time.Second), at: 10 * time.Second},
		{name: "a fault that stopped", run: newRun().fault(time.Second, 4*time.Second), at: 5 * time.Second,
			want: 12 * time.Second},
		{name: "a fault that had not stopped by then", run: newRun().fault(time.Second, 4*time.Second), at: 3 * time.Second},
		{name: "a fault that stopped while another stayed active",
			run: newRun().fault(time.Second, 20*time.Second).fault(8*time.Second, 9*time.Second),
			at:  10 * time.Second, want: 15 * time.Second},
		{name: "overlapping faults",
			run: newRun().fault(2*time.Second, 4*time.Second).fault(time.Second, 3*time.Second).
				fault(3*time.Second, 4500*time.Millisecond).fault(2500*time.Millisecond, 3500*time.Millisecond),
			at: 5 * time.Second, want: 13 * time.Second},
		{name: "faults either side of a convergence",
			run: newRun().op(invariant.OpCreate, 0).fault(time.Second, 4*time.Second).
				checkpoint(6*time.Second, invariant.Converged).fault(8*time.Second, 9*time.Second),
			at: 10 * time.Second, want: 15 * time.Second},
		{name: "a fault the target converged after",
			run: newRun().op(invariant.OpCreate, 0).fault(time.Second, 4*time.Second).
				checkpoint(6*time.Second, invariant.Converged),
			at: 10 * time.Second},
		{name: "a fault the target converged during",
			run: newRun().op(invariant.OpCreate, 0).fault(time.Second, 8*time.Second).
				checkpoint(6*time.Second, invariant.Converged),
			at: 10 * time.Second, want: 15 * time.Second},
		{name: "a convergence at the instant asked about",
			run: newRun().op(invariant.OpCreate, 0).fault(time.Second, 4*time.Second).
				checkpoint(6*time.Second, invariant.Converged),
			at: 6 * time.Second},
		{name: "a convergence after the instant asked about",
			run: newRun().op(invariant.OpCreate, 0).fault(time.Second, 4*time.Second).
				checkpoint(6*time.Second, invariant.Converged),
			at: 5 * time.Second, want: 12 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := test.run.through(30 * time.Second)

			got := in.Owed(at(test.at))

			if test.want == 0 && !got.IsZero() {
				t.Errorf("The target owes recovery until %v, want nothing.", got.Sub(epoch))
			}
			if test.want != 0 && !got.Equal(at(test.want)) {
				t.Errorf("The target owes recovery until %v, want %v.", got.Sub(epoch), test.want)
			}
		})
	}
}

func TestRecoveringIsWhileAFaultIsActiveOrOwed(t *testing.T) {
	in := newRun().fault(time.Second, 4*time.Second).through(30 * time.Second)

	for _, test := range []struct {
		at   time.Duration
		want bool
	}{
		{at: 500 * time.Millisecond, want: false},
		{at: 2 * time.Second, want: true},
		{at: 11900 * time.Millisecond, want: true},
		{at: 12 * time.Second, want: false},
	} {
		if got := in.Recovering(at(test.at)); got != test.want {
			t.Errorf("Recovering at %v is %t, want %t.", test.at, got, test.want)
		}
	}
}
