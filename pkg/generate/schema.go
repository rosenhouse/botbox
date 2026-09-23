package generate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"pgregory.net/rapid"
	"sigs.k8s.io/yaml"

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
	ListType    string             `json:"x-kubernetes-list-type"`
	IntOrString bool               `json:"x-kubernetes-int-or-string"`
	Items       *schema            `json:"items"`
	Properties  map[string]*schema `json:"properties"`
	Required    []string           `json:"required"`
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
		overlay := t.Generate.Overlay[dotted]
		if unread := unreadKeywords(overlay, ""); len(unread) > 0 {
			return nil, fmt.Errorf("generate.overlay %s: botbox does not read %s; it reads %s",
				dotted, strings.Join(unread, ", "), strings.Join(keywords, ", "))
		}
		node, err := schemaNode(root, strings.Split(dotted, "."))
		if err != nil {
			return nil, fmt.Errorf("generate.overlay %s: %w", dotted, err)
		}
		mergeInto(node, overlay)
	}
	return asSchema(root)
}

// openAPISchema finds the primary CR's openAPIV3Schema among the target's CRDs.
func openAPISchema(t *target.Target) (map[string]any, error) {
	documents, err := crdDocuments(t.CRDs)
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

// crdDocuments reads every CRD manifest the paths name. A path is a file or a
// directory of them, as the cluster reads them (DESIGN.md §8.1).
func crdDocuments(paths []string) ([]map[string]any, error) {
	var documents []map[string]any
	for _, path := range paths {
		files, err := manifests(path)
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			read, err := readDocuments(file)
			if err != nil {
				return nil, err
			}
			documents = append(documents, read...)
		}
	}
	if len(documents) == 0 {
		return nil, fmt.Errorf("the paths %v hold no CRD", paths)
	}
	return documents, nil
}

// manifestExtensions are the files a CRD directory holds.
var manifestExtensions = []string{".yaml", ".yml", ".json"}

func manifests(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading the CRDs: %w", err)
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("reading the CRDs: %w", err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && slices.Contains(manifestExtensions, filepath.Ext(entry.Name())) {
			files = append(files, filepath.Join(path, entry.Name()))
		}
	}
	return files, nil
}

func readDocuments(path string) ([]map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the CRDs: %w", err)
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var documents []map[string]any
	for {
		document, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return documents, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading the CRDs in %s: %w", path, err)
		}
		var decoded map[string]any
		if err := yaml.Unmarshal(document, &decoded); err != nil {
			return nil, fmt.Errorf("reading the CRDs in %s: %w", path, err)
		}
		if decoded != nil {
			documents = append(documents, decoded)
		}
	}
}

// keywords are the schema keywords the generator reads.
var keywords = func() []string {
	var read []string
	fields := reflect.TypeFor[schema]()
	for i := range fields.NumField() {
		read = append(read, fields.Field(i).Tag.Get("json"))
	}
	slices.Sort(read)
	return read
}()

// unreadKeywords are the keywords in an overlay that the generator ignores,
// as dotted paths inside it.
func unreadKeywords(overlay map[string]any, prefix string) []string {
	var unread []string
	for _, key := range slices.Sorted(maps.Keys(overlay)) {
		nested, _ := overlay[key].(map[string]any)
		switch {
		case !slices.Contains(keywords, key):
			unread = append(unread, prefix+key)
		case key == "items":
			unread = append(unread, unreadKeywords(nested, prefix+key+".")...)
		case key == "properties":
			for _, name := range slices.Sorted(maps.Keys(nested)) {
				property, _ := nested[name].(map[string]any)
				unread = append(unread, unreadKeywords(property, prefix+key+"."+name+".")...)
			}
		}
	}
	return unread
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
