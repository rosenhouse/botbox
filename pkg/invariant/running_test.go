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
