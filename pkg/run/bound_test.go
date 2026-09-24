package run

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/launch"
	"github.com/rosenhouse/botbox/pkg/proxy"
	"github.com/rosenhouse/botbox/pkg/target"
)

// waitingHarness runs the Runner on a clock that each wait moves by the
// longest the live harness can take over it.
type waitingHarness struct {
	*fakeHarness
	timeouts target.Timeouts
	began    time.Time
	at       time.Time
	// runsOut names, in turn, the wait at whose end the next fault's trigger
	// runs out.
	runsOut []int
	ranOut  int
}

func newWaitingHarness(timeouts target.Timeouts) *waitingHarness {
	began := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	w := &waitingHarness{fakeHarness: newFakeHarness(), timeouts: timeouts, began: began, at: began}
	w.clock = func() time.Time { return w.at }
	w.faulting = true
	return w
}

// waited counts the start's wait for a kubeconfig cluster's namespace
// defaults too.
func (w *waitingHarness) waited() time.Duration {
	return defaultNamespaceDefaultsWithin + w.at.Sub(w.began)
}

// settle waits T_settle, or until what the run owes, which can move during the
// wait. A target that exits in the wait exits a moment into it.
func (w *waitingHarness) settle(ctx context.Context, owed func() time.Time) (bool, error) {
	began := w.at
	w.at = w.at.Add(time.Millisecond)
	converged, err := w.fakeHarness.settle(ctx, owed)
	for {
		deadline := began.Add(w.timeouts.Settle)
		if owes := owed(); owes.After(deadline) {
			deadline = owes
		}
		if deadline.After(w.at) {
			w.at = deadline
			continue
		}
		if len(w.runsOut) == 0 || w.runsOut[0] != w.waits {
			return converged, err
		}
		w.runsOut = w.runsOut[1:]
		w.remove(proxy.FaultID(w.ranOut))
		w.ranOut++
	}
}

func (w *waitingHarness) sleep(ctx context.Context, d time.Duration) error {
	w.at = w.at.Add(d)
	return w.fakeHarness.sleep(ctx, d)
}

func (w *waitingHarness) awaitCRGone(ctx context.Context, name string) error {
	w.at = w.at.Add(w.timeouts.Delete)
	return w.fakeHarness.awaitCRGone(ctx, name)
}

func (w *waitingHarness) awaitClean(ctx context.Context, within time.Duration) (bool, error) {
	w.at = w.at.Add(within)
	return w.fakeHarness.awaitClean(ctx, within)
}

// restart waits a grace period for the killed target to be reaped.
func (w *waitingHarness) restart(ctx context.Context) error {
	w.at = w.at.Add(launch.DefaultGracePeriod)
	return w.fakeHarness.restart(ctx)
}

// stop gives the target a grace period after SIGTERM and another after
// SIGKILL, then deletes the namespace.
func (w *waitingHarness) stop(ctx context.Context) error {
	w.at = w.at.Add(2*launch.DefaultGracePeriod + teardownMargin)
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
	settleOp = Op{Type: OpSettle}
	faultOp  = Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}}
)

func faultUntil(op int) Op {
	return Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Op: &op}}}
}

