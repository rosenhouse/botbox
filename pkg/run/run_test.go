package run

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
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
	"k8s.io/client-go/kubernetes"
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
			name:   "a cluster with a controller manager and no config",
			target: toy,
			opts:   Options{Dir: "out", ControllerManager: true},
			want:   "controller manager",
		},
		{
			name:   "a cluster with a controller manager and a config",
			target: toy,
			opts:   Options{Dir: "out", ControllerManager: true, Config: &rest.Config{}},
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

var serviceAccountKind = schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}

func inRunNamespace(gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace("botbox-run-1")
	u.SetName(name)
	return u
}

func TestAwaitNamespaceDefaults(t *testing.T) {
	serviceAccount := inRunNamespace(serviceAccountKind, "default")
	rootCA := inRunNamespace(configMapKind, "kube-root-ca.crt")
	both := []schema.GroupVersionKind{widgetKind, serviceAccountKind, configMapKind}
	for _, test := range []struct {
		name    string
		watched []schema.GroupVersionKind
		made    []*unstructured.Unstructured
		// later is what the cluster makes while the wait sleeps.
		later  []*unstructured.Unstructured
		within time.Duration
		want   string
	}{
		{name: "both made", watched: both, made: []*unstructured.Unstructured{serviceAccount, rootCA}},
		{
			name: "one made while it waits", watched: both, within: time.Minute,
			made: []*unstructured.Unstructured{serviceAccount}, later: []*unstructured.Unstructured{rootCA},
		},
		{
			name: "the ConfigMap never made", watched: both,
			made: []*unstructured.Unstructured{serviceAccount}, want: "v1/ConfigMap kube-root-ca.crt",
		},
		{
			name: "the ServiceAccount never made", watched: both,
			made: []*unstructured.Unstructured{rootCA}, want: "v1/ServiceAccount default",
		},
		{
			name: "an unwatched kind never made", watched: []schema.GroupVersionKind{widgetKind, configMapKind},
			made: []*unstructured.Unstructured{rootCA},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := observe.NewStore(observe.Options{Namespace: "botbox-run-1"})
			for _, made := range test.made {
				store.Record(made.GroupVersionKind(), made, time.Now())
			}
			sleep := func(context.Context, time.Duration) error {
				for _, made := range test.later {
					store.Record(made.GroupVersionKind(), made, time.Now())
				}
				return nil
			}

			err := awaitNamespaceDefaults(t.Context(), store, test.watched, test.within, sleep)

			switch {
			case test.want == "" && err != nil:
				t.Errorf("awaitNamespaceDefaults returned %v, want nil.", err)
			case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
				t.Errorf("awaitNamespaceDefaults returned %v, want an error naming %q.", err, test.want)
			}
		})
	}
}

func TestExcludePresentLeavesWhatComesLaterManaged(t *testing.T) {
	store := observe.NewStore(observe.Options{Namespace: "botbox-run-1", Manages: []schema.GroupVersionKind{configMapKind}})
	store.Record(configMapKind, inRunNamespace(configMapKind, "kube-root-ca.crt"), time.Now())

	excludePresent(store, []schema.GroupVersionKind{widgetKind, configMapKind})
	store.Record(configMapKind, inRunNamespace(configMapKind, "widget-0"), time.Now())

	var managed []string
	for _, v := range store.Managed() {
		managed = append(managed, v.Name)
	}
	if !slices.Equal(managed, []string{"widget-0"}) {
		t.Errorf("The managed objects are %v, want only widget-0.", managed)
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

// A settle wait ends at the first reading of a ready that yields no bool,
// rather than waiting out settle on it.
func TestTheHarnessReadsAReadyThatYieldsNoBoolAsAnError(t *testing.T) {
	store := observe.NewStore(observe.Options{Namespace: "botbox-run-x"})
	store.Record(widgetKind, widget("widget"), time.Now())
	yieldsAnInt := &target.Target{Primary: widgetKind, Ready: func(*unstructured.Unstructured) (bool, error) {
		return false, &target.EvalError{Predicate: "ready", Expr: "status.ready", Err: fmt.Errorf("%w: it yielded int64", target.ErrNotBool)}
	}}
	h := &Harness{target: yieldsAnInt, Observer: &observe.Observer{Store: store}, Launcher: launcherReporting{status: launch.Status{Running: true}}}

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
			ready, _, err := harnessOver(test.status).state(since)

			if err != nil || ready != test.ready {
				t.Errorf("A target %s reads as ready: %t (%v), want %t.", test.name, ready, err, test.ready)
			}
		})
	}
}

