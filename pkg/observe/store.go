// Package observe records the version history of the primary CR and of every
// managed kind, so the invariant engine can decide after the fact
// (DESIGN.md §5.3).
package observe

import (
	"cmp"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Key identifies an object across its versions. A recreated object keeps its
// key and takes a new UID.
type Key struct {
	GVK       schema.GroupVersionKind
	Namespace string
	Name      string
}

// Version is one state an object passed through.
type Version struct {
	Key
	UID                types.UID
	ResourceVersion    string
	Time               time.Time // when the event arrived
	Generation         int64
	ObservedGeneration *int64
	Finalizers         []string
	OwnerReferences    []metav1.OwnerReference
	DeletionTimestamp  *metav1.Time
	Labels             map[string]string
	Deleted            bool // the object's last version: it is gone
	Object             *unstructured.Unstructured
}

// Snapshot is one object of a converged state, which G5 compares
// (DESIGN.md §8.2).
type Snapshot struct {
	GVK    schema.GroupVersionKind
	Name   string
	Object *unstructured.Unstructured
}

// Store holds the version history. Informer callbacks write it while the
// invariants read it.
type Store struct {
	opts Options

	// mu guards the fields below. latest, latestAt and isManaged expect it held.
	mu       sync.RWMutex
	versions []Version
	byKey    map[Key][]int // positions in versions, in the order recorded
	botbox   map[Key]bool
}

// NewStore returns an empty store. opts carry the attribution rule of §6: the
// managed kinds and the optional selector.
func NewStore(opts Options) *Store {
	return &Store{opts: opts, byKey: map[Key][]int{}, botbox: map[Key]bool{}}
}

// Record adds the version of obj that an event delivered at time at. A version
// the store already holds is ignored, so a relist does not duplicate history.
func (s *Store) Record(gvk schema.GroupVersionKind, obj *unstructured.Unstructured, at time.Time) {
	s.record(newVersion(gvk, obj, at, false))
}

// RecordDeletion adds the last version of obj.
func (s *Store) RecordDeletion(gvk schema.GroupVersionKind, obj *unstructured.Unstructured, at time.Time) {
	s.record(newVersion(gvk, obj, at, true))
}

// MarkBotboxCreated records that botbox, not the target, created the named
// object: the primary CR and every fixture. Such an object is never managed (§6).
func (s *Store) MarkBotboxCreated(gvk schema.GroupVersionKind, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.botbox[Key{GVK: gvk, Namespace: s.opts.Namespace, Name: name}] = true
}

// History returns every version of one object, in the order recorded.
func (s *Store) History(key Key) []Version {
	s.mu.RLock()
	defer s.mu.RUnlock()
	positions := s.byKey[key]
	history := make([]Version, len(positions))
	for i, position := range positions {
		history[i] = s.versions[position]
	}
	return history
}

// Current returns the latest version of every live object of one kind.
func (s *Store) Current(gvk schema.GroupVersionKind) []Version {
	return s.live(func(v Version) bool { return v.GVK == gvk })
}

// Managed returns the latest version of every live managed object: an object
// of a managed kind that botbox did not create (DESIGN.md §6).
func (s *Store) Managed() []Version {
	return s.live(func(v Version) bool { return s.isManaged(v) })
}

// ManagedBy returns the managed objects that name owner in their
// ownerReferences, which attributes them to one CR (§6).
func (s *Store) ManagedBy(owner types.UID) []Version {
	return slices.DeleteFunc(s.Managed(), func(v Version) bool {
		return !slices.ContainsFunc(v.OwnerReferences, func(ref metav1.OwnerReference) bool {
			return ref.UID == owner
		})
	})
}

// IsManaged reports whether the object is the target's, whether or not it is
// still live, because G3 asks about objects that are gone.
func (s *Store) IsManaged(key Key) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.latest(key)
	return ok && s.isManaged(v)
}

// Window returns every version recorded in [from, to], in the order recorded.
func (s *Store) Window(from, to time.Time) []Version {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var window []Version
	for _, v := range s.versions {
		if !v.Time.Before(from) && !v.Time.After(to) {
			window = append(window, v)
		}
	}
	return window
}

