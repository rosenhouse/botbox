package target_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/target"
)

func toyTargetPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "targets", "toy-widget", "target.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func loadToy(t *testing.T) *target.Target {
	t.Helper()
	toy, err := target.Load(toyTargetPath(t))
	if err != nil {
		t.Fatalf("Load rejected the toy target: %v", err)
	}
	return toy
}

func TestLoadToyWidget(t *testing.T) {
	toy := loadToy(t)
	dir := filepath.Dir(toyTargetPath(t))

	if toy.Name != "toy-widget" || toy.Version != "dev" {
		t.Errorf("Load read name %q and version %q.", toy.Name, toy.Version)
	}
	if want := []string{filepath.Join(dir, "crds")}; !reflect.DeepEqual(toy.CRDs, want) {
		t.Errorf("Load resolved crds to %v, want %v.", toy.CRDs, want)
	}
	if want := (schema.GroupVersionKind{Group: "toy.botbox", Version: "v1", Kind: "Widget"}); toy.Primary != want {
		t.Errorf("Load read primary %v, want %v.", toy.Primary, want)
	}
	if want := []schema.GroupVersionKind{{Version: "v1", Kind: "ConfigMap"}}; !reflect.DeepEqual(toy.Manages, want) {
		t.Errorf("Load read manages %v, want %v.", toy.Manages, want)
	}
	if toy.Sample.GroupVersionKind() != toy.Primary || toy.Sample.GetName() != "widget" {
		t.Errorf("Load read sample %v/%v.", toy.Sample.GroupVersionKind(), toy.Sample.GetName())
	}
	if count, found, err := unstructured.NestedInt64(toy.Sample.Object, "spec", "count"); count != 3 || !found || err != nil {
		t.Errorf("Load read sample spec.count as (%v, %v, %v), want 3.", count, found, err)
	}
	if len(toy.Fixtures) != 0 {
		t.Errorf("Load read %d fixtures from a target that declares none.", len(toy.Fixtures))
	}
	if toy.Selector != nil {
		t.Errorf("Load read selector %v from a target that declares none.", toy.Selector)
	}
	if toy.Equal != nil || len(toy.EqualIgnore) != 0 {
		t.Errorf("Load read an equality hook or equalIgnore from a target that declares neither.")
	}
	if len(toy.Generate.Mutate) != 0 || len(toy.Generate.Overlay) != 0 {
		t.Errorf("Load read generate %+v from a target that declares none.", toy.Generate)
	}
	wantLaunch := target.LaunchSpec{
		Binary: "bin/toy-widget",
		Args:   []string{"--kubeconfig=$KUBECONFIG", "--bug=0"},
		Env:    map[string]string{"WATCH_NAMESPACE": "$NAMESPACE"},
	}
	if !reflect.DeepEqual(toy.Launch, wantLaunch) {
		t.Errorf("Load read launch %+v, want %+v.", toy.Launch, wantLaunch)
	}
	wantTimeouts := target.Timeouts{Settle: 5 * time.Second, Stable: 2 * time.Second, Delete: 10 * time.Second}
	if toy.Timeouts != wantTimeouts {
		t.Errorf("Load read timeouts %+v, want %+v.", toy.Timeouts, wantTimeouts)
	}
	if want := (target.Thresholds{ErrLoop: 5}); toy.Thresholds != want {
		t.Errorf("Load read thresholds %+v, want %+v.", toy.Thresholds, want)
	}

	if len(toy.Properties) != 1 {
		t.Fatalf("Load read %d properties, want 1.", len(toy.Properties))
	}
	p1 := toy.Properties[0]
	if p1.ID != "P1" || p1.When != target.Checkpoint {
		t.Errorf("Load read property %q with when %q.", p1.ID, p1.When)
	}
	if !strings.Contains(p1.Description, "status.ready") {
		t.Errorf("Load read property description %q.", p1.Description)
	}
}

func widgetCR(generation, count int64, status map[string]any) *unstructured.Unstructured {
	cr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "toy.botbox/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": "widget", "generation": generation},
		"spec":       map[string]any{"count": count},
	}}
	if status != nil {
		cr.Object["status"] = status
	}
	return cr
}

// widgetBare is a Widget as the API server never returns one: no metadata, no
// spec and no status.
func widgetBare() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "toy.botbox/v1",
		"kind":       "Widget",
	}}
}

func configMap(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name},
		"data":       map[string]any{"index": "0"},
	}}
}

