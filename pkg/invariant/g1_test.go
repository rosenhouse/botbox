package invariant_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/invariant"
)

// G1 ignores a watch whether it hung or failed; G6 counts the one that failed.
func TestG1PassesWhenOnlyWatchesAndLeaseWritesRemain(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, watch()).
		request(3200*time.Millisecond, failedWatch(429)).
		request(3500*time.Millisecond, leaseUpdate()).
		through(14 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

// The window opens where the settle wait ended, which for a target that
// converges is well inside T_settle (DESIGN.md §6).
func TestG1FiresOnARequestInTheQuietWindow(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, get("w-0")).
		through(14 * time.Second)

	violation := fired(t, invariant.BoundedReconciliation, in)

	if violation.ID != "G1" {
		t.Errorf("The violation is %q, want G1.", violation.ID)
	}
	if len(violation.Requests) != 1 || violation.Requests[0].Name != "w-0" {
		t.Fatalf("The evidence holds %v, want the get of w-0.", violation.Requests)
	}
	if !strings.Contains(violation.Statement, "op 0") {
		t.Errorf("The statement is %q, want it to name the op the window follows.", violation.Statement)
	}
}

// A settle wait that expires ends T_settle after the op, and the window
// follows it there.
func TestG1FiresOnARequestAfterAnExpiredSettle(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(settleTimeout, invariant.Expired).
		request(settleTimeout+time.Second, get("w-0")).
		through(14 * time.Second)

	fired(t, invariant.BoundedReconciliation, in)
}

// A target still working towards convergence is not yet held to §6's quiet.
func TestG1IgnoresTrafficBeforeTheSettleWaitEnded(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		requests(0, 400*time.Millisecond, 5, get("w-0")).
		settled(2*time.Second, invariant.Converged).
		through(14 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

// T_settle after the op is not itself a window: a run whose settle wait has
// not ended has nothing to judge.
func TestG1JudgesNothingUntilASettleWaitEnds(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		request(settleTimeout+time.Second, get("w-0")).
		through(14 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

func TestG1IgnoresAWindowTheRunDidNotReach(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, get("w-0")).
		through(3500 * time.Millisecond)

	silent(t, invariant.BoundedReconciliation, in)
}

func TestG1IgnoresAWindowAnotherOpCutShort(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		op(invariant.OpUpdate, 3*time.Second).
		request(3500*time.Millisecond, get("w-0")).
		through(14 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

// A fault anywhere between the op and the window's close makes the reaction
// the fault's, not the target's.
func TestG1IgnoresAWindowAFaultPreceded(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		fault(1*time.Second, 1500*time.Millisecond).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, get("w-0")).
		through(14 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

func TestG1CountsEveryNonWatchRequestInTheWindow(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		requests(2100*time.Millisecond, 150*time.Millisecond, 10, get("w-0")).
		through(14 * time.Second)

	violation := fired(t, invariant.BoundedReconciliation, in)

	if !strings.Contains(violation.Statement, "10") {
		t.Errorf("The statement is %q, want it to count the 10 requests.", violation.Statement)
	}
}

func TestG1IgnoresAWindowTheTeardownReachedInto(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		checkpoint(2*time.Second, invariant.Converged).
		teardown(3*time.Second).
		request(3500*time.Millisecond, get("w-0")).
		through(14 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}
