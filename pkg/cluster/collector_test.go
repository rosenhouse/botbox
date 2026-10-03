package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/metadata/fake"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

var (
	configMapKind     = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	configMapResource = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secretKind        = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	widgetKind        = schema.GroupVersionKind{Group: "toy.botbox", Version: "v1", Kind: "Widget"}
	widgetResource    = schema.GroupVersionResource{Group: "toy.botbox", Version: "v1", Resource: "widgets"}
	// widgetV1alpha1 is another version the API server serves Widgets at.
	widgetV1alpha1 = schema.GroupVersionKind{Group: "toy.botbox", Version: "v1alpha1", Kind: "Widget"}
)

const runNamespace = "run-1"

// knowsNothing resolves no kind at all.
func knowsNothing() apimeta.RESTMapper { return apimeta.NewDefaultRESTMapper(nil) }

// serves resolves ConfigMaps, and Widgets at v1 and v1alpha1. It knows no
// Secret, which the collector does not watch either.
func serves() apimeta.RESTMapper {
	mapper := apimeta.NewDefaultRESTMapper(nil)
	for _, kind := range []schema.GroupVersionKind{configMapKind, widgetKind, widgetV1alpha1} {
		mapper.Add(kind, apimeta.RESTScopeNamespace)
	}
	return mapper
}

// watchesConfigMapsAndWidgets are the kinds the collector watches in these
// tests.
func watchesConfigMapsAndWidgets() map[schema.GroupKind]watchedKind {
	return map[schema.GroupKind]watchedKind{
		configMapKind.GroupKind(): {kind: configMapKind, resource: configMapResource},
		widgetKind.GroupKind():    {kind: widgetKind, resource: widgetResource},
	}
}

func configMapOwner(name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: name, UID: uid}
}

func secretOwner(name string) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "v1", Kind: "Secret", Name: name, UID: "uid-secret"}
}

func widgetOwner(version schema.GroupVersionKind, name string, uid types.UID) metav1.OwnerReference {
	apiVersion, kind := version.ToAPIVersionAndKind()
	return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid}
}

// watching views a namespace where the collector watches ConfigMaps and
// Widgets, and no other kind.
func watching(resolved ...map[ownerKey]owner) owners {
	view := owners{
		mapper:   serves(),
		watched:  watchesConfigMapsAndWidgets(),
		resolved: map[ownerKey]owner{},
	}
	for _, owners := range resolved {
		maps.Copy(view.resolved, owners)
	}
	return view
}

// read maps each reference to what a read of its owner found.
func read(found func(metav1.OwnerReference) owner, refs []metav1.OwnerReference) map[ownerKey]owner {
	resolved := map[ownerKey]owner{}
	for _, ref := range refs {
		resolved[keyOf(ref)] = found(ref)
	}
	return resolved
}

func living(refs ...metav1.OwnerReference) map[ownerKey]owner {
	return read(func(ref metav1.OwnerReference) owner { return owner{uid: ref.UID} }, refs)
}

func gone(refs ...metav1.OwnerReference) map[ownerKey]owner {
	return read(func(metav1.OwnerReference) owner { return owner{} }, refs)
}

func unreadable(refs ...metav1.OwnerReference) map[ownerKey]owner {
	return read(func(metav1.OwnerReference) owner { return owner{unreadable: true} }, refs)
}

func TestCollectible(t *testing.T) {
	parent := configMapOwner("parent", "uid-parent")
	reusedName := configMapOwner("parent", "uid-parent-again")
	other := configMapOwner("other", "uid-other")

	for _, tc := range []struct {
		name string
		refs []metav1.OwnerReference
		live owners
		want bool
	}{
		{"no ownerReferences", nil, watching(living(parent)), false},
		{"the only owner lives", []metav1.OwnerReference{parent}, watching(living(parent, other)), false},
		{"the only owner is gone", []metav1.OwnerReference{parent}, watching(gone(parent)), true},
		{"one of two owners lives", []metav1.OwnerReference{parent, other}, watching(gone(parent), living(other)), false},
		{"both owners are gone", []metav1.OwnerReference{parent, other}, watching(gone(parent, other)), true},
		{"the owner's name is reused by a new UID", []metav1.OwnerReference{parent}, watching(living(reusedName)), true},
		{"the owner's kind is not watched", []metav1.OwnerReference{secretOwner("tls")}, watching(), false},
		{"the owner could not be read", []metav1.OwnerReference{parent}, watching(unreadable(parent)), false},
		{"the owner was not read at all", []metav1.OwnerReference{parent}, watching(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := tc.live.collectible(configMapObject("child", "uid-child", tc.refs...)); got != tc.want {
				t.Errorf("collectible returned %t, want %t.", got, tc.want)
			}
		})
	}
}