func TestToyReady(t *testing.T) {
	ready := loadToy(t).Ready

	for _, tc := range []struct {
		name string
		cr   *unstructured.Unstructured
		want bool
	}{
		{"converged", widgetCR(2, 3, map[string]any{"observedGeneration": int64(2), "ready": int64(3)}), true},
		{"converged on count 0", widgetCR(2, 0, map[string]any{"observedGeneration": int64(2), "ready": int64(0)}), true},
		{"stale generation", widgetCR(3, 3, map[string]any{"observedGeneration": int64(2), "ready": int64(3)}), false},
		{"fewer children than count", widgetCR(2, 3, map[string]any{"observedGeneration": int64(2), "ready": int64(1)}), false},
		{"no status at all", widgetCR(1, 3, nil), false},
		{"empty status", widgetCR(1, 3, map[string]any{}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ready(tc.cr)
			if err != nil {
				t.Fatalf("ready returned an error on a well-formed Widget: %v", err)
			}
			if got != tc.want {
				t.Errorf("ready returned %v, want %v.", got, tc.want)
			}
		})
	}
}

func TestToyPropertyP1(t *testing.T) {
	eval := loadToy(t).Properties[0].Eval

	for _, tc := range []struct {
		name    string
		cr      *unstructured.Unstructured
		managed []*unstructured.Unstructured
		want    bool
	}{
		{"status matches the children", widgetCR(2, 2, map[string]any{"ready": int64(2)}),
			[]*unstructured.Unstructured{configMap("widget-0"), configMap("widget-1")}, true},
		{"status ahead of the children", widgetCR(2, 2, map[string]any{"ready": int64(2)}),
			[]*unstructured.Unstructured{configMap("widget-0")}, false},
		{"other kinds do not count", widgetCR(2, 1, map[string]any{"ready": int64(1)}),
			[]*unstructured.Unstructured{widgetCR(2, 1, nil)}, false},
		{"no status yet", widgetCR(2, 1, nil), nil, true},
		{"no children yet", widgetCR(2, 1, map[string]any{"ready": int64(0)}), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := eval(tc.cr, tc.managed)
			if err != nil {
				t.Fatalf("P1 returned an error: %v", err)
			}
			if got != tc.want {
				t.Errorf("P1 returned %v, want %v.", got, tc.want)
			}
		})
	}
}

// writeTarget writes target.yaml plus the extra files, and returns the path of
// the target.yaml.
func writeTarget(t *testing.T, targetYAML string, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"target.yaml": targetYAML}
	for name, content := range extra {
		files[name] = content
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "target.yaml")
}

const sampleWidget = `apiVersion: toy.botbox/v1
kind: Widget
metadata:
  name: widget
spec:
  count: 3
`

// minimalTarget is a valid declaration that each error case breaks in one way.
const minimalTarget = `name: min
primary: toy.botbox/v1/Widget
sample: widget.yaml
launch: {binary: bin/min}
`

// minimalTargetWithEnv ends in an open launch.env block.
const minimalTargetWithEnv = `name: min
primary: toy.botbox/v1/Widget
sample: widget.yaml
launch:
  binary: bin/min
  env:
`

