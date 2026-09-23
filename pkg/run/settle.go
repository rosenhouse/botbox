package run

import (
	"context"
	"time"

	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/target"
)

// settlePoll is how often a settle wait reads the Observer.
const settlePoll = 50 * time.Millisecond

// Settle waits for the target's reaction (DESIGN.md §5.5): the Ready predicate
// holds and neither the CR nor a managed object has changed for T_stable. It
// reports whether it converged within T_settle, or by what owed returns if
// that is later: the target may still be recovering from a fault. A wait that
// expires while no fault excuses it is a G4 violation, which the caller
// records.
func (h *Harness) Settle(ctx context.Context, owed func() time.Time) (bool, error) {
	return settle{
		timeouts: h.target.Timeouts,
		poll:     settlePoll,
		now:      time.Now,
		sleep:    sleep,
		state:    h.state,
		stopped:  h.Launcher.Exited(),
		owed:     owed,
	}.wait(ctx)
}

// settle is one settle wait. Its clock and its reading of the run are injected,
// so the decision is testable without a cluster.
type settle struct {
	timeouts target.Timeouts
	poll     time.Duration
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	// state reports whether the target is ready, and when the run namespace
	// last changed, which is never before since.
	state func(since time.Time) (ready bool, changed time.Time)
	// stopped is closed once the target's process has stopped.
	stopped <-chan struct{}
	// owed is when the target must have recovered from the faults by, which
	// can move while the wait runs.
	owed func() time.Time
}

func (s settle) wait(ctx context.Context) (bool, error) {
	start := s.now()
	for {
		ready, changed := s.state(start)
		now := s.now()
		if ready && !now.Before(changed.Add(s.timeouts.Stable)) {
			return true, nil
		}
		deadline := start.Add(s.timeouts.Settle)
		if owed := s.owed(); owed.After(deadline) {
			deadline = owed
		}
		// A target that stopped will never converge, so the wait ends there.
		if !now.Before(deadline) || closed(s.stopped) {
			return false, nil
		}
		if err := s.sleep(ctx, s.poll); err != nil {
			return false, err
		}
	}
}

func closed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// state reads the Observer. Readiness is read first, so that a change arriving
// during the read counts against stability rather than for it. The op that
// opened the wait changed the CR, which is why stability runs from since.
func (h *Harness) state(since time.Time) (bool, time.Time) {
	ready := ready(h.target.Ready, h.Observer.Current(h.target.Primary))
	changed := since
	if window := h.Observer.Window(since, time.Now()); len(window) > 0 {
		changed = window[len(window)-1].Time
	}
	return ready, changed
}

// ready reports whether the predicate holds on every primary CR observed. An
// evaluation error means "not ready" (DESIGN.md §8.4). A run whose CR is gone
// has nothing left to be ready.
func ready(predicate target.ReadyFunc, observed []observe.Version) bool {
	for _, cr := range observed {
		if ready, err := predicate(cr.Object); err != nil || !ready {
			return false
		}
	}
	return true
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
