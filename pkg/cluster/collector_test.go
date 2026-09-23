package cluster

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
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

// serves resolves ConfigMaps, Secrets, and Widgets at v1 and v1alpha1.
func serves() apimeta.RESTMapper {
	mapper := apimeta.NewDefaultRESTMapper(nil)
	for _, kind := range []schema.GroupVersionKind{configMapKind, secretKind, widgetKind, widgetV1alpha1} {
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
	want := []Unresolved{{DependentKind: configMapKind, DependentName: "child", OwnerKind: secretKind, OwnerName: secret.Name, Served: true}}
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
	want := []Unresolved{{DependentKind: configMapKind, DependentName: "child", OwnerKind: unserved, OwnerName: parent.Name}}
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
	c, client, logged := fakeCollector(t)

	c.sweep(context.Background(), []object{configMapObject("child", "uid-child", secretOwner("tls"))})

	if deleted := deletions(t, client); len(deleted) != 0 {
		t.Errorf("The collector deleted %v although it cannot resolve the owner.", deleted)
	}
	if logged.Len() != 0 {
		t.Errorf("The collector logged %q; the run notes an unresolved owner instead.", logged.String())
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

func TestUnresolvedNamesEachDependentAndOwnerOnce(t *testing.T) {
	c, _, _ := fakeCollector(t)
	secret := secretOwner("tls")
	objects := []object{
		configMapObject("second", "uid-second", secret),
		configMapObject("first", "uid-first", secret),
	}

	c.sweep(context.Background(), objects)
	c.sweep(context.Background(), objects)

	owned := func(dependent string) Unresolved {
		return Unresolved{DependentKind: configMapKind, DependentName: dependent, OwnerKind: secretKind, OwnerName: secret.Name, Served: true}
	}
	if got, want := c.Unresolved(), []Unresolved{owned("first"), owned("second")}; !slices.Equal(got, want) {
		t.Errorf("Unresolved returned %+v, want %+v.", got, want)
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