func TestLoadRejects(t *testing.T) {
	for _, tc := range []struct {
		name       string
		targetYAML string
		sample     string
		wants      []string
	}{
		{"unknown field", minimalTarget + "reday: 'true'\n", "", []string{"reday"}},
		{"unknown nested field", minimalTarget + "timeouts:\n  settel: 5s\n", "", []string{"settel"}},
		{"primary without a version", "name: min\nprimary: Widget\nsample: widget.yaml\nlaunch: {binary: bin/min}\n", "", []string{"primary", "Widget"}},
		{"primary group read as a version", "name: min\nprimary: apps/Deployment\nsample: widget.yaml\nlaunch: {binary: bin/min}\n", "", []string{"primary", "apps"}},
		{"managed kind with too many slashes", minimalTarget + "manages:\n  - a/b/c/d\n", "", []string{"manages", "a/b/c/d"}},
		{"managed kind with an empty segment", minimalTarget + "manages:\n  - /v1/ConfigMap\n", "", []string{"manages", "/v1/ConfigMap"}},
		{"ready that does not compile", minimalTarget + "ready: status.ready ==\n", "", []string{"ready"}},
		{"ready that yields a string", minimalTarget + "ready: '\"yes\"'\n", "", []string{"ready", "bool"}},
		{"ready over an unbound variable", minimalTarget + "ready: managed.size() > 0\n", "", []string{"ready", "managed"}},
		{"property that does not compile", minimalTarget + "properties:\n  - id: P1\n    cel: managed.filter(\n", "", []string{"P1"}},
		{"property that yields an int", minimalTarget + "properties:\n  - id: P1\n    cel: managed.size()\n", "", []string{"P1", "bool"}},
		{"property without an id", minimalTarget + "properties:\n  - cel: 'true'\n", "", []string{"id"}},
		{"property with an unknown when", minimalTarget + "properties:\n  - id: P1\n    cel: 'true'\n    when: sometimes\n", "", []string{"when", "sometimes"}},
		{"unknown ready hook", minimalTarget + "ready: 'go:nosuchhook'\n", "", []string{"nosuchhook"}},
		{"unknown equal hook", minimalTarget + "equal: 'go:nosuchhook'\n", "", []string{"nosuchhook"}},
		{"unparsable timeout", minimalTarget + "timeouts:\n  settle: soon\n", "", []string{"settle", "soon"}},
		{"unparsable selector", minimalTarget + "selector: 'app in'\n", "", []string{"selector"}},
		{"missing sample", "name: min\nprimary: toy.botbox/v1/Widget\nsample: nosuch.yaml\nlaunch: {binary: bin/min}\n", "", []string{"nosuch.yaml"}},
		{"missing fixture", minimalTarget + "fixtures:\n  - nosuch.yaml\n", "", []string{"nosuch.yaml"}},
		{"sample that is not an object", minimalTarget, "- 1\n- 2\n", []string{"widget.yaml"}},
		{"no name", "primary: toy.botbox/v1/Widget\nsample: widget.yaml\nlaunch: {binary: bin/min}\n", "", []string{"name"}},
		{"no primary", "name: min\nsample: widget.yaml\nlaunch: {binary: bin/min}\n", "", []string{"primary"}},
		{"no sample", "name: min\nprimary: toy.botbox/v1/Widget\nlaunch: {binary: bin/min}\n", "", []string{"sample"}},
		{"no launch binary", "name: min\nprimary: toy.botbox/v1/Widget\nsample: widget.yaml\n", "", []string{"launch.binary"}},
		{"sample of another kind", minimalTarget, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n", []string{"widget.yaml", "Secret", "Widget"}},
		{"sample holding two objects", minimalTarget, sampleWidget + "---\n" + sampleWidget, []string{"widget.yaml", "2 objects"}},
		{"sample holding no object", minimalTarget, "# just a comment\n", []string{"widget.yaml", "no object"}},
		{"sample that is not YAML", minimalTarget, "name: \"unterminated\n", []string{"widget.yaml"}},
		{"missing crds path", minimalTarget + "crds: [nosuch/]\n", "", []string{"crds", "nosuch"}},
		{"managed group read as a version", minimalTarget + "manages:\n  - apps/Deployment\n", "", []string{"manages", "apps"}},
		{"duplicate property id", minimalTarget + "properties:\n  - id: P1\n    cel: 'true'\n  - id: P1\n    cel: 'false'\n", "", []string{"P1", "twice"}},
		{"property without cel", minimalTarget + "properties:\n  - id: P1\n", "", []string{"P1", "cel"}},
		{"equal written as CEL", minimalTarget + "equal: 'a == b'\n", "", []string{"equal", "go:"}},
		{"an ignored list index", minimalTarget + "equalIgnore:\n  - status.conditions[0].message\n", "",
			[]string{"equalIgnore", "status.conditions[0].message", "offset 17", "[*]"}},
		{"an ignored annotation whose key the dots split", minimalTarget + "equalIgnore: [metadata.annotations.probe.example.com/started-at]\n", "",
			[]string{"equalIgnore", `metadata.annotations["probe.example.com/started-at"]`, "block-style list"}},
		{"timeout of zero", minimalTarget + "timeouts:\n  stable: 0s\n", "", []string{"stable", "positive"}},
		{"negative timeout", minimalTarget + "timeouts:\n  delete: -1s\n", "", []string{"delete", "positive"}},
		{"errloop of zero", minimalTarget + "thresholds:\n  errloop: 0\n", "", []string{"errloop", "positive"}},
		{"a launch env that sets the kubeconfig", minimalTargetWithEnv + "    KUBECONFIG: /elsewhere\n", "", []string{"launch.env", "KUBECONFIG"}},
		{"a launch env name holding an equals sign", minimalTargetWithEnv + "    A=B: x\n", "", []string{"launch.env", `"A=B"`}},
		{"an empty launch env name", minimalTargetWithEnv + "    '': x\n", "", []string{"launch.env", `""`}},
		// A settle wait carves T_stable of quiet out of T_settle, so these
		// leave the target no time to react and every op expires. The wants
		// carry the durations: the temp directory's path holds the case name,
		// so a bare "stable" would match whatever the loader said.
		{"stable as wide as settle", minimalTarget + "timeouts:\n  settle: 5s\n  stable: 5s\n", "", []string{"stable 5s", "settle 5s"}},
		{"stable wider than settle", minimalTarget + "timeouts:\n  settle: 5s\n  stable: 10s\n", "", []string{"stable 10s", "settle 5s"}},
		{"a stable the default settle cannot hold", minimalTarget + "timeouts:\n  stable: 40s\n", "", []string{"stable 40s", "settle 30s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sample := sampleWidget
			if tc.sample != "" {
				sample = tc.sample
			}
			path := writeTarget(t, tc.targetYAML, map[string]string{"widget.yaml": sample})

			loaded, err := target.Load(path)
			if err == nil {
				t.Fatalf("Load accepted %s; it returned %+v.", tc.name, loaded)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load reported %q, which does not name %q.", err, want)
				}
			}
		})
	}
}

