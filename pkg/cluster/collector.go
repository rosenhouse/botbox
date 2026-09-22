package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const (
	// The collector sweeps on watch events, so its watches never resync.
	noResync = 0
	// cacheSyncTimeout bounds the wait for the watches to catch up.
	cacheSyncTimeout = time.Minute
	// retryDelay is how long the collector waits to sweep again after a call
	// to the API server failed. It stays well inside §5.8's one-second bound.
	retryDelay = 250 * time.Millisecond
)

// CollectorOptions configure the garbage-collector emulation.
type CollectorOptions struct {
	// Namespace is the run namespace. The collector looks nowhere else.
	Namespace string
	// Kinds are the namespaced kinds the collector watches. An owner of any
	// other kind counts as live.
	Kinds []schema.GroupVersionKind
	// Mapper resolves those kinds to the resources the collector lists.
	Mapper apimeta.RESTMapper
	// Log defaults to slog.Default().
	Log *slog.Logger
}

// Validate reports why the options cannot start a collector.
func (o CollectorOptions) Validate() error {
	if o.Namespace == "" {
		return errors.New("a run namespace is required")
	}
	if len(o.Kinds) == 0 {
		return errors.New("at least one kind to watch is required")
	}
	if o.Mapper == nil {
		return errors.New("a RESTMapper is required")
	}
	return nil
}

// Collector deletes an object of a watched kind once every owner in its
// ownerReferences is gone, as kube-controller-manager would. envtest runs no
// controller manager (DESIGN.md §5.8). The collector holds its own client, so
// its writes reach the API server directly and never the proxy.
type Collector struct {
	client     metadata.Interface
	namespace  string
	watched    map[schema.GroupVersionKind]watchedKind
	log        *slog.Logger
	unresolved map[ownerKey]bool

	informers metadatainformer.SharedInformerFactory
	events    chan struct{}
	cancel    context.CancelFunc
	stopped   chan struct{}
}

// watchedKind is one kind the collector watches.
type watchedKind struct {
	resource schema.GroupVersionResource
	store    cache.Store
}

// StartCollector runs the collector over opts.Namespace until Stop.
func StartCollector(config *rest.Config, opts CollectorOptions) (*Collector, error) {
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("starting the collector: %w", err)
	}
	config = collectorConfig(config)
	client, err := metadata.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("building the collector's client: %w", err)
	}
	resources, err := namespacedResources(opts.Mapper, opts.Kinds)
	if err != nil {
		return nil, err
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	c := &Collector{
		client:     client,
		namespace:  opts.Namespace,
		watched:    map[schema.GroupVersionKind]watchedKind{},
		log:        log,
		unresolved: map[ownerKey]bool{},
		informers:  metadatainformer.NewFilteredSharedInformerFactory(client, noResync, opts.Namespace, nil),
		events:     make(chan struct{}, 1),
		stopped:    make(chan struct{}),
	}
	sweepOn := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.notify() },
		UpdateFunc: func(any, any) { c.notify() },
		DeleteFunc: func(any) { c.notify() },
	}
	for gvk, resource := range resources {
		informer := c.informers.ForResource(resource).Informer()
		if _, err := informer.AddEventHandler(sweepOn); err != nil {
			return nil, fmt.Errorf("watching %s: %w", gvk, err)
		}
		c.watched[gvk] = watchedKind{resource: resource, store: informer.GetStore()}
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	if err := c.sync(ctx); err != nil {
		cancel()
		c.informers.Shutdown()
		return nil, err
	}
	go c.run(ctx)
	return c, nil
}

// Stop stops the collector. A sweep in progress is abandoned.
func (c *Collector) Stop() {
	c.cancel()
	<-c.stopped
	c.informers.Shutdown()
}

// collectorConfig is the collector's own connection to the API server. Its
// user agent tells the collector's writes from the target's.
func collectorConfig(config *rest.Config) *rest.Config {
	config = rest.CopyConfig(config)
	config.UserAgent = "botbox-collector"
	return config
}

// namespacedResources maps each kind to the resource it is served at.
// Cluster-scoped kinds are out of scope in phase 1 (DESIGN.md §15, D13).
func namespacedResources(mapper apimeta.RESTMapper, kinds []schema.GroupVersionKind) (map[schema.GroupVersionKind]schema.GroupVersionResource, error) {
	resources := map[schema.GroupVersionKind]schema.GroupVersionResource{}
	for _, gvk := range kinds {
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, fmt.Errorf("resolving the kind %s: %w", gvk, err)
		}
		if mapping.Scope.Name() != apimeta.RESTScopeNameNamespace {
			return nil, fmt.Errorf("the collector watches namespaced kinds only, and %s is cluster-scoped", gvk)
		}
		// The mapping's kind carries the version the API server serves, which
		// is the version an ownerReference names.
		resources[mapping.GroupVersionKind] = mapping.Resource
	}
	return resources, nil
}

// sync starts the watches and waits for their caches, so that the first sweep
// sees every object already in the namespace.
func (c *Collector) sync(ctx context.Context) error {
	c.informers.Start(ctx.Done())
	deadline, cancel := context.WithTimeout(ctx, cacheSyncTimeout)
	defer cancel()
	for resource, synced := range c.informers.WaitForCacheSync(deadline.Done()) {
		if !synced {
			return fmt.Errorf("syncing the collector's watch on %s", resource)
		}
	}
	return nil
}

func (c *Collector) run(ctx context.Context) {
	defer close(c.stopped)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.events:
			c.sweep(ctx, c.objects())
		}
	}
}

