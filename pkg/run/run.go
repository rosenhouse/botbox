// Package run brings the machinery of one run up and down (DESIGN.md §5.5): a
// test cluster, a namespace private to the run, the proxy the target talks to,
// the Observer, the garbage-collector emulation and the target process. The
// Runner drives it; ops, invariants and shrinking are not here.
package run

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse/botbox/pkg/cluster"
	"github.com/rosenhouse/botbox/pkg/launch"
	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/proxy"
	"github.com/rosenhouse/botbox/pkg/target"
)

// The run directory holds these files (DESIGN.md §11).
const (
	kubeconfigFile = "kubeconfig"
	targetLogFile  = "target.log"
	requestsFile   = "requests.jsonl"
	objectsFile    = "objects.jsonl"
	sequenceFile   = "sequence.json"
)

// namespacePrefix opens the name of a run namespace.
const namespacePrefix = "botbox-run-"

// Options configure one run.
type Options struct {
	// Dir receives the run's files: the target's kubeconfig and target.log,
	// and the recordings Stop writes (DESIGN.md §11).
	Dir string
	// Seed drives the proxy's fault sampling, so that a replay faults the same
	// requests. Run takes it from the sequence.
	Seed int64
	// Config reaches a control plane the caller already started. Runs share
	// one, because the shrinker replays a sequence many times (DESIGN.md §5.5).
	// A nil Config starts an envtest cluster for this run alone.
	Config *rest.Config
	// ControllerManager says the cluster runs kube-controller-manager, as kind
	// does and envtest does not. Its garbage collector replaces botbox's
	// emulation, and botbox waits for what it adds to every namespace.
	ControllerManager bool
	// NamespaceDefaultsWithin bounds the wait for what the controller manager
	// adds to the run namespace. Zero takes 30 s.
	NamespaceDefaultsWithin time.Duration
	// Check evaluates the invariants and properties at each checkpoint. Run
	// requires it; Start does not use it.
	Check Checker
	// MaxManaged ends a run whose namespace holds more managed objects, as a
	// harness limit rather than a finding. Zero takes the default of
	// DESIGN.md §5.5.
	MaxManaged int
}

func (o Options) maxManaged() int {
	if o.MaxManaged > 0 {
		return o.MaxManaged
	}
	return defaultMaxManaged
}

func (o Options) namespaceDefaultsWithin() time.Duration {
	if o.NamespaceDefaultsWithin > 0 {
		return o.NamespaceDefaultsWithin
	}
	return defaultNamespaceDefaultsWithin
}

// Harness is one run's machinery.
type Harness struct {
	// Namespace is private to this run and never reused (DESIGN.md §5.5).
	Namespace string
	// Config reaches the API server directly. botbox's own writes never go
	// through the proxy (DESIGN.md §5.3).
	Config   *rest.Config
	Proxy    *proxy.Proxy
	Observer *observe.Observer
	Launcher launch.Launcher

	// mapper resolves a kind to the resource it is served at. The run builds
	// one for everything it starts, once the CRDs are installed.
	mapper meta.RESTMapper
	target *target.Target
	dir    string
	down   teardown
	// unresolved is what the collector could not resolve, once it stops.
	unresolved []cluster.Unresolved

	mu sync.Mutex
	// exited records each exit of a supervised target, and logQuoted is where
	// target.log ended at the last of them.
	exited    []Exit
	logQuoted int64
}

// Start brings the run up in the order DESIGN.md §5.5 requires and leaves the
// target running. A failure takes back down whatever came up. The caller must
// call Stop.
func Start(ctx context.Context, t *target.Target, opts Options) (*Harness, error) {
	if err := validate(t, opts); err != nil {
		return nil, fmt.Errorf("starting the run: %w", err)
	}
	h := &Harness{target: t, dir: opts.Dir}
	if err := h.start(ctx, opts); err != nil {
		return nil, errors.Join(fmt.Errorf("starting the run: %w", err), h.down.run(ctx))
	}
	return h, nil
}

