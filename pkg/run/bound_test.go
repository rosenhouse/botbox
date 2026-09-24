package run

import (
	"context"
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/launch"
	"github.com/rosenhouse/botbox/pkg/target"
)

// waitingHarness adds up the longest the live harness can take over each wait
// the Runner asks of it.
type waitingHarness struct {
	*fakeHarness
	timeouts   target.Timeouts
	waited     time.Duration
	stepsUntil time.Time
}

// newWaitingHarness counts the start's wait for a kubeconfig cluster's
// namespace defaults.
func newWaitingHarness(timeouts target.Timeouts) *waitingHarness {
	return &waitingHarness{fakeHarness: newFakeHarness(), timeouts: timeouts, waited: defaultNamespaceDefaultsWithin}
}

// settle waits T_settle, or until what the run owes, which can move during the
// wait.
func (w *waitingHarness) settle(ctx context.Context, owed func() time.Time) (bool, error) {
	converged, err := w.fakeHarness.settle(ctx, owed)
	w.waited += max(w.timeouts.Settle, time.Until(owed()))
	return converged, err
}

func (w *waitingHarness) sleep(ctx context.Context, d time.Duration) error {
	w.waited += d
	return w.fakeHarness.sleep(ctx, d)
}

func (w *waitingHarness) awaitCRGone(ctx context.Context, name string) error {
	w.waited += w.timeouts.Delete
	return w.fakeHarness.awaitCRGone(ctx, name)
}

func (w *waitingHarness) awaitClean(ctx context.Context, within time.Duration) (bool, error) {
	w.waited += within
	return w.fakeHarness.awaitClean(ctx, within)
}

// deleteCR records until when the teardown's steps may run.
func (w *waitingHarness) deleteCR(ctx context.Context, name string) error {
	w.stepsUntil, _ = ctx.Deadline()
	return w.fakeHarness.deleteCR(ctx, name)
}

// restart waits a grace period for the killed target to be reaped.
func (w *waitingHarness) restart(ctx context.Context) error {
	w.waited += launch.DefaultGracePeriod
	return w.fakeHarness.restart(ctx)
}

// stop gives the target a grace period after SIGTERM and another after
// SIGKILL, then deletes the namespace.
func (w *waitingHarness) stop(ctx context.Context) error {
	w.waited += 2*launch.DefaultGracePeriod + teardownMargin
	return w.fakeHarness.stop(ctx)
}

func withTimeouts(timeouts target.Timeouts) *target.Target {
	t := *toyTarget
	t.Timeouts = timeouts
	return &t
}

var (
	createOp = Op{Type: OpCreate, Obj: widget("widget")}
	updateOp = Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}}
	faultOp  = Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}}
)

// The timeouts are long beside the harness's own margins, so that a wait Bound
// leaves out shows.
func TestBoundCoversTheRunnersWaits(t *testing.T) {
	long := withTimeouts(target.Timeouts{Settle: 20 * time.Minute, Stable: 10 * time.Minute, Delete: 40 * time.Minute})
	settleOp := Op{Type: OpSettle}
	for _, test := range []struct {
		name string
		ops  []Op
	}{
		{"a create", []Op{createOp}},
		{"an update", []Op{createOp, updateOp}},
		{"a delete", []Op{createOp, {Type: OpDelete}}},
		{"a recreate", []Op{createOp, {Type: OpRecreate, Obj: widget("widget")}}},
		{"a recreate that does not settle", []Op{createOp, {Type: OpRecreate, Obj: widget("widget"), NoSettle: true}, settleOp}},
		{"a restart", []Op{createOp, {Type: OpRestart}, settleOp}},
		{"a deleteManaged", []Op{createOp, {Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(0)}}},
		{"a fault", []Op{createOp, faultOp, updateOp}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newWaitingHarness(long.Timeouts)
			h.faulting = true
			sequence := sequenceOf(test.ops...)

			if _, err := runSequence(t.Context(), long, sequence, Options{Check: &fakeChecker{}}, h); err != nil {
				t.Fatalf("The run failed: %v", err)
			}

			if bound := Bound(long, sequence); bound < h.waited {
				t.Errorf("Bound is %v, and the Runner's waits can take %v.", bound, h.waited)
			}
		})
	}
}

