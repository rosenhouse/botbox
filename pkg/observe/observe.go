package observe

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// noResync stops the informers from re-delivering objects that did not change,
// which would record versions the cluster never had.
const noResync = 0

// Options declare what an Observer watches and how it attributes what it sees.
type Options struct {
	// Namespace is the run namespace. The Observer watches nothing else.
	Namespace string
	// Kinds are what the Observer watches. They include Manages, or what the
	// target manages goes unseen.
	Kinds []schema.GroupVersionKind
	// Manages are the kinds the target declares it manages (DESIGN.md §8.1).
	// The Observer attributes only those to the target.
	Manages []schema.GroupVersionKind
	// Mapper resolves each watched kind to the resource its informer lists.
	Mapper meta.RESTMapper
	// Selector optionally refines attribution to the objects it matches (§6).
	Selector labels.Selector
}

// validate reports why the options cannot start an Observer.
func (o Options) validate() error {
	if o.Namespace == "" {
		return errors.New("the run namespace is empty")
	}
	if len(o.Kinds) == 0 {
		return errors.New("there is no kind to watch")
	}
	if o.Mapper == nil {
		return errors.New("a RESTMapper is required")
	}
	return nil
}

// Observer records the version history of the run namespace from the real API
// server, never through the proxy, and never writes (DESIGN.md §5.3). It
// answers its Store's queries directly.
type Observer struct {
	*Store

	factory  dynamicinformer.DynamicSharedInformerFactory
	synced   []cache.InformerSynced
	stop     chan struct{}
	stopOnce sync.Once
}

// Start watches every kind in opts and records what it sees. The caller must
// call Stop.
func Start(cfg *rest.Config, opts Options) (*Observer, error) {
	if err := opts.validate(); err != nil {
		return nil, fmt.Errorf("starting the observer: %w", err)
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building the observer's dynamic client: %w", err)
	}

	o := &Observer{
		Store:   NewStore(opts),
		factory: dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, noResync, opts.Namespace, nil),
		stop:    make(chan struct{}),
	}
	for _, gvk := range opts.Kinds {
		mapping, err := opts.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, fmt.Errorf("resolving the resource of %s: %w", kindName(gvk), err)
		}
		if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
			return nil, fmt.Errorf("watching %s: it is cluster-scoped, and the observer watches one namespace", kindName(gvk))
		}
		informer := o.factory.ForResource(mapping.Resource).Informer()
		if _, err := informer.AddEventHandler(o.handler(gvk)); err != nil {
			return nil, fmt.Errorf("watching %s: %w", kindName(gvk), err)
		}
		o.synced = append(o.synced, informer.HasSynced)
	}
	o.factory.Start(o.stop)
	return o, nil
}

// WaitForSync blocks until every informer has listed the run namespace.
func (o *Observer) WaitForSync(ctx context.Context) error {
	if !cache.WaitForCacheSync(ctx.Done(), o.synced...) {
		return fmt.Errorf("waiting for the observer to sync: %w", ctx.Err())
	}
	return nil
}

// Stop ends the watches. The history stays readable.
func (o *Observer) Stop() {
	o.stopOnce.Do(func() { close(o.stop) })
	o.factory.Shutdown()
}

// handler records each event as a version timestamped on arrival.
func (o *Observer) handler(gvk schema.GroupVersionKind) cache.ResourceEventHandler {
	record := func(obj any, deleted bool) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
		if deleted {
			o.RecordDeletion(gvk, u, time.Now())
			return
		}
		o.Record(gvk, u, time.Now())
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { record(obj, false) },
		UpdateFunc: func(_, obj any) { record(obj, false) },
		DeleteFunc: func(obj any) {
			if last, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = last.Obj
			}
			record(obj, true)
		},
	}
}
