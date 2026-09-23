package generate

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse/botbox/pkg/target"
)

const (
	toyTarget         = "../../targets/toy-widget/target.yaml"
	certManagerTarget = "../../examples/cert-manager/target.yaml"
	rulesTarget       = "testdata/rules/target.yaml"
)

func loadTarget(t *testing.T, path string) *target.Target {
	t.Helper()
	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load(%s) failed: %v.", path, err)
	}
	return loaded
}

func primarySchemaOf(t *testing.T, path string) *schema {
	t.Helper()
	read, err := primarySchema(loadTarget(t, path))
	if err != nil {
		t.Fatalf("primarySchema(%s) failed: %v.", path, err)
	}
	return read
}

// crdSchemaOf reads the primary CR's schema as the CRD alone states it, which
// is what the API server enforces.
func crdSchemaOf(t *testing.T, path string) *schema {
	t.Helper()
	loaded := loadTarget(t, path)
	loaded.Generate.Overlay = nil
	read, err := primarySchema(loaded)
	if err != nil {
		t.Fatalf("primarySchema(%s) failed: %v.", path, err)
	}
	return read
}

func TestSchemaBoundsTheToysCount(t *testing.T) {
	count := primarySchemaOf(t, toyTarget).Properties["spec"].Properties["count"]
	if count == nil {
		t.Fatal("The toy's schema describes no spec.count.")
	}
	if count.Type != "integer" {
		t.Errorf("spec.count has type %q, want integer.", count.Type)
	}
	if count.Minimum == nil || *count.Minimum != 0 {
		t.Errorf("spec.count has minimum %v, want 0.", count.Minimum)
	}
	if count.Maximum == nil || *count.Maximum != 10 {
		t.Errorf("spec.count has maximum %v, want 10.", count.Maximum)
	}
}

func TestSchemaReadsCertManagersCertificate(t *testing.T) {
	spec := primarySchemaOf(t, certManagerTarget).Properties["spec"]
	if want := []string{"issuerRef", "secretName"}; !reflect.DeepEqual(spec.Required, want) {
		t.Errorf("spec requires %v, want %v.", spec.Required, want)
	}
	dnsNames := spec.Properties["dnsNames"]
	if dnsNames.Type != "array" || dnsNames.Items == nil || dnsNames.Items.Type != "string" {
		t.Errorf("spec.dnsNames is %+v, want an array of string.", dnsNames)
	}
	algorithm := spec.Properties["privateKey"].Properties["algorithm"]
	if want := []any{"RSA", "ECDSA", "Ed25519"}; !reflect.DeepEqual(algorithm.Enum, want) {
		t.Errorf("spec.privateKey.algorithm has enum %v, want %v.", algorithm.Enum, want)
	}
}

func TestOverlayWinsOverTheSchema(t *testing.T) {
	loaded := loadTarget(t, certManagerTarget)
	// The expectations come from the target's own overlay, so that tightening
	// it does not turn this test red for saying so.
	overlay := loaded.Generate.Overlay
	read, err := primarySchema(loaded)
	if err != nil {
		t.Fatalf("primarySchema(%s) failed: %v.", certManagerTarget, err)
	}
	spec := read.Properties["spec"]

	dnsNames := spec.Properties["dnsNames"]
	wanted := overlay["spec.dnsNames"]
	if dnsNames.MinItems == nil || float64(*dnsNames.MinItems) != wanted["minItems"] {
		t.Errorf("spec.dnsNames has minItems %v, want the overlay's %v.", dnsNames.MinItems, wanted["minItems"])
	}
	if dnsNames.MaxItems == nil || float64(*dnsNames.MaxItems) != wanted["maxItems"] {
		t.Errorf("spec.dnsNames has maxItems %v, want the overlay's %v.", dnsNames.MaxItems, wanted["maxItems"])
	}
	items, ok := wanted["items"].(map[string]any)
	if !ok {
		t.Fatalf("The overlay's spec.dnsNames carries items %v, want an object.", wanted["items"])
	}
	if want := items["pattern"]; dnsNames.Items.Pattern != want {
		t.Errorf("spec.dnsNames items have pattern %q, want the overlay's %q.", dnsNames.Items.Pattern, want)
	}
	if dnsNames.Items.Type != "string" {
		t.Errorf("spec.dnsNames items have type %q; the overlay tightens the CRD's schema and must not erase it.",
			dnsNames.Items.Type)
	}

	duration := spec.Properties["duration"]
	if want := overlay["spec.duration"]["enum"]; !reflect.DeepEqual(duration.Enum, want) {
		t.Errorf("spec.duration has enum %v, want the overlay's %v.", duration.Enum, want)
	}
}