// A target that exits while a fault is active is owed T_settle past its
// restart, and the launcher can wait up to MaxBackoff to restart it.
func TestBoundCoversTheBackoffOfAnExitAFaultExcused(t *testing.T) {
	defaults := withTimeouts(target.DefaultTimeouts)
	h := newWaitingHarness(defaults.Timeouts)
	h.faulting, h.exitsInWait, h.restartsIn = true, 2, launch.MaxBackoff
	sequence := sequenceOf(createOp, faultOp, updateOp)

	if _, err := runSequence(t.Context(), defaults, sequence, Options{Check: &fakeChecker{}}, h); err != nil {
		t.Fatalf("The run failed: %v", err)
	}

	if len(h.exited) != 1 {
		t.Fatalf("The target exited %d times, want once, during the update's wait.", len(h.exited))
	}
	if bound := Bound(defaults, sequence); bound < h.waited {
		t.Errorf("Bound is %v, and the Runner's waits can take %v.", bound, h.waited)
	}
}

// The teardown's steps may run until T_stable, T_delete and 30s past where its
// quiet window opens, which is what Bound gives them.
func TestTheTeardownsStepsRunOnABudget(t *testing.T) {
	h := newWaitingHarness(testTimeouts)

	result, err := runSequence(t.Context(), toyTarget, sequenceOf(createOp), Options{Check: &fakeChecker{}}, h)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	budget := testTimeouts.Stable + testTimeouts.Delete + 30*time.Second
	if steps := h.stepsUntil.Sub(result.Timeline.Quiet.Start); steps > budget || steps < budget-time.Second {
		t.Errorf("The teardown's steps had %v, want %v.", steps, budget)
	}
}

func TestBoundAddsWhatEachOpCanWait(t *testing.T) {
	timeouts := target.DefaultTimeouts
	defaults := withTimeouts(timeouts)
	alone := Bound(defaults, sequenceOf(createOp))
	for _, test := range []struct {
		name string
		op   Op
		adds time.Duration
	}{
		{"an update", updateOp, timeouts.Settle},
		{"an update that does not settle", Op{Type: OpUpdate, Patch: updateOp.Patch, NoSettle: true}, 0},
		// The CR may take T_delete to go, and T_settle after that to settle.
		{"a delete", Op{Type: OpDelete}, timeouts.Delete + timeouts.Settle},
		{"a delete that does not settle", Op{Type: OpDelete, NoSettle: true}, 0},
		{"a recreate", Op{Type: OpRecreate, Obj: widget("widget")}, timeouts.Delete + timeouts.Settle},
		{"a recreate that does not settle", Op{Type: OpRecreate, Obj: widget("widget"), NoSettle: true}, timeouts.Delete},
		{"a restart", Op{Type: OpRestart}, launch.DefaultGracePeriod},
		{"a deleteManaged", Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(0)}, timeouts.Settle},
		{"a settle", Op{Type: OpSettle}, timeouts.Settle},
	} {
		t.Run(test.name, func(t *testing.T) {
			if adds := Bound(defaults, sequenceOf(createOp, test.op)) - alone; adds != test.adds {
				t.Errorf("Bound adds %v for %s, want %v.", adds, test.name, test.adds)
			}
		})
	}
}

// At the default timeouts, a create alone can take 30s to start, 30s to settle, and
// 10s of quiet, 60s for the CR to go and 30s more in the teardown. Stopping the
// target takes up to 10s and deleting the namespace 30s.
func TestBoundOfACreateAlone(t *testing.T) {
	if bound, want := Bound(withTimeouts(target.DefaultTimeouts), sequenceOf(createOp)), 200*time.Second; bound != want {
		t.Errorf("Bound is %v, want %v.", bound, want)
	}
}

// A target is owed as long as the faults lasted, T_settle, and T_settle past
// the restart of an exit a fault excused. So each fault doubles the time before
// the teardown and adds twice T_settle and the longest backoff.
func TestBoundDoublesTheRunForEachFault(t *testing.T) {
	defaults := withTimeouts(target.DefaultTimeouts)
	const teardown = 140 * time.Second
	for _, test := range []struct {
		name string
		ops  []Op
		want time.Duration
	}{
		// The start, the create and the update take 90s.
		{"one fault", []Op{createOp, faultOp, updateOp}, 2*90*time.Second + 60*time.Second + launch.MaxBackoff + teardown},
		{"two faults", []Op{createOp, faultOp, faultOp, updateOp},
			2*(2*90*time.Second+60*time.Second+launch.MaxBackoff) + 60*time.Second + launch.MaxBackoff + teardown},
	} {
		t.Run(test.name, func(t *testing.T) {
			if bound := Bound(defaults, sequenceOf(test.ops...)); bound != test.want {
				t.Errorf("Bound is %v, want %v.", bound, test.want)
			}
		})
	}
}