func TestCollectibleReportsOwnersOfUnwatchedKinds(t *testing.T) {
	parent := configMapOwner("parent", "uid-parent")
	secret := secretOwner("tls")

	collect, unresolved := watching(gone(parent)).collectible(configMapObject("child", "uid-child", parent, secret))

	if collect {
		t.Error("collectible chose to delete an object although one of its owners is of an unwatched kind.")
	}
	want := []Unresolved{{DependentKind: configMapKind, DependentName: "child", OwnerKind: secretKind, OwnerName: secret.Name}}
	if !slices.Equal(unresolved, want) {
		t.Errorf("collectible reported %+v as unresolved, want %+v.", unresolved, want)
	}
}

// The garbage collector resolves an owner through whichever version the
// reference names, so long as the API server serves it.
func TestCollectibleResolvesAnOwnerAtAnotherServedVersion(t *testing.T) {
	parent := widgetOwner(widgetV1alpha1, "parent", "uid-parent")
	for _, tc := range []struct {
		name string
		live owners
		want bool
	}{
		{"the owner is gone", watching(gone(parent)), true},
		{"the owner lives", watching(living(parent)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collect, unresolved := tc.live.collectible(configMapObject("child", "uid-child", parent))

			if collect != tc.want {
				t.Errorf("collectible returned %t, want %t.", collect, tc.want)
			}
			if len(unresolved) > 0 {
				t.Errorf("collectible reported %+v as unresolved, want none.", unresolved)
			}
		})
	}
}

func TestCollectibleKeepsAnObjectWhoseOwnerNamesAVersionNotServed(t *testing.T) {
	unserved := schema.GroupVersionKind{Group: widgetKind.Group, Version: "v1beta9", Kind: widgetKind.Kind}
	parent := widgetOwner(unserved, "parent", "uid-parent")

	collect, unresolved := watching(gone(parent)).collectible(configMapObject("child", "uid-child", parent))

	if collect {
		t.Error("collectible chose to delete an object whose owner names a version the API server does not serve.")
	}
	want := []Unresolved{{DependentKind: configMapKind, DependentName: "child", OwnerKind: unserved, OwnerName: parent.Name, Unserved: true}}
	if !slices.Equal(unresolved, want) {
		t.Errorf("collectible reported %+v as unresolved, want %+v.", unresolved, want)
	}
}

// fakeCollector answers from an in-memory API server and logs to the returned
// buffer, so a sweep can be exercised without a control plane.
func fakeCollector(t *testing.T) (*Collector, *fake.FakeMetadataClient, *bytes.Buffer) {
	t.Helper()
	scheme := fake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatalf("Building the test scheme failed: %v", err)
	}
	client := fake.NewSimpleMetadataClient(scheme)
	var logged bytes.Buffer
	c := &Collector{
		client:     client,
		namespace:  runNamespace,
		mapper:     serves(),
		watched:    watchesConfigMapsAndWidgets(),
		log:        slog.New(slog.NewTextHandler(&logged, nil)),
		unresolved: map[Unresolved]bool{},
		events:     make(chan struct{}, 1),
		stopped:    make(chan struct{}),
	}
	return c, client, &logged
}

func configMapObject(name string, uid types.UID, owners ...metav1.OwnerReference) object {
	return object{
		kind:     configMapKind,
		resource: configMapResource,
		meta: &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
			Namespace:       runNamespace,
			Name:            name,
			UID:             uid,
			ResourceVersion: "7",
			OwnerReferences: owners,
		}},
	}
}

