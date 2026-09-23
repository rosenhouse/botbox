package generate

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"
)

func TestTheCRDJudgesWhatTheOverlayLoosens(t *testing.T) {
	loaded := loadTarget(t, toyTarget)
	loaded.Generate.Overlay = map[string]map[string]any{"spec.count": {"minimum": 11, "maximum": 20}}
	g := newGenerator(t, loaded, Options{})
	rapid.Check(t, func(rt *rapid.T) {
		if counts := generatedAt(g.sequence(rt), loaded.Sample, "spec", "count"); len(counts) > 0 {
			rt.Fatalf("spec.count took %v, which the CRD's maximum of 10 refuses.", counts)
		}
	})
}

func TestASampleItsCRDRefusesIsAConfigurationError(t *testing.T) {
	for _, testCase := range []struct {
		field    string
		value    any
		reported string
	}{
		{"maxUnavailable", int64(4), "maxUnavailable must not exceed count"},
		{"count", int64(11), "spec.count"},
		{"tags", []any{"a", "a"}, "spec.tags"},
	} {
		t.Run(testCase.field, func(t *testing.T) {
			loaded := loadTarget(t, rulesTarget)
			if err := unstructured.SetNestedField(loaded.Sample.Object, testCase.value, "spec", testCase.field); err != nil {
				t.Fatalf("Setting the sample's %s failed: %v.", testCase.field, err)
			}
			_, err := New(loaded, Options{})
			if err == nil {
				t.Fatalf("New accepted a sample whose spec.%s is %v, which its CRD refuses.", testCase.field, testCase.value)
			}
			for _, want := range []string{"sample", testCase.reported} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("New reported %q, which does not mention %q.", err, want)
				}
			}
		})
	}
}
