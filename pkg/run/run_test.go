package run

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"

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

// The Runner sets how long a recreate waits for its CR, and moves that time
// once the Observer sees the deletion.
func TestAwaitCRGoneWaitsUntilTheInstantTheRunnerGives(t *testing.T) {
	cr := widget("widget")
	cr.SetNamespace("botbox-run-1")
	live := liveOver(widgetsClient(cr))
	began := time.Now()
	moved, due := began.Add(100*time.Millisecond), began.Add(300*time.Millisecond)
	reads := 0

	gone, err := live.awaitCRGone(t.Context(), "widget", func() time.Time {
		if reads++; reads == 1 {
			return moved
		}
		return due
	})

	if err != nil || gone {
		t.Fatalf("The wait returned (%t, %v), want a CR that stayed.", gone, err)
	}
	if waited := time.Since(began); waited < due.Sub(began) {
		t.Errorf("The wait ended %v in, want it to last until %v.", waited, due.Sub(began))
	}
}

func TestAwaitCRGoneReportsAReadThatFailed(t *testing.T) {
	client := widgetsClient()
	client.PrependReactor("get", "widgets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcd is down")
	})

	_, err := liveOver(client).awaitCRGone(t.Context(), "widget", time.Now)

	if err == nil || !strings.Contains(err.Error(), "etcd is down") {
		t.Errorf("The wait returned %v, want the read's error.", err)
	}
}

// liveOver is a live run of a toy whose T_delete is too short to matter.
func liveOver(client dynamic.Interface) *liveRun {
	return &liveRun{
		h:         &Harness{Namespace: "botbox-run-1"},
		target:    &target.Target{Primary: widgetKind, Timeouts: target.Timeouts{Delete: time.Millisecond}},
		client:    client,
		resources: map[schema.GroupVersionKind]schema.GroupVersionResource{widgetKind: widgetResource},
	}
}

func widgetsClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{widgetResource: "WidgetList"}, objects...)
}

func TestAwaitCleanWaitsOutItsWindow(t *testing.T) {
	store := observe.NewStore(observe.Options{Namespace: "botbox-run-1"})
	cr := widget("widget")
	cr.SetNamespace("botbox-run-1")
	store.Record(widgetKind, cr, time.Now())
	live := &liveRun{h: &Harness{Observer: &observe.Observer{Store: store}}, target: &target.Target{Primary: widgetKind}}
	began := time.Now()

	clean, err := live.awaitClean(t.Context(), 100*time.Millisecond)

	if err != nil || clean {
		t.Fatalf("The wait returned (%t, %v), want a namespace that stayed.", clean, err)
	}
	if waited := time.Since(began); waited < 100*time.Millisecond {
		t.Errorf("The wait ended %v in, want it to last 100ms.", waited)
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

// A settle wait ends at the first reading of a ready that yields no bool,
// rather than waiting out settle on it.
func TestTheHarnessReadsAReadyThatYieldsNoBoolAsAnError(t *testing.T) {
	store := observe.NewStore(observe.Options{Namespace: "botbox-run-x"})
	store.Record(widgetKind, widget("widget"), time.Now())
	yieldsAnInt := &target.Target{Primary: widgetKind, Ready: func(*unstructured.Unstructured) (bool, error) {
		return false, &target.EvalError{Predicate: "ready", Expr: "status.ready", Err: fmt.Errorf("%w: it yielded int64", target.ErrNotBool)}
	}}
	h := &Harness{target: yieldsAnInt, Observer: &observe.Observer{Store: store}}

	_, _, err := h.state(time.Now())

	if !errors.Is(err, target.ErrNotBool) {
		t.Errorf("Reading the run returned the error %v, want the ready that yields no bool.", err)
	}
}

func TestReady(t *testing.T) {
	holds := func(*unstructured.Unstructured) (bool, error) { return true, nil }
	fails := func(*unstructured.Unstructured) (bool, error) { return false, nil }
	errs := func(*unstructured.Unstructured) (bool, error) {
		return false, errors.New("no such field: status.ready")
	}
	yieldsAnInt := func(*unstructured.Unstructured) (bool, error) {
		return false, &target.EvalError{Predicate: "ready", Expr: "status.ready", Err: fmt.Errorf("%w: it yielded int64", target.ErrNotBool)}
	}
	widget := observe.Version{Object: &unstructured.Unstructured{}}
	named := func(name string) observe.Version {
		cr := &unstructured.Unstructured{}
		cr.SetName(name)
		return observe.Version{Object: cr}
	}
	holdsOnReady := func(cr *unstructured.Unstructured) (bool, error) { return cr.GetName() == "ready", nil }

	for _, test := range []struct {
		name      string
		predicate target.ReadyFunc
		observed  []observe.Version
		want      bool
		wantErr   bool
	}{
		{name: "it holds on the one CR", predicate: holds, observed: []observe.Version{widget}, want: true},
		{name: "it fails on one of two CRs", predicate: fails, observed: []observe.Version{widget, widget}},
		{name: "it fails on the second of two CRs", predicate: holdsOnReady, observed: []observe.Version{named("ready"), named("unready")}},
		{name: "it cannot be evaluated", predicate: errs, observed: []observe.Version{widget}},
		{name: "no CR is left to be ready", predicate: fails, want: true},
		{name: "it yields no bool", predicate: yieldsAnInt, observed: []observe.Version{widget}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ready(test.predicate, test.observed)
			if got != test.want {
				t.Errorf("ready reported %t, want %t.", got, test.want)
			}
			if (err != nil) != test.wantErr {
				t.Errorf("ready returned the error %v, want one: %t.", err, test.wantErr)
			}
		})
	}
}