func widgetObject(name string, uid types.UID, owners ...metav1.OwnerReference) object {
	obj := configMapObject(name, uid, owners...)
	obj.kind, obj.resource = widgetKind, widgetResource
	return obj
}

func deletions(t *testing.T, client *fake.FakeMetadataClient) []k8stesting.DeleteActionImpl {
	t.Helper()
	var deleted []k8stesting.DeleteActionImpl
	for _, action := range client.Actions() {
		if action, ok := action.(k8stesting.DeleteActionImpl); ok {
			deleted = append(deleted, action)
		}
	}
	return deleted
}

func TestSweepDeletesOnlyTheObjectItRead(t *testing.T) {
	c, client, _ := fakeCollector(t)
	child := configMapObject("child", "uid-child", configMapOwner("parent", "uid-parent"))

	c.sweep(context.Background(), []object{child})

	deleted := deletions(t, client)
	if len(deleted) != 1 || deleted[0].Name != child.meta.Name {
		t.Fatalf("The collector made these deletions: %v, want the child alone.", deleted)
	}
	preconditions := deleted[0].DeleteOptions.Preconditions
	if preconditions == nil || *preconditions.UID != child.meta.UID ||
		*preconditions.ResourceVersion != child.meta.ResourceVersion {
		t.Errorf("The collector deleted the child with preconditions %+v, want its UID and resourceVersion.", preconditions)
	}
}

func TestSweepKeepsAChildOfAnOwnerItCannotResolve(t *testing.T) {
	unserved := schema.GroupVersionKind{Group: widgetKind.Group, Version: "v1beta9", Kind: widgetKind.Kind}
	for _, tc := range []struct {
		name  string
		owner metav1.OwnerReference
	}{
		{"the collector does not watch the kind", secretOwner("tls")},
		{"the API server does not serve the version", widgetOwner(unserved, "parent", "uid-parent")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, client, logged := fakeCollector(t)

			c.sweep(context.Background(), []object{configMapObject("child", "uid-child", tc.owner)})

			if len(client.Actions()) != 0 {
				t.Errorf("The collector made the calls %v, want none for an owner it cannot resolve.", client.Actions())
			}
			if logged.Len() != 0 {
				t.Errorf("The collector logged %q; the run notes an unresolved owner instead.", logged.String())
			}
		})
	}
}

func TestSweepCollectsAChildWhoseOwnerIsGoneAtAnotherServedVersion(t *testing.T) {
	c, client, _ := fakeCollector(t)
	child := configMapObject("child", "uid-child", widgetOwner(widgetV1alpha1, "gone", "uid-gone"))

	c.sweep(context.Background(), []object{child})

	if deleted := deletions(t, client); len(deleted) != 1 {
		t.Errorf("The collector made these deletions: %v, want the child of the Widget that is gone.", deleted)
	}
}

func TestSweepReadsAnOwnerOnceWhateverVersionNamesIt(t *testing.T) {
	c, client, _ := fakeCollector(t)
	objects := []object{
		configMapObject("v1-child", "uid-v1-child", widgetOwner(widgetKind, "parent", "uid-parent")),
		configMapObject("v1alpha1-child", "uid-v1alpha1-child", widgetOwner(widgetV1alpha1, "parent", "uid-parent")),
	}

	c.sweep(context.Background(), objects)

	reads := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "get" {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("The collector read the owner %d times, want once.", reads)
	}
}

func TestSweepKeepsAChildWhoseOwnerLives(t *testing.T) {
	c, client, _ := fakeCollector(t)
	parent := configMapOwner("parent", "uid-parent")
	if err := client.Tracker().Add(&metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Namespace: runNamespace, Name: parent.Name, UID: parent.UID},
	}); err != nil {
		t.Fatalf("Seeding the owner failed: %v", err)
	}

	c.sweep(context.Background(), []object{configMapObject("child", "uid-child", parent)})

	if deleted := deletions(t, client); len(deleted) != 0 {
		t.Errorf("The collector deleted %v although the owner is alive.", deleted)
	}
}