func TestBoundCoversTheRunnersWaits(t *testing.T) {
	// Long beside the harness's own margins, so that a wait Bound leaves out
	// shows.
	long := target.Timeouts{Settle: 20 * time.Minute, Stable: 10 * time.Minute, Delete: 40 * time.Minute}
	for _, test := range []struct {
		name     string
		timeouts target.Timeouts
		ops      []Op
		// settles are the outcomes of the settle waits, and exitsInWait the
		// wait the target exits in, to restart after the longest backoff.
		settles     []bool
		exitsInWait int
		runsOut     []int
		// waits is what the Runner's waits take, which shows the case holds
		// what it names.
		waits time.Duration
	}{
		{name: "a create", timeouts: long, ops: []Op{createOp}},
		{name: "an update", timeouts: long, ops: []Op{createOp, updateOp}},
		{name: "a delete", timeouts: long, ops: []Op{createOp, {Type: OpDelete}}},
		{name: "a recreate", timeouts: long, ops: []Op{createOp, {Type: OpRecreate, Obj: widget("widget")}}},
		{name: "a recreate that does not settle", timeouts: long,
			ops: []Op{createOp, {Type: OpRecreate, Obj: widget("widget"), NoSettle: true}, settleOp}},
		{name: "a restart", timeouts: long, ops: []Op{createOp, {Type: OpRestart}, settleOp}},
		{name: "a deleteManaged", timeouts: long, ops: []Op{createOp, {Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(0)}}},
		{name: "a fault the teardown stops", timeouts: long,
			ops: []Op{createOp, faultOp, updateOp, updateOp, updateOp}, settles: []bool{true, false, false, false},
			waits: 3*time.Hour + 31*time.Minute + 11*time.Second},
		{name: "faults that stop one after another", timeouts: long,
			ops:     []Op{createOp, faultUntil(4), faultOp, updateOp, updateOp, updateOp},
			settles: []bool{true, false, false, false}, waits: 4*time.Hour + 11*time.Minute + 11*time.Second},
		{name: "faults whose triggers run out as a wait would end", timeouts: long,
			ops:     []Op{createOp, faultOp, faultOp, faultOp, faultOp, updateOp},
			settles: []bool{true, false}, runsOut: []int{2, 2, 2, 2}, waits: 11*time.Hour + 31*time.Minute + 11*time.Second},
		{name: "an exit while a fault is active", timeouts: target.DefaultTimeouts,
			ops: []Op{createOp, faultOp, updateOp}, settles: []bool{true, false}, exitsInWait: 2,
			waits: 14*time.Minute + 21*time.Second},
		{name: "an exit after a fault stopped", timeouts: target.DefaultTimeouts,
			ops: []Op{createOp, faultUntil(3), settleOp, updateOp}, settles: []bool{true, false, false}, exitsInWait: 3,
			waits: 8*time.Minute + 51*time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newWaitingHarness(test.timeouts)
			h.settles, h.exitsInWait, h.restartsIn, h.runsOut = test.settles, test.exitsInWait, launch.MaxBackoff, test.runsOut
			exercised, sequence := withTimeouts(test.timeouts), sequenceOf(test.ops...)

			if _, err := runSequence(t.Context(), exercised, sequence, Options{Check: &fakeChecker{}}, h); err != nil {
				t.Fatalf("The run failed: %v", err)
			}

			if test.exitsInWait > 0 && len(h.exited) != 1 {
				t.Fatalf("The target exited %d times, want once.", len(h.exited))
			}
			if len(h.runsOut) > 0 {
				t.Fatalf("The triggers of %d faults never ran out.", len(h.runsOut))
			}
			if waited := h.waited().Truncate(time.Second); test.waits > 0 && waited != test.waits {
				t.Fatalf("The Runner's waits took %v, want %v.", waited, test.waits)
			}
			if bound := Bound(exercised, sequence); bound < h.waited() {
				t.Errorf("Bound is %v, and the Runner's waits can take %v.", bound, h.waited())
			}
		})
	}
}

// deadlineHarness records until when the teardown's steps may run.
type deadlineHarness struct {
	*fakeHarness
	stepsUntil time.Time
}

func (d *deadlineHarness) deleteCR(ctx context.Context, name string) error {
	d.stepsUntil, _ = ctx.Deadline()
	return d.fakeHarness.deleteCR(ctx, name)
}

// The teardown's steps may run until T_stable, T_delete and 30s past where its
// quiet window opens, which is what Bound gives them.
func TestTheTeardownsStepsRunOnABudget(t *testing.T) {
	h := &deadlineHarness{fakeHarness: newFakeHarness()}

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
		{"a delete", Op{Type: OpDelete}, timeouts.Delete + timeouts.Settle},
		{"a delete that does not settle", Op{Type: OpDelete, NoSettle: true}, 0},
		{"a recreate", Op{Type: OpRecreate, Obj: widget("widget")}, timeouts.Delete + timeouts.Settle},
		{"a recreate that does not settle", Op{Type: OpRecreate, Obj: widget("widget"), NoSettle: true}, timeouts.Delete},
		{"a restart", Op{Type: OpRestart}, launch.DefaultGracePeriod},
		{"a deleteManaged", Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(0)}, timeouts.Settle},
		{"a settle", settleOp, timeouts.Settle},
	} {
		t.Run(test.name, func(t *testing.T) {
			if adds := Bound(defaults, sequenceOf(createOp, test.op)) - alone; adds != test.adds {
				t.Errorf("Bound adds %v for %s, want %v.", adds, test.name, test.adds)
			}
		})
	}
}

