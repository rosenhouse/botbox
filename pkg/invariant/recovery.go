package invariant

import "time"

// Recovery is the Op of the checkpoint after the settle wait the teardown
// gives the target once the last fault stops.
const Recovery = -2

// Owed is when the target must have converged by, after the faults that
// stopped by t: as long after the last of them as they lasted, and T_settle
// more. A target backs off while its requests fail, and one that doubles its
// delay retries within as long as it has been failing. The faults are those
// since the target last converged, which is when it last recovered. Owed is
// zero where none of them has stopped.
func (in Input) Owed(t time.Time) time.Time {
	recovered := in.lastConverged(t)
	var first, last time.Time
	for _, fault := range in.Faults {
		if !fault.End.IsZero() && !fault.End.After(recovered) {
			continue
		}
		if first.IsZero() || fault.Start.Before(first) {
			first = fault.Start
		}
		if !fault.End.IsZero() && !fault.End.After(t) && fault.End.After(last) {
			last = fault.End
		}
	}
	if last.IsZero() {
		return time.Time{}
	}
	return last.Add(last.Sub(first) + in.timeouts().Settle)
}

// Recovering reports whether the faults excuse a target that has not
// converged by t: one is still active, or the target is still owed time to
// recover from one.
func (in Input) Recovering(t time.Time) bool {
	return in.faulted(t, t) || t.Before(in.Owed(t))
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