func TestSweepIgnoresADeleteItLost(t *testing.T) {
	configMaps := schema.GroupResource{Resource: "configmaps"}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"the object is already gone", apierrors.NewNotFound(configMaps, "child")},
		{"another object took the name", apierrors.NewConflict(configMaps, "child", errors.New("the UID has changed"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, client, logged := fakeCollector(t)
			client.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})

			c.sweep(context.Background(), []object{
				configMapObject("child", "uid-child", configMapOwner("parent", "uid-parent")),
			})

			if logged.Len() != 0 {
				t.Errorf("The collector reported an expected outcome as a failure: %s", logged.String())
			}
		})
	}
}

func TestSweepRecordsEachDeleteItTriesAndHowItEnded(t *testing.T) {
	configMaps := schema.GroupResource{Resource: "configmaps"}
	for _, tc := range []struct {
		name   string
		err    error
		result string
	}{
		{"the delete succeeds", nil, "deleted"},
		{"the object is already gone", apierrors.NewNotFound(configMaps, "child"), "not found"},
		{"the object changed since the collector read it", apierrors.NewConflict(configMaps, "child",
			errors.New("Precondition failed: ResourceVersion in precondition: 7, ResourceVersion in object meta: 8")), "conflict"},
		{"the API server fails", apierrors.NewInternalError(errors.New("the API server is unwell")), "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, client, logged := fakeCollector(t)
			var sent time.Time
			client.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
				sent = time.Now()
				return true, nil, tc.err
			})
			before := time.Now()

			c.sweep(context.Background(), []object{
				configMapObject("child", "uid-child", configMapOwner("parent", "uid-parent")),
			})

			want := map[string]any{
				"kind": "v1/ConfigMap", "namespace": runNamespace, "name": "child", "uid": "uid-child", "resourceVersion": "7",
				"owners": []any{map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "parent", "uid": "uid-parent", "gone": "not found"}},
				"result": tc.result,
			}
			if tc.err != nil {
				want["error"] = tc.err.Error()
			}
			lines, tried := recorded(t, c)
			if len(lines) != 1 {
				t.Fatalf("The collector recorded %v, want the one delete it tried.", lines)
			}
			if !reflect.DeepEqual(lines[0], want) {
				t.Errorf("The collector recorded\n\t%v\nwant\n\t%v", lines[0], want)
			}
			if tried[0].Before(before) || tried[0].After(sent) {
				t.Errorf("The collector recorded the delete at %v, want when it sent it, between %v and %v.", tried[0], before, sent)
			}
			if failed := tc.result == "error"; (logged.Len() > 0) != failed {
				t.Errorf("The collector logged %q, want a failure logged only where the API server failed.", logged.String())
			}
		})
	}
}

func TestSweepRecordsWhyEachOwnerIsGone(t *testing.T) {
	c, client, _ := fakeCollector(t)
	replaced := configMapOwner("replaced", "uid-replaced")
	if err := client.Tracker().Add(&metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Namespace: runNamespace, Name: replaced.Name, UID: "uid-replaced-again"},
	}); err != nil {
		t.Fatalf("Seeding the owner's successor failed: %v", err)
	}

	c.sweep(context.Background(), []object{
		configMapObject("child", "uid-child", configMapOwner("deleted", "uid-deleted"), replaced),
	})

	want := []any{
		map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "deleted", "uid": "uid-deleted", "gone": "not found"},
		map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "replaced", "uid": "uid-replaced",
			"gone": "another UID", "foundUID": "uid-replaced-again"},
	}
	lines, _ := recorded(t, c)
	if len(lines) != 1 || !reflect.DeepEqual(lines[0]["owners"], want) {
		t.Errorf("The collector recorded %v, want one delete whose owners are\n\t%v", lines, want)
	}
}

func TestWriteLogWritesEachDeleteInTheOrderTried(t *testing.T) {
	c, _, _ := fakeCollector(t)
	parent := configMapOwner("parent", "uid-parent")

	c.sweep(context.Background(), []object{widgetObject("first", "uid-first", parent)})
	c.sweep(context.Background(), []object{configMapObject("second", "uid-second", parent)})

	lines, _ := recorded(t, c)
	var got []string
	for _, line := range lines {
		got = append(got, fmt.Sprint(line["kind"], " ", line["name"]))
	}
	if want := []string{"toy.botbox/v1/Widget first", "v1/ConfigMap second"}; !slices.Equal(got, want) {
		t.Errorf("The collector recorded the deletes %q, want %q.", got, want)
	}
}