// At the default timeouts, a create alone can take 30s to start, 30s to settle,
// and 10s of quiet, 60s for the CR to go and 30s more in the teardown. Stopping
// the target takes up to 10s and deleting the namespace 30s.
func TestBoundOfACreateAlone(t *testing.T) {
	if bound, want := Bound(withTimeouts(target.DefaultTimeouts), sequenceOf(createOp)), 200*time.Second; bound != want {
		t.Errorf("Bound is %v, want %v.", bound, want)
	}
}

// A target that exits while a fault excuses it restarts after a backoff of up
// to 5m and is owed T_settle past the restart. Once the faults stop, they owe
// it as long as they lasted and T_settle. So each fault allows an exit,
// doubles the time before the teardown, and allows another exit.
func TestBoundDoublesTheRunForEachFault(t *testing.T) {
	defaults := withTimeouts(target.DefaultTimeouts)
	const settle, teardown = 30 * time.Second, 140 * time.Second
	exit := launch.MaxBackoff + settle
	// The start, the create and the update take 90s.
	oneFault := 2*(90*time.Second+exit) + settle + exit
	for _, test := range []struct {
		name string
		ops  []Op
		want time.Duration
	}{
		{"one fault", []Op{createOp, faultOp, updateOp}, oneFault + teardown},
		{"two faults", []Op{createOp, faultOp, faultOp, updateOp}, 2*(oneFault+exit) + settle + exit + teardown},
	} {
		t.Run(test.name, func(t *testing.T) {
			if bound := Bound(defaults, sequenceOf(test.ops...)); bound != test.want {
				t.Errorf("Bound is %v, want %v.", bound, test.want)
			}
		})
	}
}

func TestBoundOfSeveralRunsIsTheirSum(t *testing.T) {
	defaults := withTimeouts(target.DefaultTimeouts)
	one, other := sequenceOf(createOp), sequenceOf(createOp, updateOp)

	if bound, want := Bound(defaults, one, other), Bound(defaults, one)+Bound(defaults, other); bound != want {
		t.Errorf("Bound of both runs is %v, want %v.", bound, want)
	}
}

func TestBoundSaturatesRatherThanOverflow(t *testing.T) {
	defaults := withTimeouts(target.DefaultTimeouts)
	ops := []Op{createOp}
	for range 64 {
		ops = append(ops, faultOp)
	}
	manyFaults := sequenceOf(append(ops, updateOp)...)
	// Two of the longest durations add up to one that looks short.
	const longest = math.MaxInt64
	settles := withTimeouts(target.Timeouts{Settle: longest, Stable: time.Second, Delete: time.Second})
	deletes := withTimeouts(target.Timeouts{Settle: 2 * time.Second, Stable: time.Second, Delete: longest / 3})
	teardowns := withTimeouts(target.Timeouts{Settle: 2 * time.Second, Stable: longest, Delete: longest})
	// A run takes over half the longest duration.
	long := withTimeouts(target.Timeouts{Settle: longest / 4, Stable: time.Second, Delete: longest / 4})
	recreate := Op{Type: OpRecreate, Obj: widget("widget"), NoSettle: true}
	for _, test := range []struct {
		name  string
		bound time.Duration
	}{
		{"many faults", Bound(defaults, manyFaults)},
		{"long settle waits", Bound(settles, sequenceOf(createOp, updateOp))},
		{"long deletions", Bound(deletes, sequenceOf(slices.Concat([]Op{createOp}, slices.Repeat([]Op{recreate}, 7))...))},
		{"a long teardown", Bound(teardowns, sequenceOf(createOp))},
		{"two long runs", Bound(long, sequenceOf(createOp), sequenceOf(createOp))},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.bound != math.MaxInt64 {
				t.Errorf("Bound is %v, want the longest duration.", test.bound)
			}
		})
	}
}
