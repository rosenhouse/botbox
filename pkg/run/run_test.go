package run

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/target"
)

var (
	widgetKind    = schema.GroupVersionKind{Group: "toy.botbox", Version: "v1", Kind: "Widget"}
	configMapKind = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
)

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

func TestWatchedKindsHoldThePrimaryOnce(t *testing.T) {
	watched := watchedKinds(&target.Target{
		Primary: widgetKind,
		Manages: []schema.GroupVersionKind{configMapKind, widgetKind},
	})

	if want := []schema.GroupVersionKind{widgetKind, configMapKind}; !slices.Equal(watched, want) {
		t.Errorf("The harness watches %v, want %v.", watched, want)
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