func TestLoadKeepsAnAbsolutePath(t *testing.T) {
	sample := filepath.Join(t.TempDir(), "widget.yaml")
	if err := os.WriteFile(sample, []byte(sampleWidget), 0o644); err != nil {
		t.Fatal(err)
	}
	path := writeTarget(t, "name: min\nprimary: toy.botbox/v1/Widget\nlaunch: {binary: bin/min}\nsample: "+sample+"\n", nil)

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected an absolute sample path: %v", err)
	}
	if loaded.Sample.GetName() != "widget" {
		t.Errorf("Load read sample %q.", loaded.Sample.GetName())
	}
}

func TestLoadRejectsAMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "target.yaml")

	if _, err := target.Load(missing); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("Load reported %v for a target.yaml that does not exist.", err)
	}
}

// TestLoadReportsTheResolvedPath covers the mistake of writing a path relative
// to the repository root where target.yaml's own directory is the base.
func TestLoadReportsTheResolvedPath(t *testing.T) {
	path := writeTarget(t, "name: min\nprimary: toy.botbox/v1/Widget\nsample: targets/toy-widget/widget.yaml\n", nil)

	_, err := target.Load(path)
	if err == nil {
		t.Fatal("Load accepted a sample path that does not exist.")
	}
	if want := filepath.Join(filepath.Dir(path), "targets/toy-widget/widget.yaml"); !strings.Contains(err.Error(), want) {
		t.Errorf("Load reported %q, which does not name the path it looked in, %q.", err, want)
	}
}

// TestLoadResolvesPathsAgainstTheTargetDirectory pins the two different bases
// of DESIGN.md §8.1: files sit beside target.yaml, the binary sits under the
// repository root.
func TestLoadResolvesPathsAgainstTheTargetDirectory(t *testing.T) {
	path := writeTarget(t, `name: min
primary: toy.botbox/v1/Widget
crds: [crds/]
sample: widget.yaml
fixtures: [fixture.yaml]
launch:
  binary: bin/min
`, map[string]string{
		"widget.yaml":  sampleWidget,
		"fixture.yaml": "apiVersion: v1\nkind: Secret\nmetadata:\n  name: fixture\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: also-a-fixture\n",
		"crds/w.yaml":  "# the loader resolves the path without reading it\n",
	})
	dir := filepath.Dir(path)
	t.Chdir(t.TempDir())

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load failed from a working directory elsewhere: %v", err)
	}
	if want := []string{filepath.Join(dir, "crds")}; !reflect.DeepEqual(loaded.CRDs, want) {
		t.Errorf("Load resolved crds to %v, want %v.", loaded.CRDs, want)
	}
	// A fixture file holds as many objects as it declares.
	if len(loaded.Fixtures) != 2 || loaded.Fixtures[0].GetName() != "fixture" || loaded.Fixtures[1].GetName() != "also-a-fixture" {
		t.Errorf("Load read %d fixtures, want 2.", len(loaded.Fixtures))
	}
	if loaded.Launch.Binary != "bin/min" {
		t.Errorf("Load resolved launch.binary to %q; it is relative to the repository root.", loaded.Launch.Binary)
	}
}

