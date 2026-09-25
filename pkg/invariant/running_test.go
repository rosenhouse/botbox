package invariant_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/invariant"
)

func TestBackIsTheFirstRequestThatShowsTheTargetRunning(t *testing.T) {
	for _, test := range []struct {
		name string
		run  *run
		want time.Duration
	}{
		{"a watch", newRun().request(1500*time.Millisecond, watch()), 1500 * time.Millisecond},
		{"a get the API server refused", newRun().request(1500*time.Millisecond, failedGet("w-0", http.StatusNotFound)), 1500 * time.Millisecond},
		{"the earliest, though logged last", newRun().
			request(1700*time.Millisecond, watch()).
			request(1600*time.Millisecond, get("w-0")), 1600 * time.Millisecond},
		{"one after leader election and discovery", newRun().
			request(1100*time.Millisecond, lease("get")).
			request(1200*time.Millisecond, leaseCandidate("create")).
			request(1300*time.Millisecond, nonResource("/api")).
			request(1400*time.Millisecond, watch()), 1400 * time.Millisecond},
		{"one after requests up to since", newRun().
			request(500*time.Millisecond, get("w-0")).
			request(time.Second, get("w-0")).
			request(1400*time.Millisecond, watch()), 1400 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			back, found := invariant.Back(test.run.in.Requests, at(time.Second))

			if !found || !back.Equal(at(test.want)) {
				t.Errorf("Back returned (%v, %t), want %v.", back, found, at(test.want))
			}
		})
	}
}

// botbox chose to restart the target, so a wait gives it T_settle past its
// return, where it returned within T_settle.
func TestWaitOwedRunsPastTheReturnFromARestartOp(t *testing.T) {
	restarted := func() *run { return newRun().running(time.Second).op(invariant.OpRestart, 3*time.Second) }
	for _, test := range []struct {
		name string
		run  *run
		at   time.Duration
		// want is zero where nothing is owed.
		want time.Duration
	}{
		{name: "back", run: restarted().running(6500 * time.Millisecond), at: 7 * time.Second, want: 11500 * time.Millisecond},
		{name: "not back", run: restarted(), at: 7 * time.Second, want: 8 * time.Second},
		{name: "with leader election alone", run: restarted().requests(3100*time.Millisecond, time.Second, 4, lease("update")),
			at: 7 * time.Second, want: 8 * time.Second},
		{name: "back only T_settle after it", run: restarted().running(8 * time.Second), at: 9 * time.Second, want: 8 * time.Second},
		{name: "a restart at the instant asked about", run: restarted().running(3500 * time.Millisecond), at: 3 * time.Second},
		{name: "a restart and a later op", run: restarted().running(6500*time.Millisecond).op(invariant.OpUpdate, 7*time.Second),
			at: 8 * time.Second, want: 11500 * time.Millisecond},
		{name: "two restarts", run: restarted().running(3500*time.Millisecond).op(invariant.OpRestart, 5*time.Second).running(9 * time.Second),
			at: 10 * time.Second, want: 14 * time.Second},
		{name: "an exit no fault excused", run: newRun().running(time.Second).exit(3*time.Second, 3*time.Second).running(3500 * time.Millisecond),
			at: 4 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := test.run.through(20 * time.Second).WaitOwed(at(test.at))

			if test.want == 0 && !got.IsZero() {
				t.Errorf("The wait is owed until %v, want nothing.", got.Sub(epoch))
			}
			if test.want != 0 && !got.Equal(at(test.want)) {
				t.Errorf("The wait is owed until %v, want %v.", got.Sub(epoch), test.want)
			}
		})
	}
}

func TestBackFindsNothingUntilTheTargetShowsItRuns(t *testing.T) {
	for name, r := range map[string]*run{
		"no request":            newRun(),
		"leader election alone": newRun().request(1100*time.Millisecond, lease("update")).request(1200*time.Millisecond, leaseCandidate("list")),
		"discovery alone":       newRun().request(1100*time.Millisecond, nonResource("/apis")),
		"requests up to since":  newRun().request(500*time.Millisecond, watch()).request(time.Second, watch()),
	} {
		t.Run(name, func(t *testing.T) {
			if back, found := invariant.Back(r.in.Requests, at(time.Second)); found {
				t.Errorf("Back found the target back at %v, want nothing.", back)
			}
		})
	}
}
