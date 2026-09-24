package generate

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"

	"github.com/rosenhouse/botbox/pkg/target"
)

// schema is the part of a CRD's OpenAPI v3 schema the generator reads
// (DESIGN.md §5.4). What it leaves out, it cannot generate from.
type schema struct {
	Type             string   `json:"type"`
	Format           string   `json:"format"`
	Enum             []any    `json:"enum"`
	Minimum          *float64 `json:"minimum"`
	Maximum          *float64 `json:"maximum"`
	ExclusiveMinimum bool     `json:"exclusiveMinimum"`
	ExclusiveMaximum bool     `json:"exclusiveMaximum"`
	MinLength        *int64   `json:"minLength"`
	MaxLength        *int64   `json:"maxLength"`
	Pattern          string   `json:"pattern"`
	MinItems         *int64   `json:"minItems"`
	MaxItems         *int64   `json:"maxItems"`
	// ListType is x-kubernetes-list-type: a set holds no duplicate item, and a
	// map holds no duplicate key.
	ListType   string             `json:"x-kubernetes-list-type"`
	Items      *schema            `json:"items"`
	Properties map[string]*schema `json:"properties"`
	Required   []string           `json:"required"`
}

// field is one path the generator may change, and the values it may take.
type field struct {
	path   []string
	dotted string
	values *rapid.Generator[any]
	// optional says the schema does not require the field, so a mutation may
	// remove it.
	optional bool
}

// primarySchema reads the schema of the target's primary CR from its CRDs,
// with generate.overlay applied (DESIGN.md §8.3).
func primarySchema(t *target.Target) (*schema, error) {
	root, err := openAPISchema(t)
	if err != nil {
		return nil, err
	}
	for _, dotted := range slices.Sorted(maps.Keys(t.Generate.Overlay)) {
		node, err := schemaNode(root, strings.Split(dotted, "."))
		if err != nil {
			return nil, fmt.Errorf("generate.overlay %s: %w", dotted, err)
		}
		mergeInto(node, t.Generate.Overlay[dotted])
	}
	return asSchema(root)
}

// openAPISchema finds the primary CR's openAPIV3Schema among the target's CRDs.
func openAPISchema(t *target.Target) (map[string]any, error) {
	documents, err := target.ReadCRDs(t.CRDs)
	if err != nil {
		return nil, err
	}
	for _, document := range documents {
		spec, _ := document["spec"].(map[string]any)
		names, _ := spec["names"].(map[string]any)
		if spec["group"] != t.Primary.Group || names["kind"] != t.Primary.Kind {
			continue
		}
		versions, _ := spec["versions"].([]any)
		for _, entry := range versions {
			version, _ := entry.(map[string]any)
			if version["name"] != t.Primary.Version {
				continue
			}
			node, _ := version["schema"].(map[string]any)
			root, _ := node["openAPIV3Schema"].(map[string]any)
			if root == nil {
				return nil, fmt.Errorf("the CRD of %s carries no schema for %s",
					t.Primary.Kind, t.Primary.Version)
			}
			return root, nil
		}
	}
	return nil, fmt.Errorf("the target's CRDs describe no %s/%s %s",
		t.Primary.Group, t.Primary.Version, t.Primary.Kind)
}

// schemaNode walks a dotted path into a schema's properties (DESIGN.md §8.1).
func schemaNode(root map[string]any, path []string) (map[string]any, error) {
	node := root
	for i, segment := range path {
		properties, _ := node["properties"].(map[string]any)
		next, _ := properties[segment].(map[string]any)
		if next == nil {
			return nil, fmt.Errorf("the schema describes no %s", strings.Join(path[:i+1], "."))
		}
		node = next
	}
	return node, nil
}

// mergeInto merges the overlay into the schema node. The overlay wins wherever
// the two disagree, and keeps what it does not mention (DESIGN.md §8.3).
func mergeInto(node, overlay map[string]any) {
	for key, value := range overlay {
		tightening, isObject := value.(map[string]any)
		into, alsoObject := node[key].(map[string]any)
		if isObject && alsoObject {
			mergeInto(into, tightening)
			continue
		}
		node[key] = value
	}
}

// asSchema reads the raw schema into the part of it the generator uses.
func asSchema(root map[string]any) (*schema, error) {
	data, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("reading the schema: %w", err)
	}
	var read schema
	if err := json.Unmarshal(data, &read); err != nil {
		return nil, fmt.Errorf("reading the schema: %w", err)
	}
	return &read, nil
}

// mutableFields are the paths the generator may change: the allowlist in
// generate.mutate, or every path under spec where it is absent (DESIGN.md
// §5.4).
func mutableFields(t *target.Target, s *schema) ([]field, error) {
	if len(t.Generate.Mutate) == 0 {
		return specFields(s, t.Sample.Object), nil
	}
	var fields []field
	for _, dotted := range slices.Compact(slices.Sorted(slices.Values(t.Generate.Mutate))) {
		mutable, err := mutableField(s, dotted)
		if err != nil {
			return nil, fmt.Errorf("generate.mutate %s: %w", dotted, err)
		}
		fields = append(fields, mutable)
	}
	return fields, nil
}

// mutableField reads the schema at a dotted path.
func mutableField(s *schema, dotted string) (field, error) {
	path := strings.Split(dotted, ".")
	at, optional := s, false
	for i, segment := range path {
		next := at.Properties[segment]
		if next == nil {
			return field{}, fmt.Errorf("the schema describes no %s", strings.Join(path[:i+1], "."))
		}
		optional = !slices.Contains(at.Required, segment)
		at = next
	}
	values, err := valuesOf(at)
	if err != nil {
		return field{}, err
	}
	return field{path: path, dotted: dotted, values: values, optional: optional}, nil
}

// specFields are the paths under spec that the schema says enough about to
// generate values for, in path order. Only spec moves: apiVersion, kind and
// metadata are not the target's input, and status is a subresource a CR op
// cannot write. A path the schema says too little about is left alone rather
// than guessed at.
func specFields(s *schema, sample map[string]any) []field {
	spec := s.Properties["spec"]
	if spec == nil {
		return nil
	}
	return walker{sample}.walk(spec, []string{"spec"}, !slices.Contains(s.Required, "spec"))
}

// walker reads the target's sample, which says which objects a field may be
// set inside.
type walker struct{ sample map[string]any }

// walk descends into an object the sample carries and into one that requires
// no property of its own. It takes any other object whole, so that a field is
// never set inside an object the sample lacks and whose required properties
// would then be missing.
func (w walker) walk(s *schema, path []string, optional bool) []field {
	if s.Type == "object" && len(s.Properties) > 0 && (len(s.Required) == 0 || w.carries(path)) {
		var fields []field
		for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
			child := append(slices.Clone(path), name)
			fields = append(fields, w.walk(s.Properties[name], child, !slices.Contains(s.Required, name))...)
		}
		return fields
	}
	values, err := valuesOf(s)
	if err != nil {
		return nil
	}
	return []field{{path: path, dotted: strings.Join(path, "."), values: values, optional: optional}}
}

func (w walker) carries(path []string) bool {
	value, found, err := unstructured.NestedFieldNoCopy(w.sample, path...)
	if err != nil || !found {
		return false
	}
	_, isObject := value.(map[string]any)
	return isObject
}