func TestMutateRestrictsThePathsThatMove(t *testing.T) {
	for _, testCase := range []struct {
		path  string
		paths []string
	}{
		{toyTarget, []string{"spec.count"}},
		{certManagerTarget, []string{
			"spec.dnsNames", "spec.duration", "spec.privateKey.algorithm", "spec.privateKey.rotationPolicy",
		}},
	} {
		t.Run(testCase.path, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			fields, err := mutableFields(loaded, primarySchemaOf(t, testCase.path))
			if err != nil {
				t.Fatalf("mutableFields failed: %v.", err)
			}
			var paths []string
			for _, field := range fields {
				paths = append(paths, field.dotted)
			}
			if !reflect.DeepEqual(paths, testCase.paths) {
				t.Errorf("The generator mutates %v, want %v.", paths, testCase.paths)
			}
		})
	}
}

func TestPathsTheSchemaDoesNotDescribeAreConfigurationErrors(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		declare func(*target.GenerateSpec)
	}{
		{"mutate", func(g *target.GenerateSpec) { g.Mutate = []string{"spec.nonesuch"} }},
		{"overlay", func(g *target.GenerateSpec) {
			g.Overlay = map[string]map[string]any{"spec.nonesuch": {"maximum": 3}}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			loaded := loadTarget(t, toyTarget)
			testCase.declare(&loaded.Generate)
			_, err := New(loaded, Options{})
			if err == nil {
				t.Fatalf("New accepted generate.%s naming a path the schema does not describe.", testCase.name)
			}
			if !strings.Contains(err.Error(), "spec.nonesuch") {
				t.Errorf("New reported %q, which does not name the path.", err)
			}
		})
	}
}

func TestAnOverlayKeywordBotboxDoesNotReadIsAConfigurationError(t *testing.T) {
	for _, testCase := range []struct {
		path, dotted string
		overlay      map[string]any
		unread       string
	}{
		{toyTarget, "spec.count", map[string]any{"maximun": 0}, "maximun"},
		{certManagerTarget, "spec.dnsNames", map[string]any{"items": map[string]any{"patern": "^a$"}}, "items.patern"},
		{certManagerTarget, "spec.privateKey",
			map[string]any{"properties": map[string]any{"algorithm": map[string]any{"enumm": []any{"RSA"}}}},
			"properties.algorithm.enumm"},
	} {
		t.Run(testCase.unread, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			loaded.Generate.Overlay = map[string]map[string]any{testCase.dotted: testCase.overlay}
			_, err := New(loaded, Options{})
			if err == nil {
				t.Fatalf("New accepted the overlay %v, whose %s botbox does not read.", testCase.overlay, testCase.unread)
			}
			for _, want := range []string{testCase.dotted, testCase.unread, "maximum", "pattern"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("New reported %q, which does not mention %q.", err, want)
				}
			}
		})
	}
}

func TestMutateIsAnAllowlistOfSchemaPaths(t *testing.T) {
	loaded := loadTarget(t, certManagerTarget)
	loaded.Generate.Mutate = []string{"spec.privateKey.algorithm"}
	fields, err := mutableFields(loaded, primarySchemaOf(t, certManagerTarget))
	if err != nil {
		t.Fatalf("mutableFields failed: %v.", err)
	}
	if len(fields) != 1 || fields[0].dotted != "spec.privateKey.algorithm" {
		t.Fatalf("The allowlist of one path yielded %d fields, want spec.privateKey.algorithm alone.", len(fields))
	}
	if !slices.Equal(fields[0].path, []string{"spec", "privateKey", "algorithm"}) {
		t.Errorf("The field's path is %v, want it split on the dots.", fields[0].path)
	}
}