func validate(t *target.Target, opts Options) error {
	if t == nil {
		return errors.New("a target is required")
	}
	if opts.Dir == "" {
		return errors.New("an output directory is required")
	}
	if opts.ControllerManager && opts.Config == nil {
		return errors.New("a cluster with a controller manager must come with a Config: botbox starts envtest, which runs none")
	}
	return nil
}

func (h *Harness) start(ctx context.Context, opts Options) error {
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return fmt.Errorf("creating the run directory: %w", err)
	}
	targetLog, err := os.Create(filepath.Join(opts.Dir, targetLogFile))
	if err != nil {
		return fmt.Errorf("creating %s: %w", targetLogFile, err)
	}
	h.down.push("closing "+targetLogFile, func(context.Context) error { return targetLog.Close() })

	if h.Config = opts.Config; h.Config == nil {
		started, err := cluster.Start(cluster.Options{CRDPaths: h.target.CRDs})
		if err != nil {
			return err
		}
		h.down.push("stopping the test cluster", func(context.Context) error { return started.Stop() })
		h.Config = started.Config()
	}
	if h.mapper, err = cluster.NewRESTMapper(h.Config); err != nil {
		return err
	}

	core, err := kubernetes.NewForConfig(h.Config)
	if err != nil {
		return fmt.Errorf("building botbox's client: %w", err)
	}
	if err := h.createNamespace(ctx, core); err != nil {
		return err
	}
	h.down.push("deleting the run namespace", func(ctx context.Context) error {
		return deleteNamespace(ctx, core, h.Namespace)
	})

	if h.Proxy, err = proxy.Start(h.Config, proxy.Options{Seed: opts.Seed}); err != nil {
		return err
	}
	h.down.push("stopping the proxy", func(context.Context) error { return h.Proxy.Stop() })
	kubeconfig := filepath.Join(opts.Dir, kubeconfigFile)
	if err := h.Proxy.Kubeconfig(kubeconfig, h.Namespace); err != nil {
		return err
	}

	h.Observer, err = observe.Start(h.Config, h.observeOptions())
	if err != nil {
		return err
	}
	h.down.push("stopping the observer", func(context.Context) error { h.Observer.Stop(); return nil })
	if err := h.Observer.WaitForSync(ctx); err != nil {
		return err
	}
	if opts.ControllerManager {
		err := awaitNamespaceDefaults(ctx, h.Observer.Store, h.target.WatchedKinds(), opts.namespaceDefaultsWithin(), sleep)
		if err != nil {
			return err
		}
	}
	excludePresent(h.Observer.Store, h.target.WatchedKinds())

	if !opts.ControllerManager {
		collector, err := cluster.StartCollector(h.Config, cluster.CollectorOptions{
			Namespace: h.Namespace,
			Kinds:     h.target.WatchedKinds(),
			Mapper:    h.mapper,
		})
		if err != nil {
			return err
		}
		h.down.push("stopping the collector", func(context.Context) error {
			h.unresolved = collector.Stop()
			return nil
		})
	}

	if err := h.applyFixtures(ctx); err != nil {
		return err
	}

	h.Launcher = launch.NewBinary(launch.Options{
		Path:      h.target.Launch.Binary,
		Args:      h.target.Launch.Args,
		Namespace: h.Namespace,
		Env:       h.target.Launch.Env,
		Log:       targetLog,
	})
	if err := h.Launcher.Start(ctx, kubeconfig); err != nil {
		return err
	}
	h.down.push("stopping the target", h.Launcher.Stop)
	return nil
}

// Stop takes the run down in the reverse of the order Start brought it up, and
// writes the run's recordings (DESIGN.md §5.7). Emptying the namespace is the
// Runner's own step (§5.5).
func (h *Harness) Stop(ctx context.Context) error {
	return errors.Join(h.down.run(ctx), h.writeRecordings())
}

func (h *Harness) createNamespace(ctx context.Context, core kubernetes.Interface) error {
	name := newNamespaceName()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if _, err := core.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating the run namespace: %w", err)
	}
	h.Namespace = name
	return nil
}

