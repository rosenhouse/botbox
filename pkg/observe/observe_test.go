package observe_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse/botbox/pkg/observe"
)

// unreachable points at nothing, so these tests fail unless Start validates
// before it touches the API server.
func unreachable() *rest.Config { return &rest.Config{Host: "http://127.0.0.1:1"} }

func requireStartRejects(t *testing.T, opts observe.Options, reason string) {
	t.Helper()
	o, err := observe.Start(unreachable(), opts)
	if err == nil {
		o.Stop()
		t.Fatalf("Start accepted %+v.", opts)
	}
	if o != nil {
		t.Error("Start returned an Observer together with an error.")
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("Start returned %q, which does not mention %q.", err, reason)
	}
}

func TestStartRejectsOptionsWithoutANamespace(t *testing.T) {
	requireStartRejects(t, observe.Options{Manages: []schema.GroupVersionKind{configMapGVK}}, "namespace")
}

func TestStartRejectsOptionsWithNoKindToWatch(t *testing.T) {
	requireStartRejects(t, observe.Options{Namespace: namespace}, "kind")
}