// A restarted target has to hold still for T_stable too, or a wait converges
// before the new process has done anything.
func TestARestartCountsAsAChange(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	restarted := since.Add(time.Second)

	_, changed, err := harnessOver(launch.Status{Running: true, Started: restarted}).state(since)

	if err != nil || !changed.Equal(restarted) {
		t.Errorf("The run last changed at %v (%v), want the restart at %v.", changed, err, restarted)
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

	live.supervise(t.Context())

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

// An interrupt has ended the run's context, and an API server may never answer.
func TestTheRunNamespaceIsDeletedAfterAnInterruptWithinABound(t *testing.T) {
	requests := make(chan string, 10)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	core, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	deleted := make(chan error, 1)
	go func() { deleted <- deleteNamespace(ended, core, "botbox-run", 100*time.Millisecond) }()

	select {
	case err := <-deleted:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Deleting the namespace returned %v, want it to give up.", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Deleting the namespace still waited on the API server after 10s.")
	}
	select {
	case got := <-requests:
		if want := "DELETE /api/v1/namespaces/botbox-run"; got != want {
			t.Errorf("The API server got %q, want %q.", got, want)
		}
	default:
		t.Error("The API server got no request to delete the namespace.")
	}
}

// The API server serves no Widget, so Start fails once it has created the
// namespace, and takes the namespace back as Stop would.
func TestTheRunNamespaceIsDeletedOnTheBudgetBoundGivesIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api":
			fmt.Fprint(w, `{"kind":"APIVersions","versions":["v1"]}`)
		case "GET /apis":
			fmt.Fprint(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`)
		case "GET /api/v1":
			fmt.Fprint(w, `{"kind":"APIResourceList","groupVersion":"v1","resources":[]}`)
		case "POST /api/v1/namespaces":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"botbox-run"}}`)
		default:
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
		}
	}))
	t.Cleanup(server.Close)
	budgets := make(chan time.Duration, 10)
	config := &rest.Config{Host: server.URL, WrapTransport: func(next http.RoundTripper) http.RoundTripper {
		return roundTripper(func(r *http.Request) (*http.Response, error) {
			if deadline, ok := r.Context().Deadline(); ok && r.Method == http.MethodDelete {
				budgets <- time.Until(deadline)
			}
			return next.RoundTrip(r)
		})
	}}

	h, err := Start(t.Context(), toyTarget, Options{Dir: t.TempDir(), Config: config})

	if h != nil || err == nil {
		t.Fatalf("Start returned (%v, %v), want an error: the API server serves no Widget.", h, err)
	}
	select {
	case budget := <-budgets:
		if want := stopBudget - launch.StopWithin; budget > want || budget < want-time.Second {
			t.Errorf("Deleting the namespace had %v, want %v.", budget, want)
		}
	default:
		t.Error("The API server got no request to delete the namespace on a budget.")
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The launcher supervises the target under the run's context, so a target that
// exits after an interrupt is not restarted.
func TestTheHarnessSupervisesUnderTheContextItIsGiven(t *testing.T) {
	dir := t.TempDir()
	binary := launch.NewBinary(launch.Options{Path: "/bin/sh", Args: []string{"-c", "exit 3"}})
	t.Cleanup(func() { _ = binary.Stop(context.Background()) })
	live := &liveRun{h: &Harness{dir: dir, Launcher: binary}}
	if err := binary.Start(t.Context(), "kubeconfig"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	live.supervise(ctx)

	select {
	case <-binary.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("The target was still supervised after its context ended.")
	}
	if exits := live.exits(); len(exits) != 0 {
		t.Errorf("The harness recorded the exits %+v, and supervision had ended.", exits)
	}
}