func TestWriteLogReportsAWriteThatFails(t *testing.T) {
	c, _, _ := fakeCollector(t)
	c.sweep(context.Background(), []object{configMapObject("child", "uid-child", configMapOwner("parent", "uid-parent"))})

	if err := c.WriteLog(failingWriter{}); err == nil {
		t.Error("WriteLog returned no error, although nothing it wrote was written.")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the disk is full") }

// recorded decodes the collector's log, one JSON object per line, and takes
// out when each delete was tried.
func recorded(t *testing.T, c *Collector) ([]map[string]any, []time.Time) {
	t.Helper()
	var written bytes.Buffer
	if err := c.WriteLog(&written); err != nil {
		t.Fatalf("WriteLog returned an error: %v", err)
	}
	var lines []map[string]any
	var tried []time.Time
	for _, text := range strings.Split(strings.TrimSuffix(written.String(), "\n"), "\n") {
		if text == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			t.Fatalf("The line %q of the collector's log does not parse: %v", text, err)
		}
		at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(line["time"]))
		if err != nil {
			t.Fatalf("The line %q of the collector's log says no time: %v", text, err)
		}
		delete(line, "time")
		lines = append(lines, line)
		tried = append(tried, at)
	}
	return lines, tried
}

func TestASweepCutShortByStopReportsNoFailure(t *testing.T) {
	c, client, logged := fakeCollector(t)
	client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("the request was cancelled"))
	})
	stopped, stop := context.WithCancel(context.Background())
	stop()

	c.sweep(stopped, []object{configMapObject("child", "uid-child", configMapOwner("parent", "uid-parent"))})

	if logged.Len() != 0 {
		t.Errorf("The collector reported its own shutdown as a failure: %s", logged.String())
	}
}

func TestAStoppedCollectorSendsAndRecordsNoDelete(t *testing.T) {
	c, client, _ := fakeCollector(t)
	stopped, stop := context.WithCancel(context.Background())
	stop()

	c.sweep(stopped, []object{configMapObject("child", "uid-child", configMapOwner("parent", "uid-parent"))})

	if deleted := deletions(t, client); len(deleted) != 0 {
		t.Errorf("The stopped collector sent the deletes %v.", deleted)
	}
	if lines, _ := recorded(t, c); len(lines) != 0 {
		t.Errorf("The stopped collector recorded %v, want no delete.", lines)
	}
}

// An owner the collector cannot read counts as live for every dependent that
// names it, whatever UID each of them carries.
func TestSweepKeepsChildrenOfAnUnreadableOwner(t *testing.T) {
	c, client, _ := fakeCollector(t)
	client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("the API server is unwell"))
	})
	objects := []object{
		configMapObject("older-child", "uid-older-child", configMapOwner("parent", "uid-parent")),
		configMapObject("newer-child", "uid-newer-child", configMapOwner("parent", "uid-parent-again")),
	}

	c.sweep(context.Background(), objects)

	if deleted := deletions(t, client); len(deleted) != 0 {
		t.Errorf("The collector deleted %v although it could not read their owner.", deleted)
	}
}

func TestObjectsSkipWhatTheCollectorMustNotDelete(t *testing.T) {
	c, _, _ := fakeCollector(t)
	terminating := configMapObject("terminating", "uid-terminating").meta
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	elsewhere := configMapObject("elsewhere", "uid-elsewhere").meta
	elsewhere.Namespace = "another-run"
	c.watched = map[schema.GroupKind]watchedKind{configMapKind.GroupKind(): {
		kind:     configMapKind,
		resource: configMapResource,
		store:    seededStore(t, configMapObject("live", "uid-live").meta, terminating, elsewhere),
	}}

	found := c.objects()

	if len(found) != 1 || found[0].meta.Name != "live" {
		t.Fatalf("objects returned %v, want the live ConfigMap alone.", found)
	}
	if found[0].kind != configMapKind {
		t.Errorf("objects gave the ConfigMap the kind %v, want %v.", found[0].kind, configMapKind)
	}
}

