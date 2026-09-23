package invariant

import "time"

// WaitOwed is when a settle wait that has not converged may end: once the
// target has had its time to recover from the faults, each deleted CR has gone
// or reached its G3 deadline, and the run has had T_settle to settle after a
// CR went.
func (in Input) WaitOwed(t time.Time) time.Time {
	return later(in.Owed(t), in.deletionOwed(t))
}

func (in Input) deletionOwed(t time.Time) time.Time {
	var owed time.Time
	for _, deleted := range in.crDeletionsBy(t) {
		deadline := deleted.at.Add(in.timeouts().Delete)
		if gone, found := in.goneBy(deleted, t); found && !gone.After(deadline) {
			deadline = gone.Add(in.timeouts().Settle)
		}
		owed = later(owed, deadline)
	}
	return owed
}

// Excused reports whether a settle wait that expired is not G4's to report: a
// fault excuses it, or G3 judges it.
func (in Input) Excused(checkpoint Checkpoint) bool {
	return in.Recovering(checkpoint.Time) || in.DeletionOverdue(checkpoint)
}

// DeletionOverdue reports whether a settle wait saw a CR outlive a deletion
// deadline G3 judges: the CR was still there at the deadline, or where the
// wait began if that was later.
func (in Input) DeletionOverdue(checkpoint Checkpoint) bool {
	for _, deleted := range in.crDeletionsBy(checkpoint.Time) {
		deadline := deleted.at.Add(in.timeouts().Delete)
		if deadline.After(checkpoint.Time) || in.faulted(deleted.at, deadline) {
			continue
		}
		if cr, found := in.stateAt(later(deadline, checkpoint.Began)).version(deleted.key); found && cr.UID == deleted.uid {
			return true
		}
	}
	return false
}

// goneBy is when the Observer saw the deleted CR go, if it did by t.
func (in Input) goneBy(deleted deletion, t time.Time) (time.Time, bool) {
	for _, v := range in.History.History(deleted.key) {
		if v.UID == deleted.uid && v.Deleted && !v.Time.After(t) {
			return v.Time, true
		}
	}
	return time.Time{}, false
}
