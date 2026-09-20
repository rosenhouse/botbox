package cluster

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
)

const runNamespace = "run-1"

func configMapOwner(name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: name, UID: uid}
}

func secretOwner(name string) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "v1", Kind: "Secret", Name: name, UID: "uid-secret"}
}

// watchingConfigMaps views a namespace where the collector watches ConfigMaps
// and no other kind.
func watchingConfigMaps(resolved ...map[ownerKey]owner) owners {
	view := owners{
		watched:  map[schema.GroupVersionKind]watchedKind{configMapKind: {resource: configMapResource}},
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
		{"no ownerReferences", nil, watchingConfigMaps(living(parent)), false},
		{"the only owner lives", []metav1.OwnerReference{parent}, watchingConfigMaps(living(parent, other)), false},
		{"the only owner is gone", []metav1.OwnerReference{parent}, watchingConfigMaps(gone(parent)), true},
		{"one of two owners lives", []metav1.OwnerReference{parent, other}, watchingConfigMaps(gone(parent), living(other)), false},
		{"both owners are gone", []metav1.OwnerReference{parent, other}, watchingConfigMaps(gone(parent, other)), true},
		{"the owner's name is reused by a new UID", []metav1.OwnerReference{parent}, watchingConfigMaps(living(reusedName)), true},
		{"the owner's kind is not watched", []metav1.OwnerReference{secretOwner("tls")}, watchingConfigMaps(), false},
		{"the owner could not be read", []metav1.OwnerReference{parent}, watchingConfigMaps(unreadable(parent)), false},
		{"the owner was not read at all", []metav1.OwnerReference{parent}, watchingConfigMaps(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := tc.live.collectible(tc.refs); got != tc.want {
				t.Errorf("collectible returned %t, want %t.", got, tc.want)
			}
		})
	}
}

func TestCollectibleReportsOwnersOfUnwatchedKinds(t *testing.T) {
	parent := configMapOwner("parent", "uid-parent")
	secret := secretOwner("tls")

	collect, unresolved := watchingConfigMaps(gone(parent)).collectible([]metav1.OwnerReference{parent, secret})

	if collect {
		t.Error("collectible chose to delete an object although one of its owners is of an unwatched kind.")
	}
	if len(unresolved) != 1 || unresolved[0].Name != secret.Name {
		t.Errorf("collectible reported %v as unresolved, want the Secret owner alone.", unresolved)
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
		watched:    map[schema.GroupVersionKind]watchedKind{configMapKind: {resource: configMapResource}},
		log:        slog.New(slog.NewTextHandler(&logged, nil)),
		unresolved: map[ownerKey]bool{},
		events:     make(chan struct{}, 1),
		stopped:    make(chan struct{}),
	}
	return c, client, &logged
}

func configMapObject(name string, uid types.UID, owners ...metav1.OwnerReference) object {
	return object{
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

func TestSweepLogsAnOwnerOfAnUnwatchedKind(t *testing.T) {
	c, client, logged := fakeCollector(t)
	secret := secretOwner("tls")

	c.sweep(context.Background(), []object{configMapObject("child", "uid-child", secret)})

	if deleted := deletions(t, client); len(deleted) != 0 {
		t.Errorf("The collector deleted %v although it cannot resolve the owner.", deleted)
	}
	if !strings.Contains(logged.String(), secret.Name) {
		t.Errorf("The collector logged %q, which does not name the unresolved owner.", logged.String())
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
	c.watched[configMapKind] = watchedKind{
		resource: configMapResource,
		store:    seededStore(t, configMapObject("live", "uid-live").meta, terminating, elsewhere),
	}

	found := c.objects()

	if len(found) != 1 || found[0].meta.Name != "live" {
		t.Errorf("objects returned %v, want the live ConfigMap alone.", found)
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
	c.watched[configMapKind] = watchedKind{resource: configMapResource, store: seededStore(t, child.meta)}
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

func TestUnresolvedOwnersAreLoggedOncePerRun(t *testing.T) {
	c, _, logged := fakeCollector(t)
	secret := secretOwner("tls")

	c.logUnresolved([]metav1.OwnerReference{secret})
	c.logUnresolved([]metav1.OwnerReference{secret})

	if lines := strings.Count(logged.String(), "\n"); lines != 1 {
		t.Errorf("The collector logged %d lines, want 1: %s", lines, logged.String())
	}
	if !strings.Contains(logged.String(), secret.Name) {
		t.Errorf("The log line does not name the owner: %s", logged.String())
	}
}

func TestCollectorOptionsValidate(t *testing.T) {
	kinds := []schema.GroupVersionKind{configMapKind}

	if err := (CollectorOptions{Namespace: runNamespace, Kinds: kinds}).Validate(); err != nil {
		t.Errorf("Validate rejected complete options: %v", err)
	}
	if err := (CollectorOptions{Kinds: kinds}).Validate(); err == nil {
		t.Error("Validate accepted options with no namespace.")
	}
	if err := (CollectorOptions{Namespace: runNamespace}).Validate(); err == nil {
		t.Error("Validate accepted options with no kinds to watch.")
	}
}

func TestStartCollectorValidatesBeforeReachingTheAPIServer(t *testing.T) {
	unreachable := &rest.Config{Host: "http://127.0.0.1:1"}

	c, err := StartCollector(unreachable, CollectorOptions{Kinds: []schema.GroupVersionKind{configMapKind}})
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
	unreachable := &rest.Config{Host: "http://127.0.0.1:1"}

	if _, err := StartCollector(unreachable, CollectorOptions{
		Namespace: runNamespace,
		Kinds:     []schema.GroupVersionKind{configMapKind},
	}); err == nil {
		t.Fatal("StartCollector reached an API server that is not listening.")
	}
	if unreachable.UserAgent != "" {
		t.Errorf("StartCollector set the caller's user agent to %q.", unreachable.UserAgent)
	}
}
