package run

import (
	"context"
	"errors"
	"time"

	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/target"
)

// settlePoll is how often a settle wait reads the Observer.
const settlePoll = 50 * time.Millisecond

// Settle waits for the target's reaction (DESIGN.md §5.5): the Ready predicate
// holds and neither the CR nor a managed object has changed for T_stable. It
// reports whether it converged within T_settle, or by what owed returns if
// that is later: the target may still be recovering from a fault or deleting a
// CR. A nil owed owes nothing. The caller judges a wait that expires.
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
	// last changed, which is never before since. Its error ends the wait.
	state func(since time.Time) (ready bool, changed time.Time, err error)
	// stopped is closed once the target's process has stopped.
	stopped <-chan struct{}
	// owed is when the wait may give up, which can move while the wait runs.
	// Nil owes nothing.
	owed func() time.Time
}

func (s settle) wait(ctx context.Context) (bool, error) {
	start := s.now()
	for {
		ready, changed, err := s.state(start)
		if err != nil {
			return false, err
		}
		now := s.now()
		if ready && !now.Before(changed.Add(s.timeouts.Stable)) {
			return true, nil
		}
		deadline := start.Add(s.timeouts.Settle)
		if s.owed != nil {
			if owed := s.owed(); owed.After(deadline) {
				deadline = owed
			}
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
func (h *Harness) state(since time.Time) (bool, time.Time, error) {
	ready, err := ready(h.target.Ready, h.Observer.Current(h.target.Primary))
	changed := since
	if window := h.Observer.Window(since, time.Now()); len(window) > 0 {
		changed = window[len(window)-1].Time
	}
	return ready, changed, err
}

// ready reports whether the predicate holds on every primary CR observed. An
// evaluation error means "not ready", and a result that is not a bool is a
// configuration error. A CR under deletion is not ready until it is gone, and
// a run whose CR is gone has nothing left to be ready.
func ready(predicate target.ReadyFunc, observed []observe.Version) (bool, error) {
	for _, cr := range observed {
		ready, err := predicate(cr.Object)
		if errors.Is(err, target.ErrNotBool) {
			return false, err
		}
		if err != nil || !ready || cr.DeletionTimestamp != nil {
			return false, nil
		}
	}
	return true, nil
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
