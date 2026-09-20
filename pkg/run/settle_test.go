package run

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/target"
)

var testTimeouts = target.Timeouts{Settle: 5 * time.Second, Stable: 2 * time.Second, Delete: 10 * time.Second}

// clock drives a settle wait without sleeping: each poll advances it.
type clock struct{ now time.Time }

func newClock() *clock { return &clock{now: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)} }

func (c *clock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now = c.now.Add(d)
	return nil
}

// waitOn runs a settle wait over state and returns whether it converged and
// how long it took on the clock.
func waitOn(t *testing.T, ctx context.Context, c *clock, state func(since time.Time) (bool, time.Time)) (bool, time.Duration, error) {
	t.Helper()
	start := c.now
	wait := settle{
		timeouts: testTimeouts,
		poll:     100 * time.Millisecond,
		now:      func() time.Time { return c.now },
		sleep:    c.sleep,
		state:    state,
	}
	converged, err := wait.wait(ctx)
	return converged, c.now.Sub(start), err
}

func TestSettleConvergesOnceTheRunHoldsStill(t *testing.T) {
	c := newClock()
	quiet := func(since time.Time) (bool, time.Time) { return true, since }

	converged, elapsed, err := waitOn(t, t.Context(), c, quiet)

	if err != nil || !converged {
		t.Fatalf("The wait returned (%t, %v), want convergence.", converged, err)
	}
	if elapsed != testTimeouts.Stable {
		t.Errorf("The wait took %v, want T_stable of %v.", elapsed, testTimeouts.Stable)
	}
}

func TestSettleWaitsForTStableAfterTheLastChange(t *testing.T) {
	c := newClock()
	const lastChange = time.Second
	changedOnce := func(since time.Time) (bool, time.Time) { return true, since.Add(lastChange) }

	converged, elapsed, err := waitOn(t, t.Context(), c, changedOnce)

	if err != nil || !converged {
		t.Fatalf("The wait returned (%t, %v), want convergence.", converged, err)
	}
	if want := lastChange + testTimeouts.Stable; elapsed != want {
		t.Errorf("The wait took %v, want %v: T_stable is counted from the last change.", elapsed, want)
	}
}

func TestSettleGivesUpWhileTheRunKeepsChanging(t *testing.T) {
	c := newClock()
	churning := func(time.Time) (bool, time.Time) { return true, c.now }

	converged, elapsed, err := waitOn(t, t.Context(), c, churning)

	if err != nil || converged {
		t.Fatalf("The wait returned (%t, %v), want no convergence: the run never holds still.", converged, err)
	}
	if elapsed != testTimeouts.Settle {
		t.Errorf("The wait took %v, want T_settle of %v.", elapsed, testTimeouts.Settle)
	}
}

func TestSettleGivesUpWhileTheTargetIsNotReady(t *testing.T) {
	c := newClock()
	notReady := func(since time.Time) (bool, time.Time) { return false, since }

	converged, elapsed, err := waitOn(t, t.Context(), c, notReady)

	if err != nil || converged {
		t.Fatalf("The wait returned (%t, %v), want no convergence: the target is not ready.", converged, err)
	}
	if elapsed != testTimeouts.Settle {
		t.Errorf("The wait took %v, want T_settle of %v.", elapsed, testTimeouts.Settle)
	}
}

func TestSettleEndsWithTheContext(t *testing.T) {
	c := newClock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	converged, _, err := waitOn(t, ctx, c, func(since time.Time) (bool, time.Time) { return false, since })

	if !errors.Is(err, context.Canceled) || converged {
		t.Errorf("The wait returned (%t, %v), want the context's error.", converged, err)
	}
}
