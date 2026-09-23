package generate

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"

	"github.com/rosenhouse/botbox/pkg/target"
)

func TestTheCRDJudgesWhatTheOverlayLoosens(t *testing.T) {
	loaded := loadTarget(t, toyTarget)
	loaded.Generate.Overlay = map[string]map[string]any{"spec.count": {"minimum": 5, "maximum": 20}}
	g := newGenerator(t, loaded, Options{})
	drawn := 0
	rapid.Check(t, func(rt *rapid.T) {
		for _, count := range generatedAt(g.sequence(rt), loaded.Sample, "spec", "count") {
			if count.(int64) > 10 {
				rt.Fatalf("spec.count took %v, which the CRD's maximum of 10 refuses.", count)
			}
			drawn++
		}
	})
	if drawn == 0 {
		t.Error("No draw changed spec.count.")
	}
}

func TestAMutatePathTheCRDRefusesInTheSampleIsAConfigurationError(t *testing.T) {
	for _, testCase := range []struct {
		path, mutate string
		overlay      map[string]map[string]any
		reported     string
	}{
		{toyTarget, "spec.count", map[string]map[string]any{"spec.count": {"minimum": 11, "maximum": 20}},
			"less than or equal to 10"},
		{rulesTarget, "spec.right", nil, "exactly one of left and right must be set"},
	} {
		t.Run(testCase.mutate, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			loaded.Generate = target.GenerateSpec{Mutate: []string{testCase.mutate}, Overlay: testCase.overlay}
			_, err := New(loaded, Options{})
			if err == nil {
				t.Fatalf("New accepted generate.mutate %s, which no drawn value moves.", testCase.mutate)
			}
			for _, want := range []string{"generate.mutate " + testCase.mutate, testCase.reported} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("New reported %q, which does not mention %q.", err, want)
				}
			}
		})
	}
}

func TestNewDrawsAFieldAHundredTimesToFindAValueTheCRDAccepts(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	g := newGenerator(t, loaded, Options{})
	for _, testCase := range []struct {
		count    int64
		draws    int
		accepted bool
	}{
		{5, 1, true},
		// No count reaches minCount's default.
		{0, 100, false},
	} {
		draws := 0
		err := g.rules.acceptsADraw(loaded.Sample, countsOf(testCase.count, &draws)[0])
		if (err == nil) != testCase.accepted || draws != testCase.draws {
			t.Errorf("With every count %d, New drew %d times and returned %v, want %d draws and accepted=%t.",
				testCase.count, draws, err, testCase.draws, testCase.accepted)
		}
	}
}

func TestATransitionRuleSeesTheOldObjectsDefaults(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	crd, err := openAPISchema(loaded)
	if err != nil {
		t.Fatalf("openAPISchema failed: %v.", err)
	}
	rules, err := newCRDRules(crd)
	if err != nil {
		t.Fatalf("newCRDRules failed: %v.", err)
	}
	for _, testCase := range []struct {
		field   string
		value   int64
		refused bool
	}{
		{"count", 5, false},
		{"minCount", 2, true},
	} {
		t.Run(testCase.field, func(t *testing.T) {
			updated := loaded.Sample.DeepCopy()
			if err := unstructured.SetNestedField(updated.Object, testCase.value, "spec", testCase.field); err != nil {
				t.Fatalf("Setting spec.%s failed: %v.", testCase.field, err)
			}
			if err := rules.refusal(updated.Object, loaded.Sample.Object); (err != nil) != testCase.refused {
				t.Errorf("Setting spec.%s to %d on a gadget without minCount returned %v, want refused=%t.",
					testCase.field, testCase.value, err, testCase.refused)
			}
		})
	}
}

func TestAFieldTheSampleCannotHoldIsRefused(t *testing.T) {
	g := newGenerator(t, loadTarget(t, rulesTarget), Options{})
	draws := 0
	inside := countsOf(5, &draws)[0]
	inside.path = []string{"spec", "count", "inside"}
	if err := g.rules.acceptsADraw(g.target.Sample, inside); err == nil {
		t.Error("New drew a value inside the sample's spec.count, which holds an integer.")
	}
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
