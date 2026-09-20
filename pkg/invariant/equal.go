package invariant

import (
	"reflect"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rosenhouse/botbox/pkg/observe"
)

// ignoredMetadata are the metadata fields the §6 default equality ignores.
// It compares everything else, labels, annotations, ownerReferences and
// finalizers among them.
var ignoredMetadata = []string{"resourceVersion", "uid", "creationTimestamp", "generation", "managedFields"}

// equality compares the snapshots around one Restart. A target that declares
// a hook replaces the whole predicate (DESIGN.md §8.4); otherwise this is the
// §6 default, which needs both snapshots to tell a live owner from a dangling
// reference.
func (in Input) equality(before, after state) func(a, b observe.Version) bool {
	if in.Target.Equal != nil {
		return func(a, b observe.Version) bool {
			return in.Target.Equal(snapshot(a), snapshot(b))
		}
	}
	liveBefore, liveAfter := in.owners(before), in.owners(after)
	return func(a, b observe.Version) bool {
		return reflect.DeepEqual(
			in.comparable(a.Object, liveBefore),
			in.comparable(b.Object, liveAfter),
		)
	}
}

func snapshot(v observe.Version) observe.Snapshot {
	return observe.Snapshot{GVK: v.GVK, Name: v.Name, Object: v.Object}
}

// comparable reduces an object to what §6 compares.
func (in Input) comparable(obj *unstructured.Unstructured, live ownerSet) map[string]any {
	content := obj.DeepCopy().Object
	metadata, _, _ := unstructured.NestedMap(content, "metadata")
	for _, field := range ignoredMetadata {
		delete(metadata, field)
	}
	live.pruneDangling(metadata)
	delete(content, "metadata")
	if len(metadata) > 0 {
		content["metadata"] = metadata
	}
	dropTransitionTimes(content)
	for _, path := range in.Target.EqualIgnore {
		unstructured.RemoveNestedField(content, strings.Split(path, ".")...)
	}
	return content
}

// dropTransitionTimes removes status.conditions[*].lastTransitionTime, which
// moves whenever a controller re-decides the same condition.
func dropTransitionTimes(content map[string]any) {
	conditions, found, err := unstructured.NestedFieldNoCopy(content, "status", "conditions")
	if !found || err != nil {
		return
	}
	entries, ok := conditions.([]any)
	if !ok {
		return
	}
	for _, entry := range entries {
		if condition, ok := entry.(map[string]any); ok {
			delete(condition, "lastTransitionTime")
		}
	}
}

// ownerSet holds the owners a snapshot can resolve, the way the collector
// resolves them: by apiVersion, kind and name, then by UID (DESIGN.md §5.8).
type ownerSet struct {
	uids map[ownerKey]types.UID
	// watched are the kinds botbox observes. An owner of any other kind is
	// unresolvable, so it counts as live.
	watched map[ownerKey]bool
}

type ownerKey struct{ apiVersion, kind, name string }

func (in Input) owners(s state) ownerSet {
	set := ownerSet{uids: map[ownerKey]types.UID{}, watched: map[ownerKey]bool{}}
	for _, gvk := range append([]schema.GroupVersionKind{in.Target.Primary}, in.Target.Manages...) {
		set.watched[ownerKey{apiVersion: gvk.GroupVersion().String(), kind: gvk.Kind}] = true
	}
	for _, v := range s.live {
		key := ownerKey{apiVersion: v.GVK.GroupVersion().String(), kind: v.GVK.Kind, name: v.Name}
		set.uids[key] = v.UID
	}
	return set
}

// pruneDangling drops the ownerReferences whose owner no longer exists, which
// §6 ignores, and the key itself once none remain.
func (o ownerSet) pruneDangling(metadata map[string]any) {
	refs, found := metadata["ownerReferences"].([]any)
	if !found {
		return
	}
	kept := slices.DeleteFunc(refs, func(entry any) bool {
		ref, ok := entry.(map[string]any)
		return ok && !o.exists(ref)
	})
	if len(kept) == 0 {
		delete(metadata, "ownerReferences")
		return
	}
	metadata["ownerReferences"] = kept
}

func (o ownerSet) exists(ref map[string]any) bool {
	key := ownerKey{apiVersion: text(ref["apiVersion"]), kind: text(ref["kind"]), name: text(ref["name"])}
	if !o.watched[ownerKey{apiVersion: key.apiVersion, kind: key.kind}] {
		return true // botbox does not watch the kind, so it cannot say the owner is gone.
	}
	uid, found := o.uids[key]
	return found && uid == types.UID(text(ref["uid"]))
}

func text(value any) string {
	s, _ := value.(string)
	return s
}
