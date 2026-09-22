package target_test

import (
	"slices"
	"testing"

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