func seededStore(t *testing.T, objects ...*metav1.PartialObjectMetadata) cache.Store {
	t.Helper()
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	for _, object := range objects {
		if err := store.Add(object); err != nil {
			t.Fatalf("Seeding the store failed: %v", err)
		}
	}
	return store
}

// A failed call brings no watch event, so the collector sweeps again by itself.
func TestASweepIsRetriedAfterAFailedCall(t *testing.T) {
	c, client, _ := fakeCollector(t)
	var reads atomic.Int32
	client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if reads.Add(1) > 1 {
			return false, nil, nil
		}
		return true, nil, apierrors.NewInternalError(errors.New("the API server is unwell"))
	})
	child := configMapObject("child", "uid-child", configMapOwner("parent", "uid-parent"))
	c.watched = map[schema.GroupKind]watchedKind{
		configMapKind.GroupKind(): {kind: configMapKind, resource: configMapResource, store: seededStore(t, child.meta)},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		<-c.stopped
	}()
	go c.run(ctx)

	c.notify()

	deadline := time.After(10 * retryDelay)
	for len(deletions(t, client)) == 0 {
		select {
		case <-deadline:
			t.Fatal("The collector never swept again after a call to the API server failed.")
		case <-time.After(retryDelay / 10):
		}
	}
}

func TestStopNamesTheOwnersTheSweepItCutShortCouldNotResolve(t *testing.T) {
	c, client, _ := fakeCollector(t)
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.informers = metadatainformer.NewSharedInformerFactory(client, noResync)
	reading := make(chan struct{})
	client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		close(reading)
		<-ctx.Done()
		return true, nil, ctx.Err()
	})
	child := configMapObject("child", "uid-child", configMapOwner("parent", "uid-parent"), secretOwner("tls"))
	c.watched = map[schema.GroupKind]watchedKind{
		configMapKind.GroupKind(): {kind: configMapKind, resource: configMapResource, store: seededStore(t, child.meta)},
	}
	go c.run(ctx)
	c.notify()
	<-reading

	got := c.Stop()

	want := []Unresolved{{DependentKind: configMapKind, DependentName: "child", OwnerKind: secretKind, OwnerName: "tls"}}
	if !slices.Equal(got, want) {
		t.Errorf("Stop returned %+v, want %+v.", got, want)
	}
}

func TestNotifyDoesNotBlockWhenASweepIsPending(t *testing.T) {
	c, _, _ := fakeCollector(t)
	notified := make(chan struct{})

	go func() {
		defer close(notified)
		c.notify()
		c.notify()
	}()

	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("notify blocked although a sweep was already pending.")
	}
}

// Each pair of neighbours in want ties on the sort keys before the one that
// orders it, and the keys after would order it the other way. The sweep meets
// them in reverse.
func TestUnresolvedOwnersNameEachDependentAndOwnerOnce(t *testing.T) {
	c, _, _ := fakeCollector(t)
	deployment := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "a", UID: "uid-a"}
	secretX := configMapObject("x", "uid-secret-x", secretOwner("a"))
	secretX.kind = secretKind
	objects := []object{
		secretX,
		configMapObject("y", "uid-y", secretOwner("c"), secretOwner("b"), secretOwner("a")),
		configMapObject("x", "uid-x", deployment, secretOwner("b")),
	}

	c.sweep(context.Background(), objects)
	c.sweep(context.Background(), objects)

	owned := func(dependent schema.GroupVersionKind, name string, owner metav1.OwnerReference) Unresolved {
		return Unresolved{
			DependentKind: dependent, DependentName: name,
			OwnerKind: schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind), OwnerName: owner.Name,
		}
	}
	want := []Unresolved{
		owned(configMapKind, "x", secretOwner("b")),
		owned(configMapKind, "x", deployment),
		owned(configMapKind, "y", secretOwner("a")),
		owned(configMapKind, "y", secretOwner("b")),
		owned(configMapKind, "y", secretOwner("c")),
		owned(secretKind, "x", secretOwner("a")),
	}
	if got := c.unresolvedOwners(); !slices.Equal(got, want) {
		t.Errorf("unresolvedOwners returned\n\t%+v\nwant\n\t%+v", got, want)
	}
}

