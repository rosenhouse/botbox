package invariant

import (
	"slices"
	"time"
)

// Owed is when the target must have converged by, after the faults that
// stopped by t. An exit the faults excused owes what settledBy gives its
// restart, since botbox chose the restart's backoff. While a fault is active,
// only the first such exit since the last op stamped before that time owes
// it, so that a crash loop under a fault is not owed time for good. Owed is
// zero where the target owes nothing.
func (in Input) Owed(t time.Time) time.Time {
	owed := in.faultsOwed(t)
	recovered := in.lastConverged(t)
	active := in.faulted(t, t)
	for _, exit := range in.Exits {
		if exit.At.After(t) || !exit.At.After(recovered) || !in.faultsExcuse(exit.At) {
			continue
		}
		settled := in.settledBy(exit.Restart)
		if !active || in.firstExcusedExitSince(in.opBefore(settled), exit) {
			owed = later(owed, settled)
		}
	}
	return owed
}

// firstExcusedExitSince reports whether no other exit the faults excused came
// between the op and this exit.
func (in Input) firstExcusedExitSince(op Op, exit Exit) bool {
	return !slices.ContainsFunc(in.Exits, func(earlier Exit) bool {
		return !earlier.At.Before(op.Time) && earlier.At.Before(exit.At) && in.faultsExcuse(earlier.At)
	})
}

// faultsOwed is Owed of the faults alone: as long after the last of them as
// they lasted, and timeouts.settle more. A target backs off while its
// requests fail, and one that doubles its delay retries within as long as it
// has been failing. That span starts no earlier than the target's last convergence,
// which is when it last recovered.
func (in Input) faultsOwed(t time.Time) time.Time {
	recovered := in.lastConverged(t)
	var first, last time.Time
	for _, fault := range in.Faults {
		// An active fault's zero End is never after recovered.
		if fault.End.After(t) || !fault.End.After(recovered) {
			continue
		}
		if first.IsZero() || fault.Start.Before(first) {
			first = fault.Start
		}
		if fault.End.After(last) {
			last = fault.End
		}
	}
	if last.IsZero() {
		return time.Time{}
	}
	return last.Add(last.Sub(later(first, recovered)) + in.timeouts().Settle)
}

// Recovering reports whether the faults excuse a target that has not
// converged by t: one is still active, or the target is still owed time to
// recover from one.
func (in Input) Recovering(t time.Time) bool {
	return in.faulted(t, t) || t.Before(in.Owed(t))
}

// faultsExcuse is Recovering of the faults alone, so that an exit never
// excuses another.
func (in Input) faultsExcuse(t time.Time) bool {
	return in.faulted(t, t) || t.Before(in.faultsOwed(t))
}

// PendingRestart is when the target starts again after an exit the faults
// excused, where it is waiting to at t. It is zero otherwise.
func (in Input) PendingRestart(t time.Time) time.Time {
	for _, exit := range in.Exits {
		if !exit.At.After(t) && exit.Restart.After(t) && in.faultsExcuse(exit.At) {
			return exit.Restart
		}
	}
	return time.Time{}
}

// excusedExit reports whether the target exited in [from, to] where the
// faults excused it.
func (in Input) excusedExit(from, to time.Time) bool {
	return slices.ContainsFunc(in.Exits, func(exit Exit) bool {
		return !exit.At.Before(from) && !exit.At.After(to) && in.faultsExcuse(exit.At)
	})
}

// lastConverged is when the last settle wait that converged by t ended, or
// zero if none did.
func (in Input) lastConverged(t time.Time) time.Time {
	var converged time.Time
	for _, checkpoint := range in.Checkpoints {
		if checkpoint.Settle == Converged && !checkpoint.Time.After(t) && checkpoint.Time.After(converged) {
			converged = checkpoint.Time
		}
	}
	return converged
}

// nextConverged is when the first settle wait that converged after t ended,
// or zero if none did.
func (in Input) nextConverged(t time.Time) time.Time {
	var converged time.Time
	for _, checkpoint := range in.Checkpoints {
		if checkpoint.Settle == Converged && checkpoint.Time.After(t) && (converged.IsZero() || checkpoint.Time.Before(converged)) {
			converged = checkpoint.Time
		}
	}
	return converged
}