func TestLoadLaunchEnv(t *testing.T) {
	path := writeTarget(t, minimalTargetWithEnv+"    WATCH_NAMESPACE: $NAMESPACE\n    EMPTY: ''\n",
		map[string]string{"widget.yaml": sampleWidget})

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected launch.env: %v", err)
	}
	if want := map[string]string{"WATCH_NAMESPACE": "$NAMESPACE", "EMPTY": ""}; !reflect.DeepEqual(loaded.Launch.Env, want) {
		t.Errorf("Load read launch.env %v, want %v.", loaded.Launch.Env, want)
	}
}

func TestLoadDefaults(t *testing.T) {
	path := writeTarget(t, minimalTarget, map[string]string{"widget.yaml": sampleWidget})

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected a minimal target: %v", err)
	}

	wantTimeouts := target.Timeouts{Settle: 30 * time.Second, Stable: 10 * time.Second, Delete: 60 * time.Second}
	if loaded.Timeouts != wantTimeouts {
		t.Errorf("Load defaulted timeouts to %+v, want %+v.", loaded.Timeouts, wantTimeouts)
	}
	if want := (target.Thresholds{ErrLoop: 20}); loaded.Thresholds != want {
		t.Errorf("Load defaulted thresholds to %+v, want %+v.", loaded.Thresholds, want)
	}

	// The default Ready predicate of DESIGN.md §6 ignores status.ready.
	for _, tc := range []struct {
		name string
		cr   *unstructured.Unstructured
		want bool
	}{
		{"observed", widgetCR(2, 3, map[string]any{"observedGeneration": int64(2)}), true},
		{"stale", widgetCR(3, 3, map[string]any{"observedGeneration": int64(2)}), false},
		{"unobserved", widgetCR(1, 3, nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loaded.Ready(tc.cr)
			if err != nil {
				t.Fatalf("the default ready predicate returned an error: %v", err)
			}
			if got != tc.want {
				t.Errorf("the default ready predicate returned %v, want %v.", got, tc.want)
			}
		})
	}
}

func TestLoadDefaultsEachTimeoutSeparately(t *testing.T) {
	path := writeTarget(t, minimalTarget+"timeouts:\n  stable: 1s\n  delete: 2s\nthresholds: {}\n", map[string]string{"widget.yaml": sampleWidget})

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected a partial timeouts block: %v", err)
	}
	want := target.Timeouts{Settle: 30 * time.Second, Stable: time.Second, Delete: 2 * time.Second}
	if loaded.Timeouts != want {
		t.Errorf("Load read timeouts %+v, want %+v.", loaded.Timeouts, want)
	}
	if loaded.Thresholds.ErrLoop != 20 {
		t.Errorf("Load read errloop %d, want the default 20.", loaded.Thresholds.ErrLoop)
	}
}

func TestLoadGenerateAndSelector(t *testing.T) {
	ignored := []string{
		"status.lastSyncTime",
		`metadata.annotations["probe.example.com/started-at"]`,
		"status.conditions[*].lastHeartbeatTime",
		`["top.level"].x`,
	}
	path := writeTarget(t, minimalTarget+`selector: app=widget,tier in (a,b)
equalIgnore:
  - `+ignored[0]+`
  - `+ignored[1]+`
  - `+ignored[2]+`
  - '`+ignored[3]+`'
generate:
  mutate: [spec.count]
  overlay:
    spec.count: {minimum: 1, maximum: 3}
`, map[string]string{"widget.yaml": sampleWidget})

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected generate, selector and equalIgnore: %v", err)
	}
	if loaded.Selector == nil || !loaded.Selector.Matches(labels.Set{"app": "widget", "tier": "a"}) {
		t.Errorf("Load read selector %v, which does not match app=widget,tier=a.", loaded.Selector)
	}
	var read []string
	for _, path := range loaded.EqualIgnore {
		read = append(read, path.String())
	}
	if !reflect.DeepEqual(read, ignored) {
		t.Errorf("Load read equalIgnore %q, want %q.", read, ignored)
	}
	if want := []string{"spec.count"}; !reflect.DeepEqual(loaded.Generate.Mutate, want) {
		t.Errorf("Load read generate.mutate %v, want %v.", loaded.Generate.Mutate, want)
	}
	overlay, err := json.Marshal(loaded.Generate.Overlay["spec.count"])
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"maximum":3,"minimum":1}`; string(overlay) != want {
		t.Errorf("Load read the overlay of spec.count as %s, want %s.", overlay, want)
	}
}
