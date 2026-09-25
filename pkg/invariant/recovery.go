package invariant

import (
	"slices"
	"time"
)

// Owed is when the target must have converged by, after the faults that
// stopped by t. An exit the faults excused owes what settledBy gives its
// restart, since botbox chose the restart's backoff. Owed is zero where the
// target owes nothing.
func (in Input) Owed(t time.Time) time.Time {
	owed := in.faultsOwed(t)
	recovered := in.lastConverged(t)
	for _, exit := range in.Exits {
		if !exit.At.After(t) && exit.At.After(recovered) && in.faultsExcuse(exit.At) {
			owed = later(owed, in.settledBy(exit.Restart))
		}
	}
	return owed
}

// faultsOwed is Owed of the faults alone: as long after the last of them as
// they lasted, and T_settle more. A target backs off while its requests fail,
// and one that doubles its delay retries within as long as it has been
// failing. That span starts no earlier than the target's last convergence,
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