// notify asks for a sweep. One pending request stands for any number of
// events, which is what keeps a burst to a single sweep.
func (c *Collector) notify() {
	select {
	case c.events <- struct{}{}:
	default:
	}
}

// sweep deletes every object whose owners are all gone.
func (c *Collector) sweep(ctx context.Context, objects []object) {
	live := c.liveOwners(ctx, objects)
	for _, obj := range objects {
		collect, unresolved := live.collectible(obj.meta.OwnerReferences)
		c.logUnresolved(unresolved)
		if collect {
			c.delete(ctx, obj)
		}
	}
}

// object is one watched object in the run namespace.
type object struct {
	resource schema.GroupVersionResource
	meta     *metav1.PartialObjectMetadata
}

// objects lists what the watches hold in the run namespace. An object already
// marked for deletion is left out, because its deletion is under way.
func (c *Collector) objects() []object {
	var found []object
	for _, watched := range c.watched {
		for _, cached := range watched.store.List() {
			meta, ok := cached.(*metav1.PartialObjectMetadata)
			if !ok || meta.Namespace != c.namespace || meta.DeletionTimestamp != nil {
				continue
			}
			found = append(found, object{resource: watched.resource, meta: meta})
		}
	}
	return found
}

// liveOwners reads each owner the objects name, once per sweep. The read is
// live because the watch on one kind can lag the watch on another, and a
// dependent that arrives before its owner must not be collected.
func (c *Collector) liveOwners(ctx context.Context, objects []object) owners {
	live := owners{watched: c.watched, resolved: map[ownerKey]owner{}}
	for _, obj := range objects {
		for _, ref := range obj.meta.OwnerReferences {
			key := keyOf(ref)
			kind, watched := live.watched[key.gvk]
			_, alreadyRead := live.resolved[key]
			if !watched || alreadyRead {
				continue
			}
			found, err := c.client.Resource(kind.resource).Namespace(c.namespace).
				Get(ctx, key.name, metav1.GetOptions{})
			switch {
			case err == nil:
				live.resolved[key] = owner{uid: found.UID}
			case apierrors.IsNotFound(err):
				live.resolved[key] = owner{}
			default:
				live.resolved[key] = owner{unreadable: true}
				c.logFailure(ctx, "The collector could not read an owner.",
					"kind", key.gvk.Kind, "name", key.name, "error", err)
			}
		}
	}
	return live
}

// delete removes obj unless it changed since the collector read it. The
// preconditions catch a name taken over by another object, and a dependent
// adopted by a live owner in the meantime; either change brings a watch event
// of its own, and with it another sweep.
func (c *Collector) delete(ctx context.Context, obj object) {
	uid, version := obj.meta.UID, obj.meta.ResourceVersion
	preconditions := metav1.Preconditions{UID: &uid, ResourceVersion: &version}
	err := c.client.Resource(obj.resource).Namespace(c.namespace).
		Delete(ctx, obj.meta.Name, metav1.DeleteOptions{Preconditions: &preconditions})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		c.logFailure(ctx, "The collector could not delete an object.",
			"resource", obj.resource.Resource, "name", obj.meta.Name, "error", err)
	}
}

// logFailure reports a call the collector could not make and asks for another
// sweep, because a failed call brings no watch event of its own. A call cut
// short by Stop is not a failure.
func (c *Collector) logFailure(ctx context.Context, message string, args ...any) {
	if ctx.Err() != nil {
		return
	}
	c.log.Error(message, args...)
	time.AfterFunc(retryDelay, c.notify)
}

// ownerKey identifies an owner within the run namespace.
type ownerKey struct {
	gvk  schema.GroupVersionKind
	name string
}

func keyOf(ref metav1.OwnerReference) ownerKey {
	return ownerKey{gvk: schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind), name: ref.Name}
}

// owner is what one live read found. The zero value says the owner is gone.
type owner struct {
	uid types.UID
	// unreadable marks an owner the collector failed to read. It counts as
	// live, so a failed read never costs a dependent its life.
	unreadable bool
}

// owners is what the collector knows about the run namespace: the kinds it
// watches and the owners it resolved.
type owners struct {
	watched  map[schema.GroupVersionKind]watchedKind
	resolved map[ownerKey]owner
}

// collectible reports whether an object carrying these ownerReferences must be
// deleted: it has at least one and every owner is gone. Only an owner read and
// found missing counts as gone, and a name read back under a different UID
// counts the same. An owner of an unwatched kind counts as live and comes back
// as unresolved, so the collector never deletes an object whose owners it
// cannot resolve (DESIGN.md §5.8).
func (o owners) collectible(refs []metav1.OwnerReference) (bool, []metav1.OwnerReference) {
	collect := len(refs) > 0
	var unresolved []metav1.OwnerReference
	for _, ref := range refs {
		key := keyOf(ref)
		if _, watched := o.watched[key.gvk]; !watched {
			unresolved = append(unresolved, ref)
			collect = false
			continue
		}
		if found, read := o.resolved[key]; !read || found.unreadable || found.uid == ref.UID {
			collect = false
		}
	}
	return collect, unresolved
}

// logUnresolved names each owner the collector cannot resolve once per run.
func (c *Collector) logUnresolved(refs []metav1.OwnerReference) {
	for _, ref := range refs {
		key := keyOf(ref)
		if c.unresolved[key] {
			continue
		}
		c.unresolved[key] = true
		c.log.Warn("The collector does not watch this owner's kind and treats it as live.",
			"apiVersion", ref.APIVersion, "kind", ref.Kind, "name", ref.Name)
	}
}
