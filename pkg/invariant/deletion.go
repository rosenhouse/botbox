package invariant

import "time"

// DeletionOwed is when a settle wait may end without convergence, given each
// deletion of the primary CR recorded by t: T_stable past when the CR went,
// or past its G3 deadline if that came first. Zero means no CR was deleted.
func (in Input) DeletionOwed(t time.Time) time.Time {
	var owed time.Time
	for _, deleted := range in.crDeletionsBy(t) {
		ended := deleted.at.Add(in.timeouts().Delete)
		if gone, found := in.goneBy(deleted, t); found && gone.Before(ended) {
			ended = gone
		}
		owed = later(owed, ended.Add(in.timeouts().Stable))
	}
	return owed
}

// DeletionOverdue reports whether a settle wait saw a CR outlive its deletion
// deadline: the CR was still there at the deadline, or where the wait began if
// that was later. G3 judges such a wait, not G4.
func (in Input) DeletionOverdue(checkpoint Checkpoint) bool {
	for _, deleted := range in.crDeletionsBy(checkpoint.Time) {
		deadline := deleted.at.Add(in.timeouts().Delete)
		if deadline.After(checkpoint.Time) {
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
