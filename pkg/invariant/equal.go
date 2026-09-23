package invariant

import (
	"fmt"
	"reflect"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/target"
)

// ignoredByDefault are the paths the default equality ignores. It compares
// everything else, labels, annotations, ownerReferences and finalizers among
// them.
var ignoredByDefault = []target.Path{
	target.MustParsePath("metadata.resourceVersion"),
	target.MustParsePath("metadata.uid"),
	target.MustParsePath("metadata.creationTimestamp"),
	target.MustParsePath("metadata.generation"),
	target.MustParsePath("metadata.managedFields"),
	target.MustParsePath("status.conditions[*].lastTransitionTime"),
}

// equality compares the snapshots around one Restart. A target that declares
// a hook replaces the whole predicate (DESIGN.md §8.4); otherwise this is the
// §6 default, which needs both snapshots to tell a live owner from a dangling
// reference.
func (in Input) equality(before, after state, out *Result) func(a, b observe.Version) bool {
	if in.Target.Equal != nil {
		return func(a, b observe.Version) bool {
			return in.Target.Equal(snapshot(a), snapshot(b))
		}
	}
	liveBefore, liveAfter := in.owners(before), in.owners(after)
	return func(a, b observe.Version) bool {
		return reflect.DeepEqual(
			in.comparable(a.Object, liveBefore, out),
			in.comparable(b.Object, liveAfter, out),
		)
	}
}

func snapshot(v observe.Version) observe.Snapshot {
	return observe.Snapshot{GVK: v.GVK, Name: v.Name, Object: v.Object}
}

// comparable reduces an object to what §6 compares.
func (in Input) comparable(obj *unstructured.Unstructured, live ownerSet, out *Result) map[string]any {
	content := obj.DeepCopy().Object
	for _, path := range ignoredByDefault {
		path.Remove(content)
	}
	metadata, _ := content["metadata"].(map[string]any)
	live.pruneDangling(metadata)
	for _, path := range in.Target.EqualIgnore {
		if err := path.Remove(content); err != nil {
			out.noteOnce(fmt.Sprintf("%s could not follow equalIgnore %s: %v", out.ID, path, err))
		}
	}
	return content
}

func (out *Result) noteOnce(note string) {
	if !slices.Contains(out.Notes, note) {
		out.Notes = append(out.Notes, note)
	}
}

// ownerSet holds the owners a snapshot can resolve: by group, kind and name,
// whatever version a reference names, then by UID.
type ownerSet struct {
	uids map[ownerKey]types.UID
	// watched are the kinds botbox observes. An owner of any other kind is
	// unresolvable, so it counts as live.
	watched map[schema.GroupKind]bool
}

type ownerKey struct {
	kind schema.GroupKind
	name string
}

func (in Input) owners(s state) ownerSet {
	set := ownerSet{uids: map[ownerKey]types.UID{}, watched: map[schema.GroupKind]bool{}}
	for _, gvk := range append([]schema.GroupVersionKind{in.Target.Primary}, in.Target.Manages...) {
		set.watched[gvk.GroupKind()] = true
	}
	for _, v := range s.live {
		set.uids[ownerKey{kind: v.GVK.GroupKind(), name: v.Name}] = v.UID
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
	gvk := schema.FromAPIVersionAndKind(text(ref["apiVersion"]), text(ref["kind"]))
	key := ownerKey{kind: gvk.GroupKind(), name: text(ref["name"])}
	if !o.watched[key.kind] {
		return true // botbox does not watch the kind, so it cannot say the owner is gone.
	}
	uid, found := o.uids[key]
	return found && uid == types.UID(text(ref["uid"]))
}

func text(value any) string {
	s, _ := value.(string)
	return s
}
