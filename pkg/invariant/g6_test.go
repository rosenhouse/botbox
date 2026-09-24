package invariant_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/invariant"
	"github.com/rosenhouse/botbox/pkg/proxy"
	"github.com/rosenhouse/botbox/pkg/target"
)

// loop repeats a request every 500ms from 1s into the run, which keeps every
// repetition inside one T_settle window.
func loop(count int, req proxy.Request) *run {
	return newRun().op(invariant.OpCreate, 0).requests(time.Second, 500*time.Millisecond, count, req)
}

func TestG6PassesAtTheThreshold(t *testing.T) {
	in := loop(errLoop, failedGet("w-0", 404)).through(8 * time.Second)

	silent(t, invariant.NoErrorLoop, in)
}

func TestG6FiresPastTheThreshold(t *testing.T) {
	in := loop(errLoop+1, failedGet("w-0", 404)).through(8 * time.Second)

	violation := fired(t, invariant.NoErrorLoop, in)

	if violation.ID != "G6" {
		t.Errorf("The violation is %q, want G6.", violation.ID)
	}
	if !strings.Contains(violation.Statement, "get") || !strings.Contains(violation.Statement, "w-0") {
		t.Errorf("The statement is %q, want it to name the request the target repeated.", violation.Statement)
	}
	if len(violation.Requests) != errLoop+1 {
		t.Fatalf("The evidence holds %d requests, want all %d of the loop.", len(violation.Requests), errLoop+1)
	}
}

func TestG6CountsServerErrorsToo(t *testing.T) {
	in := loop(errLoop+1, failedGet("w-0", 500)).through(8 * time.Second)

	fired(t, invariant.NoErrorLoop, in)
}

func TestG6IgnoresRequestsThatSucceed(t *testing.T) {
	in := loop(errLoop+1, get("w-0")).through(8 * time.Second)

	silent(t, invariant.NoErrorLoop, in)
}

// cert-manager runs six controllers that write one Certificate's status, so
// they conflict and re-read. That is the API server's optimistic-concurrency
// contract, not an error loop (DESIGN.md §6).
func TestG6IgnoresAConflictOnAnUpdateOrAPatch(t *testing.T) {
	for _, verb := range []string{"update", "patch"} {
		t.Run(verb, func(t *testing.T) {
			in := loop(errLoop+1, conflicted(verb, "w-0")).through(8 * time.Second)

			silent(t, invariant.NoErrorLoop, in)
		})
	}
}

// Only an update and a patch lose that race. A 409 on a create is
// AlreadyExists, which says the object is there: re-creating it is the error
// loop G6 exists to catch.
func TestG6CountsAConflictOnAnyOtherVerb(t *testing.T) {
	for _, verb := range []string{"create", "delete", "get"} {
		t.Run(verb, func(t *testing.T) {
			in := loop(errLoop+1, conflicted(verb, "w-0")).through(8 * time.Second)

			fired(t, invariant.NoErrorLoop, in)
		})
	}
}

// G1 ignores a watch because a watch that hangs is the target waiting. A watch
// that fails returns at once, and repeating it is a loop, so G6 counts it.
func TestG6CountsAFailingWatch(t *testing.T) {
	in := loop(errLoop+1, failedWatch(429)).through(8 * time.Second)

	fired(t, invariant.NoErrorLoop, in)
}

func TestG6CountsOneRequestAtATime(t *testing.T) {
	// An earlier run's namespace, which the target can still reconcile.
	elsewhere := failedGet("w-0", 404)
	elsewhere.Namespace = "botbox-run-0"
	for _, differs := range []struct {
		field string
		other proxy.Request
	}{
		{"name", failedGet("w-1", 404)},
		{"verb", failedDelete("w-0", 404)},
		{"resource", failedWidgetGet(404)},
		{"namespace", elsewhere},
	} {
		t.Run(differs.field, func(t *testing.T) {
			in := loop(errLoop, failedGet("w-0", 404)).
				requests(1200*time.Millisecond, 500*time.Millisecond, errLoop, differs.other).
				through(8 * time.Second)

			silent(t, invariant.NoErrorLoop, in)
		})
	}
}

func TestG6OrdersLoopsThatDifferOnlyByNamespace(t *testing.T) {
	elsewhere := failedGet("w-0", 404)
	elsewhere.Namespace = "botbox-run-0"
	in := loop(errLoop+1, failedGet("w-0", 404)).
		requests(time.Second, 500*time.Millisecond, errLoop+1, elsewhere).
		through(8 * time.Second)

	// Go ranges over a map in a random order, so one evaluation can pass by
	// chance.
	for range 100 {
		var namespaces []string
		for _, violation := range evaluate(t, invariant.NoErrorLoop, in).Violations {
			namespaces = append(namespaces, violation.Requests[0].Namespace)
		}
		if want := []string{"botbox-run-0", namespace}; !slices.Equal(namespaces, want) {
			t.Fatalf("G6 reported loops in the namespaces %v, want %v.", namespaces, want)
		}
	}
}

func TestG6IgnoresFailuresSpreadPastTheSettleTimeout(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		requests(time.Second, time.Second+100*time.Millisecond, errLoop+1, failedGet("w-0", 404)).
		through(12 * time.Second)

	silent(t, invariant.NoErrorLoop, in)
}

func TestG6IgnoresFailuresAFaultCaused(t *testing.T) {
	in := loop(errLoop+1, failedGet("w-0", 500)).
		fault(0, 6*time.Second).
		through(8 * time.Second)

	silent(t, invariant.NoErrorLoop, in)
}

func TestG6IgnoresFailuresAnOpSplits(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		requests(time.Second, 200*time.Millisecond, errLoop, failedGet("w-0", 404)).
		op(invariant.OpUpdate, 2*time.Second).
		requests(2200*time.Millisecond, 200*time.Millisecond, errLoop, failedGet("w-0", 404)).
		through(12 * time.Second)

	silent(t, invariant.NoErrorLoop, in)
}

func TestG6TakesTheThresholdSection6DefaultsWhenTheTargetDeclaresNone(t *testing.T) {
	in := loop(target.DefaultThresholds.ErrLoop, failedGet("w-0", 404)).through(40 * time.Second)
	in.Target.Thresholds = target.Thresholds{}
	in.Target.Timeouts = target.Timeouts{}

	silent(t, invariant.NoErrorLoop, in)

	in = loop(target.DefaultThresholds.ErrLoop+1, failedGet("w-0", 404)).through(40 * time.Second)
	in.Target.Thresholds = target.Thresholds{}
	in.Target.Timeouts = target.Timeouts{}

	fired(t, invariant.NoErrorLoop, in)
}

func TestG6ReadsALogTheProxyStampedOutOfOrder(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		request(30*time.Second, failedGet("w-0", 404)).
		requests(time.Second, time.Second, errLoop, failedGet("w-0", 404)).
		through(40 * time.Second)

	silent(t, invariant.NoErrorLoop, in)
}
