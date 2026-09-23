package run

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse/botbox/pkg/launch"
	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/proxy"
	"github.com/rosenhouse/botbox/pkg/target"
)

var (
	widgetKind        = schema.GroupVersionKind{Group: "toy.botbox", Version: "v1", Kind: "Widget"}
	configMapKind     = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	widgetResource    = schema.GroupVersionResource{Group: "toy.botbox", Version: "v1", Resource: "widgets"}
	configMapResource = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
)

// unreachable points at nothing, so a test that reaches it fails rather than
// discovering the resources for itself.
func unreachable() *rest.Config { return &rest.Config{Host: "http://127.0.0.1:1"} }

func mapperFor(kinds ...schema.GroupVersionKind) meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range kinds {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return mapper
}

func TestValidateOptions(t *testing.T) {
	toy := &target.Target{Name: "toy-widget", Primary: widgetKind}
	for _, test := range []struct {
		name   string
		target *target.Target
		opts   Options
		want   string
	}{
		{name: "an output directory and a target are enough", target: toy, opts: Options{Dir: "out"}},
		{name: "no target", opts: Options{Dir: "out"}, want: "target"},
		{name: "no output directory", target: toy, want: "output directory"},
		{
			name:   "a garbage-collected cluster without a config",
			target: toy,
			opts:   Options{Dir: "out", GarbageCollected: true},
			want:   "garbage",
		},
		{
			name:   "a garbage-collected cluster with a config",
			target: toy,
			opts:   Options{Dir: "out", GarbageCollected: true, Config: &rest.Config{}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validate(test.target, test.opts)
			switch {
			case test.want == "" && err != nil:
				t.Errorf("validate returned %v, want the options accepted.", err)
			case test.want != "" && err == nil:
				t.Errorf("validate accepted the options, want an error naming %q.", test.want)
			case test.want != "" && !strings.Contains(err.Error(), test.want):
				t.Errorf("validate returned %q, want it to name %q.", err, test.want)
			}
		})
	}
}

func TestNewNamespaceNameIsFreshAndServable(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		name := newNamespaceName()
		if problems := validation.IsDNS1123Label(name); len(problems) > 0 {
			t.Fatalf("The name %q is not a namespace name: %v", name, problems)
		}
		if !strings.HasPrefix(name, namespacePrefix) {
			t.Fatalf("The name %q does not start with %q.", name, namespacePrefix)
		}
		if seen[name] {
			t.Fatalf("The name %q is reused; a run namespace never is (DESIGN.md §5.5).", name)
		}
		seen[name] = true
	}
}

// The Observer watches what the Runner and the collector act on.
func TestTheObserverWatchesTheTargetsKinds(t *testing.T) {
	h := &Harness{
		Namespace: "botbox-run-1",
		mapper:    mapperFor(),
		target:    &target.Target{Primary: widgetKind, Manages: []schema.GroupVersionKind{configMapKind}},
	}

	opts := h.observeOptions()

	if want := []schema.GroupVersionKind{widgetKind, configMapKind}; !slices.Equal(opts.Kinds, want) {
		t.Errorf("The Observer watches %v, want %v.", opts.Kinds, want)
	}
}

// Start builds the run's one mapper, so a discovery that fails ends the run.
func TestStartReportsADiscoveryThatFailed(t *testing.T) {
	toy := &target.Target{Name: "toy-widget", Primary: widgetKind}

	h, err := Start(t.Context(), toy, Options{Dir: t.TempDir(), Config: unreachable()})

	if err == nil {
		t.Fatal("Start discovered the API resources of a server that is not listening.")
	}
	if h != nil {
		t.Error("Start returned a Harness together with an error.")
	}
	if !strings.Contains(err.Error(), "discovering") {
		t.Errorf("Start returned %q, which does not say that discovery failed.", err)
	}
}

// The Runner resolves the kinds it acts on through the run's one mapper, and
// never discovers for itself.
func TestNewLiveRunResolvesThroughTheHarnessMapper(t *testing.T) {
	h := &Harness{
		Namespace: "botbox-run-1",
		Config:    unreachable(),
		mapper:    mapperFor(widgetKind, configMapKind),
	}
	toy := &target.Target{Primary: widgetKind, Manages: []schema.GroupVersionKind{configMapKind}}

	live, err := newLiveRun(h, toy)

	if err != nil {
		t.Fatalf("newLiveRun returned an error: %v", err)
	}
	want := map[schema.GroupVersionKind]schema.GroupVersionResource{
		widgetKind:    widgetResource,
		configMapKind: configMapResource,
	}
	if !maps.Equal(live.resources, want) {
		t.Errorf("The Runner resolved %v, want %v.", live.resources, want)
	}
}

// The Runner names each fault on the run's proxy by the ID the proxy gave it.
func TestTheLiveRunDrivesTheProxysFaults(t *testing.T) {
	p, err := proxy.Start(unreachable(), proxy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Stop() })
	live := &liveRun{h: &Harness{Proxy: p}}
	spec := proxy.FaultSpec{Action: proxy.Error{Code: http.StatusInternalServerError}}
	get := func() int {
		resp, err := http.Get(p.URL() + "/api/v1/namespaces/ns1/configmaps")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	kept, removed := live.addFault(spec), live.addFault(spec)

	live.removeFault(removed)
	get()

	if window := live.faultWindow(removed); window.Retired.IsZero() {
		t.Error("The fault removed is still applying.")
	}
	if window := live.faultWindow(kept); window.First.IsZero() || !window.Retired.IsZero() {
		t.Errorf("The fault kept ran %+v, want it applied and still applying.", window)
	}
	live.clearFaults()
	if status := get(); status == http.StatusInternalServerError {
		t.Error("A request after clearFaults was faulted.")
	}
}

