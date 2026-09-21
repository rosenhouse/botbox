package invariant

import (
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rosenhouse/botbox/pkg/observe"
)

// CleanDeletion is G3: after the CR is deleted with no fault active, every
// object the target manages for it is deleted and the CR's finalizers are
// cleared within T_delete (DESIGN.md §6). Owned children go with the
// collector, so what remains is an orphan or an uncleared finalizer (§5.8).
func CleanDeletion(in Input) (Result, error) {
	out := Result{ID: "G3"}
	for _, deleted := range in.crDeletions() {
		deadline := deleted.at.Add(in.timeouts().Delete)
		switch {
		case in.cleanedBy(deadline): // The namespace emptied, so nothing was left.
		case in.faulted(deleted.at, deadline):
			out.note("for the deletion of %s: a fault was active before its deadline", deleted.key.Name)
		case !in.observed(deadline):
			out.note("for the deletion of %s: the run ended before its %s deadline", deleted.key.Name, in.timeouts().Delete)
		default:
			out.reportLeftovers(in, deleted, deadline)
		}
	}
	return out, nil
}

// cleanedBy reports whether botbox saw the run namespace empty by t, which
// leaves nothing for G3 to find at t.
func (in Input) cleanedBy(t time.Time) bool {
	return !in.Cleaned.IsZero() && !in.Cleaned.After(t)
}

// deletion is one deletion of the primary CR, timestamped as the Observer saw
// it: metadata.deletionTimestamp holds whole seconds only.
type deletion struct {
	key observe.Key
	uid types.UID
	at  time.Time
}

func (in Input) crDeletions() []deletion {
	var deletions []deletion
	seen := map[types.UID]bool{}
	for _, v := range in.versions() {
		if v.GVK != in.Target.Primary || seen[v.UID] || (v.DeletionTimestamp == nil && !v.Deleted) {
			continue
		}
		seen[v.UID] = true
		deletions = append(deletions, deletion{key: v.Key, uid: v.UID, at: v.Time})
	}
	return deletions
}

// reportLeftovers judges one deleted CR and the objects it had when it went.
// A CR or a child the run recreated carries a new UID, so it belongs to the
// CR that came after and is not this deletion's to answer for.
func (out *Result) reportLeftovers(in Input, deleted deletion, deadline time.Time) {
	states := in.statesAt([]time.Time{deleted.at, deadline})
	when, since := states[0], states[1]
	if cr, found := since.version(deleted.key); found && cr.UID == deleted.uid && len(cr.Finalizers) > 0 {
		out.violate(Violation{
			Statement: fmt.Sprintf("the CR %s still carried the finalizers %v %s after its deletion",
				cr.Name, cr.Finalizers, in.timeouts().Delete),
			At:       deadline,
			Versions: recent(in.History.History(cr.Key)),
		})
	}
	had := map[observe.Key]types.UID{}
	for _, object := range when.managed(in) {
		had[object.Key] = object.UID
	}
	for _, left := range since.managed(in) {
		if uid, was := had[left.Key]; !was || uid != left.UID {
			continue
		}
		out.violate(Violation{
			Statement: fmt.Sprintf("the %s %s was still there %s after the CR was deleted%s",
				kindName(left.GVK), left.Name, in.timeouts().Delete, orphaned(left, deleted.uid)),
			At:       deadline,
			Versions: recent(in.History.History(left.Key)),
		})
	}
}

// orphaned names what the collector could not reach: an object with no
// ownerReference to the CR is the target's own to delete (DESIGN.md §6, §5.8).
func orphaned(left observe.Version, cr types.UID) string {
	if ownedBy(left.OwnerReferences, cr) {
		return ""
	}
	return ", orphaned: it carries no ownerReference to the CR"
}

func ownedBy(refs []metav1.OwnerReference, owner types.UID) bool {
	return slices.ContainsFunc(refs, func(ref metav1.OwnerReference) bool { return ref.UID == owner })
}
