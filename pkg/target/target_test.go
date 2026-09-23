package target_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/target"
)

func TestWatchedKindsHoldThePrimaryOnce(t *testing.T) {
	widget := schema.GroupVersionKind{Group: "toy.botbox", Version: "v1", Kind: "Widget"}
	configMap := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

	watched := (&target.Target{
		Primary: widget,
		Manages: []schema.GroupVersionKind{configMap, widget},
	}).WatchedKinds()

	if want := []schema.GroupVersionKind{widget, configMap}; !slices.Equal(watched, want) {
		t.Errorf("botbox watches %v, want %v.", watched, want)
	}
}

func TestCheckScopesNamesEveryClusterScopedKind(t *testing.T) {
	widget := schema.GroupVersionKind{Group: "toy.botbox", Version: "v1", Kind: "Widget"}
	clusterRole := schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}
	webhook := schema.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingWebhookConfiguration"}
	unserved := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Unserved"}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(widget, meta.RESTScopeNamespace)
	mapper.Add(clusterRole, meta.RESTScopeRoot)
	mapper.Add(webhook, meta.RESTScopeRoot)
	fixture := &unstructured.Unstructured{}
	fixture.SetGroupVersionKind(webhook)
	fixture.SetName("widget-validator")

	err := (&target.Target{
		Primary:  widget,
		Manages:  []schema.GroupVersionKind{clusterRole, unserved},
		Fixtures: []*unstructured.Unstructured{fixture},
	}).CheckScopes(mapper)

	if err == nil {
		t.Fatal("CheckScopes accepted cluster-scoped kinds.")
	}
	for _, want := range []string{"managed rbac.authorization.k8s.io/v1/ClusterRole",
		"fixture admissionregistration.k8s.io/v1/ValidatingWebhookConfiguration widget-validator"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CheckScopes returned %q, which does not name %q.", err, want)
		}
	}
	for _, other := range []string{"Widget", "Unserved"} {
		if strings.Contains(err.Error(), other) {
			t.Errorf("CheckScopes returned %q, which names %s.", err, other)
		}
	}
	if err := (&target.Target{Primary: widget}).CheckScopes(mapper); err != nil {
		t.Errorf("CheckScopes refused a namespaced primary: %v", err)
	}
}

func TestLaunchCheckFindsTheBinaryWhereBotboxRuns(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for name, mode := range map[string]os.FileMode{"operator": 0o755, "notes": 0o644} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		binary string
		want   []string
		// relative is whether the error names the working directory.
		relative bool
	}{
		{binary: "./operator"},
		{binary: filepath.Join(dir, "operator")},
		{binary: "sh"},
		{binary: "bin/operator", want: []string{"launch.binary", "bin/operator", "working directory " + dir, "target.yaml"}, relative: true},
		{binary: "./notes", want: []string{"launch.binary", "./notes", "permission denied"}, relative: true},
		{binary: "no-such-program-on-path", want: []string{"launch.binary", "no-such-program-on-path", "PATH"}},
		{binary: filepath.Join(dir, "no-such-operator"), want: []string{"launch.binary", filepath.Join(dir, "no-such-operator")}},
	} {
		t.Run(test.binary, func(t *testing.T) {
			err := target.LaunchSpec{Binary: test.binary}.Check()

			if len(test.want) == 0 {
				if err != nil {
					t.Errorf("Check refused %s: %v", test.binary, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Check accepted %s.", test.binary)
			}
			for _, said := range test.want {
				if !strings.Contains(err.Error(), said) {
					t.Errorf("Check returned %q, which does not say %q.", err, said)
				}
			}
			if named := strings.Contains(err.Error(), "working directory"); named != test.relative {
				t.Errorf("Check returned %q; want it to name the working directory only for a relative path.", err)
			}
		})
	}
}