// The fixtures resolve through the same mapper.
func TestApplyFixturesResolvesThroughTheHarnessMapper(t *testing.T) {
	fixture := &unstructured.Unstructured{}
	fixture.SetGroupVersionKind(configMapKind)
	fixture.SetName("fixture")
	h := &Harness{
		Namespace: "botbox-run-1",
		Config:    unreachable(),
		mapper:    mapperFor(widgetKind),
		target:    &target.Target{Primary: widgetKind, Fixtures: []*unstructured.Unstructured{fixture}},
	}

	err := h.applyFixtures(t.Context())

	if err == nil {
		t.Fatal("applyFixtures applied a fixture whose kind the mapper cannot resolve.")
	}
	if !strings.Contains(err.Error(), "resolving the fixture") {
		t.Errorf("applyFixtures returned %q, which does not say it could not resolve the fixture.", err)
	}
}

func TestReady(t *testing.T) {
	holds := func(*unstructured.Unstructured) (bool, error) { return true, nil }
	fails := func(*unstructured.Unstructured) (bool, error) { return false, nil }
	errs := func(*unstructured.Unstructured) (bool, error) {
		return false, errors.New("no such field: status.ready")
	}
	widget := observe.Version{Object: &unstructured.Unstructured{}}

	for _, test := range []struct {
		name      string
		predicate target.ReadyFunc
		observed  []observe.Version
		want      bool
	}{
		{name: "it holds on the one CR", predicate: holds, observed: []observe.Version{widget}, want: true},
		{name: "it fails on one of two CRs", predicate: fails, observed: []observe.Version{widget, widget}},
		{name: "it cannot be evaluated", predicate: errs, observed: []observe.Version{widget}},
		{name: "no CR is left to be ready", predicate: fails, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ready(test.predicate, test.observed); got != test.want {
				t.Errorf("ready reported %t, want %t.", got, test.want)
			}
		})
	}
}

// launcherReporting is a launcher that only reports status.
type launcherReporting struct {
	launch.Launcher
	status launch.Status
}

func (l launcherReporting) Status() launch.Status { return l.status }

// harnessOver reads a quiet, empty run namespace and the target as status says.
func harnessOver(status launch.Status) *Harness {
	return &Harness{
		target:   &target.Target{Primary: widgetKind},
		Observer: &observe.Observer{Store: observe.NewStore(observe.Options{Namespace: "botbox-run-1"})},
		Launcher: launcherReporting{status: status},
	}
}

// A target waiting out a restart backoff is down, whatever state it left.
func TestATargetWaitingToRestartIsNotReady(t *testing.T) {
	since := time.Now()
	for _, test := range []struct {
		name   string
		status launch.Status
		ready  bool
	}{
		{"running", launch.Status{Running: true, Started: since.Add(-time.Minute)}, true},
		{"waiting to restart", launch.Status{Running: true, Restarting: true, Started: since.Add(-time.Minute)}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if ready, _ := harnessOver(test.status).state(since); ready != test.ready {
				t.Errorf("A target %s reads as ready: %t, want %t.", test.name, ready, test.ready)
			}
		})
	}
}

// A restarted target has to hold still for T_stable too, or a wait converges
// before the new process has done anything.
func TestARestartCountsAsAChange(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	restarted := since.Add(time.Second)

	_, changed := harnessOver(launch.Status{Running: true, Started: restarted}).state(since)

	if !changed.Equal(restarted) {
		t.Errorf("The run last changed at %v, want the restart at %v.", changed, restarted)
	}
}

// Each exit of a supervised target is recorded with the line its own process
// wrote as it stopped, though every process writes to one log.
func TestTheHarnessRecordsWhatTheTargetWroteAsItExited(t *testing.T) {
	dir := t.TempDir()
	panicked := filepath.Join(dir, "panicked")
	log, err := os.Create(filepath.Join(dir, targetLogFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	binary := launch.NewBinary(launch.Options{
		Path: "/bin/sh",
		Args: []string{"-c", `if [ -e "$0" ]; then echo "E0923 lost the lease"; exit 1; fi
			echo "panic: first"; echo "goroutine 1 [running]:"; : > "$0"; exit 2`, panicked},
		Log:     log,
		Backoff: launch.MaxBackoff,
	})
	t.Cleanup(func() { _ = binary.Stop(context.Background()) })
	live := &liveRun{h: &Harness{dir: dir, Launcher: binary}}
	if err := binary.Start(t.Context(), "kubeconfig"); err != nil {
		t.Fatal(err)
	}

	live.supervise()

	for deadline := time.Now().Add(10 * time.Second); len(live.exits()) < 2 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	exits := live.exits()
	if len(exits) != 2 {
		t.Fatalf("The harness recorded the exits %+v, want the first and the one after its restart.", exits)
	}
	for i, want := range []struct {
		status, said string
		backoff      time.Duration
	}{
		{"exit status 2", "panic: first", 0},
		{"exit status 1", "E0923 lost the lease", launch.MaxBackoff},
	} {
		exit := exits[i]
		if exit.Said != want.said || exit.Err == nil || exit.Err.Error() != want.status || exit.At.IsZero() {
			t.Errorf("Exit %d is %+v, want %s after the target wrote %q.", i+1, exit, want.status, want.said)
		}
		if down := exit.Restart.Sub(exit.At); down > want.backoff || down < want.backoff-time.Second {
			t.Errorf("Exit %d restarts %v after it, want %v.", i+1, down, want.backoff)
		}
	}
}
