package invariant_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/invariant"
)

func TestG1PassesWhenOnlyWatchesAndLeaseWritesRemain(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		checkpoint(2*time.Second, invariant.Converged).
		request(6*time.Second, watch()).
		request(6500*time.Millisecond, leaseUpdate()).
		through(8 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

func TestG1FiresOnARequestInTheQuietWindow(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		request(6*time.Second, get("w-0")).
		through(8 * time.Second)

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

func TestG1IgnoresTrafficBeforeTheSettleTimeoutHasPassed(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		requests(0, time.Second, 5, get("w-0")).
		through(8 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

func TestG1IgnoresAWindowTheRunDidNotReach(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		request(6*time.Second, get("w-0")).
		through(6500 * time.Millisecond)

	silent(t, invariant.BoundedReconciliation, in)
}

func TestG1IgnoresAWindowAnotherOpCutShort(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		op(invariant.OpUpdate, 4*time.Second).
		request(6*time.Second, get("w-0")).
		through(12 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

func TestG1IgnoresAWindowAFaultReachedInto(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		fault(1*time.Second, 3*time.Second).
		request(6*time.Second, get("w-0")).
		through(8 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

func TestG1CountsEveryNonWatchRequestInTheWindow(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		requests(5*time.Second, 200*time.Millisecond, 10, get("w-0")).
		through(8 * time.Second)

	violation := fired(t, invariant.BoundedReconciliation, in)

	if !strings.Contains(violation.Statement, "10") {
		t.Errorf("The statement is %q, want it to count the 10 requests.", violation.Statement)
	}
}

func TestG1IgnoresAWindowTheTeardownReachedInto(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		teardown(5500*time.Millisecond).
		request(6*time.Second, get("w-0")).
		through(8 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}