func deleteNamespace(ctx context.Context, core kubernetes.Interface, name string) error {
	err := core.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// newNamespaceName returns a name no other run takes (DESIGN.md §5.5), so that
// a namespace left terminating by envtest is harmless.
func newNamespaceName() string {
	return namespacePrefix + strings.ToLower(rand.Text()[:8])
}

// observeOptions ask the Observer to watch what the Runner and the collector
// act on, and to attribute only the managed kinds to the target.
func (h *Harness) observeOptions() observe.Options {
	return observe.Options{
		Namespace: h.Namespace,
		Kinds:     h.target.WatchedKinds(),
		Manages:   h.target.Manages,
		Mapper:    h.mapper,
		Selector:  h.target.Selector,
	}
}

// applyFixtures creates the target's fixtures in the run namespace and tells
// the Observer botbox created them, so that they never count as managed
// (DESIGN.md §6).
func (h *Harness) applyFixtures(ctx context.Context) error {
	if len(h.target.Fixtures) == 0 {
		return nil
	}
	client, err := dynamic.NewForConfig(h.Config)
	if err != nil {
		return fmt.Errorf("building botbox's dynamic client: %w", err)
	}
	for _, declared := range h.target.Fixtures {
		fixture := declared.DeepCopy()
		fixture.SetNamespace(h.Namespace)
		gvk := fixture.GroupVersionKind()
		mapping, err := h.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return fmt.Errorf("resolving the fixture %s %s: %w", gvk.Kind, fixture.GetName(), err)
		}
		if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
			return fmt.Errorf("the fixture %s %s is cluster-scoped, and a run owns one namespace", gvk.Kind, fixture.GetName())
		}
		created, err := client.Resource(mapping.Resource).Namespace(h.Namespace).
			Create(ctx, fixture, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating the fixture %s %s: %w", gvk.Kind, fixture.GetName(), err)
		}
		h.Observer.Exclude(gvk, created.GetName())
	}
	return nil
}

func (h *Harness) writeRecordings() error {
	return errors.Join(
		writeFile(filepath.Join(h.dir, requestsFile), h.Proxy.WriteLog),
		writeFile(filepath.Join(h.dir, objectsFile), h.Observer.WriteHistory),
	)
}

// namespaceDefaults are what kube-controller-manager adds to every namespace.
// Kubernetes' own e2e framework waits for the same two.
var namespaceDefaults = []struct {
	gvk  schema.GroupVersionKind
	name string
}{
	{schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}, "default"},
	{schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, "kube-root-ca.crt"},
}

const defaultNamespaceDefaultsWithin = 30 * time.Second

// awaitNamespaceDefaults waits for the store to hold each namespace default
// of a kind the run watches.
func awaitNamespaceDefaults(ctx context.Context, store *observe.Store, watched []schema.GroupVersionKind,
	within time.Duration, sleep func(context.Context, time.Duration) error) error {
	deadline := time.Now().Add(within)
	for _, object := range namespaceDefaults {
		if !slices.Contains(watched, object.gvk) {
			continue
		}
		for len(store.HistoryOf(object.gvk, object.name)) == 0 {
			if !time.Now().Before(deadline) {
				return fmt.Errorf("the cluster created no %s %s in the run namespace within %v. "+
					"kube-controller-manager creates one in every namespace, and botbox waits for it so as not to count it as the target's",
					kindName(object.gvk), object.name, within)
			}
			if err := sleep(ctx, settlePoll); err != nil {
				return err
			}
		}
	}
	return nil
}

// excludePresent excludes every object already in the run namespace. The
// target has not started, so none of them is its.
func excludePresent(store *observe.Store, watched []schema.GroupVersionKind) {
	for _, gvk := range watched {
		for _, version := range store.Current(gvk) {
			store.Exclude(gvk, version.Name)
		}
	}
}

func writeFile(path string, write func(io.Writer) error) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return errors.Join(write(file), file.Close())
}