// SnapshotAt returns the latest version of every object live at t, ordered by
// kind and name.
func (s *Store) SnapshotAt(t time.Time) []Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var snapshot []Snapshot
	for key := range s.byKey {
		v, ok := s.latestAt(key, t)
		if !ok || v.Deleted {
			continue
		}
		snapshot = append(snapshot, Snapshot{GVK: key.GVK, Name: key.Name, Object: v.Object})
	}
	slices.SortFunc(snapshot, func(a, b Snapshot) int {
		return cmp.Or(strings.Compare(kindName(a.GVK), kindName(b.GVK)), strings.Compare(a.Name, b.Name))
	})
	return snapshot
}

// all returns every version recorded, in the order recorded.
func (s *Store) all() []Version {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.versions)
}

func (s *Store) record(v Version) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, ok := s.latest(v.Key); ok && last.UID == v.UID &&
		last.ResourceVersion == v.ResourceVersion && last.Deleted == v.Deleted {
		return
	}
	s.byKey[v.Key] = append(s.byKey[v.Key], len(s.versions))
	s.versions = append(s.versions, v)
}

// live returns the latest version of every live object that keep accepts,
// ordered by kind, namespace and name.
func (s *Store) live(keep func(Version) bool) []Version {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var current []Version
	for key := range s.byKey {
		v, ok := s.latest(key)
		if !ok || v.Deleted || !keep(v) {
			continue
		}
		current = append(current, v)
	}
	slices.SortFunc(current, func(a, b Version) int { return compareKeys(a.Key, b.Key) })
	return current
}

func (s *Store) latest(key Key) (Version, bool) {
	positions := s.byKey[key]
	if len(positions) == 0 {
		return Version{}, false
	}
	return s.versions[positions[len(positions)-1]], true
}

func (s *Store) latestAt(key Key, t time.Time) (Version, bool) {
	var latest Version
	var found bool
	for _, position := range s.byKey[key] {
		if v := s.versions[position]; !v.Time.After(t) {
			latest, found = v, true
		}
	}
	return latest, found
}

func (s *Store) isManaged(v Version) bool {
	if !slices.Contains(s.opts.Manages, v.GVK) || s.botbox[v.Key] {
		return false
	}
	return s.opts.Selector == nil || s.opts.Selector.Matches(labels.Set(v.Labels))
}

func newVersion(gvk schema.GroupVersionKind, obj *unstructured.Unstructured, at time.Time, deleted bool) Version {
	obj = obj.DeepCopy()
	return Version{
		Key:                Key{GVK: gvk, Namespace: obj.GetNamespace(), Name: obj.GetName()},
		UID:                obj.GetUID(),
		ResourceVersion:    obj.GetResourceVersion(),
		Time:               at,
		Generation:         obj.GetGeneration(),
		ObservedGeneration: observedGeneration(obj),
		Finalizers:         obj.GetFinalizers(),
		OwnerReferences:    obj.GetOwnerReferences(),
		DeletionTimestamp:  obj.GetDeletionTimestamp(),
		Labels:             obj.GetLabels(),
		Deleted:            deleted,
		Object:             obj,
	}
}

// observedGeneration reads status.observedGeneration, or, where a target
// reports it per condition instead, the least generation every condition has
// caught up to (DESIGN.md §5.3).
func observedGeneration(obj *unstructured.Unstructured) *int64 {
	if top, found, err := unstructured.NestedInt64(obj.Object, "status", "observedGeneration"); found && err == nil {
		return &top
	}
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	var least *int64
	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		observed, found, err := unstructured.NestedInt64(condition, "observedGeneration")
		if !found || err != nil {
			continue
		}
		if least == nil || observed < *least {
			least = &observed
		}
	}
	return least
}

func compareKeys(a, b Key) int {
	return cmp.Or(
		strings.Compare(kindName(a.GVK), kindName(b.GVK)),
		strings.Compare(a.Namespace, b.Namespace),
		strings.Compare(a.Name, b.Name),
	)
}

// kindName renders a kind the way DESIGN.md §8.1 writes `manages`:
// group/version/Kind, and version/Kind in the core group.
func kindName(gvk schema.GroupVersionKind) string {
	if gvk.Group == "" {
		return gvk.Version + "/" + gvk.Kind
	}
	return gvk.Group + "/" + gvk.Version + "/" + gvk.Kind
}