// A kind given without a version is listed at the one the API server prefers,
// and the collector names its objects at that version.
func TestNamespacedKindsHoldTheVersionTheyList(t *testing.T) {
	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{configMapKind.GroupVersion()})
	mapper.Add(configMapKind, apimeta.RESTScopeNamespace)

	watched, err := namespacedKinds(mapper, []schema.GroupVersionKind{{Kind: configMapKind.Kind}})

	if err != nil {
		t.Fatalf("namespacedKinds returned an error: %v", err)
	}
	if got := watched[configMapKind.GroupKind()].kind; got != configMapKind {
		t.Errorf("namespacedKinds holds the kind %v, want %v.", got, configMapKind)
	}
}

func TestCollectorOptionsValidate(t *testing.T) {
	kinds := []schema.GroupVersionKind{configMapKind}
	mapper := knowsNothing()

	if err := (CollectorOptions{Namespace: runNamespace, Kinds: kinds, Mapper: mapper}).Validate(); err != nil {
		t.Errorf("Validate rejected complete options: %v", err)
	}
	if err := (CollectorOptions{Kinds: kinds, Mapper: mapper}).Validate(); err == nil {
		t.Error("Validate accepted options with no namespace.")
	}
	if err := (CollectorOptions{Namespace: runNamespace, Mapper: mapper}).Validate(); err == nil {
		t.Error("Validate accepted options with no kinds to watch.")
	}
	if err := (CollectorOptions{Namespace: runNamespace, Kinds: kinds}).Validate(); err == nil {
		t.Error("Validate accepted options with no RESTMapper.")
	}
}

// The collector resolves what it watches through the mapper the run built, and
// never discovers for itself.
func TestStartCollectorResolvesTheWatchedKindsThroughTheGivenMapper(t *testing.T) {
	unreachable := &rest.Config{Host: "http://127.0.0.1:1"}

	c, err := StartCollector(unreachable, CollectorOptions{
		Namespace: runNamespace,
		Kinds:     []schema.GroupVersionKind{configMapKind},
		Mapper:    knowsNothing(),
	})

	if err == nil {
		c.Stop()
		t.Fatal("StartCollector watched a kind its mapper cannot resolve.")
	}
	if !strings.Contains(err.Error(), "resolving the kind") {
		t.Errorf("StartCollector returned %q, which does not say it could not resolve the kind.", err)
	}
}

func TestStartCollectorValidatesBeforeReachingTheAPIServer(t *testing.T) {
	unreachable := &rest.Config{Host: "http://127.0.0.1:1"}

	c, err := StartCollector(unreachable, CollectorOptions{
		Kinds:  []schema.GroupVersionKind{configMapKind},
		Mapper: knowsNothing(),
	})
	if err == nil {
		c.Stop()
		t.Fatal("StartCollector accepted options with no namespace.")
	}
	if c != nil {
		t.Error("StartCollector returned a non-nil Collector together with an error.")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Errorf("StartCollector returned %q, which does not name the missing namespace.", err)
	}
}

func TestCollectorConfigIsItsOwn(t *testing.T) {
	callers := &rest.Config{Host: "https://example.invalid"}

	own := collectorConfig(callers)

	if own.UserAgent == "" {
		t.Error("The collector's configuration carries no user agent to tell its writes from the target's.")
	}
	if callers.UserAgent != "" {
		t.Errorf("collectorConfig set the caller's user agent to %q.", callers.UserAgent)
	}
}

func TestStartCollectorLeavesTheCallersConfigAlone(t *testing.T) {
	callers := &rest.Config{Host: "http://127.0.0.1:1"}

	if _, err := StartCollector(callers, CollectorOptions{
		Namespace: runNamespace,
		Kinds:     []schema.GroupVersionKind{configMapKind},
		Mapper:    knowsNothing(),
	}); err == nil {
		t.Fatal("StartCollector started with a mapper that resolves nothing.")
	}
	if callers.UserAgent != "" {
		t.Errorf("StartCollector set the caller's user agent to %q.", callers.UserAgent)
	}
}
