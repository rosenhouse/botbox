package target

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// declaration mirrors target.yaml (DESIGN.md §8.1). Decoding is strict, so a
// misspelled key is a configuration error rather than silence.
type declaration struct {
	Name         string                `json:"name"`
	Version      string                `json:"version"`
	CRDs         []string              `json:"crds"`
	Primary      string                `json:"primary"`
	Sample       string                `json:"sample"`
	Fixtures     []string              `json:"fixtures"`
	Manages      []string              `json:"manages"`
	NotRecreated []string              `json:"notRecreated"`
	Selector     string                `json:"selector"`
	Ready        string                `json:"ready"`
	Equal        string                `json:"equal"`
	EqualIgnore  []string              `json:"equalIgnore"`
	Properties   []propertyDeclaration `json:"properties"`
	Generate     generateDeclaration   `json:"generate"`
	Launch       LaunchSpec            `json:"launch"`
	Timeouts     timeoutsDeclaration   `json:"timeouts"`
	Thresholds   thresholdsDeclaration `json:"thresholds"`
}

type propertyDeclaration struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	CEL         string `json:"cel"`
	When        string `json:"when"`
}

type generateDeclaration struct {
	Mutate  []string                  `json:"mutate"`
	Overlay map[string]map[string]any `json:"overlay"`
}

type timeoutsDeclaration struct {
	Settle string `json:"settle"`
	Stable string `json:"stable"`
	Delete string `json:"delete"`
}

type thresholdsDeclaration struct {
	ErrLoop *int `json:"errloop"`
}

// Load reads target.yaml at path. Paths inside it resolve against the file's
// own directory, except launch.binary, which resolves against the repository
// root (DESIGN.md §8.1).
func Load(path string) (*Target, error) {
	loaded, err := load(path)
	if err != nil {
		return nil, fmt.Errorf("loading target %s: %w", path, err)
	}
	return loaded, nil
}

func load(path string) (*Target, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var declared declaration
	if err := yaml.UnmarshalStrict(data, &declared); err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)

	loaded := &Target{
		Name:     declared.Name,
		Version:  declared.Version,
		Generate: GenerateSpec(declared.Generate),
		Launch:   declared.Launch,
	}
	if loaded.Name == "" {
		return nil, errors.New("name is required")
	}
	for _, crd := range declared.CRDs {
		crdPath := resolve(dir, crd)
		if _, err := os.Stat(crdPath); err != nil {
			return nil, fmt.Errorf("crds: %w", err)
		}
		loaded.CRDs = append(loaded.CRDs, crdPath)
	}

	if declared.Primary == "" {
		return nil, errors.New("primary is required")
	}
	if loaded.Primary, err = parseGVK(declared.Primary); err != nil {
		return nil, fmt.Errorf("primary %q: %w", declared.Primary, err)
	}
	for _, managed := range declared.Manages {
		gvk, err := parseGVK(managed)
		if err != nil {
			return nil, fmt.Errorf("manages %q: %w", managed, err)
		}
		loaded.Manages = append(loaded.Manages, gvk)
	}
	for _, kind := range declared.NotRecreated {
		gvk, err := parseGVK(kind)
		if err != nil {
			return nil, fmt.Errorf("notRecreated %q: %w", kind, err)
		}
		if !slices.Contains(loaded.Manages, gvk) {
			return nil, fmt.Errorf("notRecreated %q: the target does not list it under manages", kind)
		}
		loaded.NotRecreated = append(loaded.NotRecreated, gvk)
	}

	if declared.Sample == "" {
		return nil, errors.New("sample is required")
	}
	samplePath := resolve(dir, declared.Sample)
	sample, err := loadObjects(samplePath)
	if err != nil {
		return nil, fmt.Errorf("sample: %w", err)
	}
	if len(sample) != 1 {
		return nil, fmt.Errorf("sample %s: holds %d objects, want the primary CR alone", samplePath, len(sample))
	}
	loaded.Sample = sample[0]
	if gvk := loaded.Sample.GroupVersionKind(); gvk != loaded.Primary {
		return nil, fmt.Errorf("sample %s: holds %s, not the primary %s", samplePath, gvk, loaded.Primary)
	}
	for _, fixture := range declared.Fixtures {
		objects, err := loadObjects(resolve(dir, fixture))
		if err != nil {
			return nil, fmt.Errorf("fixture: %w", err)
		}
		loaded.Fixtures = append(loaded.Fixtures, objects...)
	}

	if declared.Selector != "" {
		if loaded.Selector, err = labels.Parse(declared.Selector); err != nil {
			return nil, fmt.Errorf("selector %q: %w", declared.Selector, err)
		}
	}

	if declared.Ready == "" {
		declared.Ready = DefaultReady
	}
	loaded.ReadyExpr = declared.Ready
	if loaded.Ready, err = readyPredicate(declared.Ready); err != nil {
		return nil, fmt.Errorf("ready: %w", err)
	}
	if name, isHook := hookName(declared.Equal); isHook {
		if loaded.Equal, err = equalHook(name); err != nil {
			return nil, fmt.Errorf("equal: %w", err)
		}
	} else if declared.Equal != "" {
		return nil, fmt.Errorf("equal %q: equality is not CEL; it takes a go:<name> hook", declared.Equal)
	}
	for _, text := range declared.EqualIgnore {
		path, err := ParsePath(text)
		if err != nil {
			return nil, fmt.Errorf("equalIgnore %q: %w", text, err)
		}
		loaded.EqualIgnore = append(loaded.EqualIgnore, path)
	}
	if loaded.Equal != nil && len(loaded.EqualIgnore) > 0 {
		return nil, errors.New("equalIgnore does nothing beside an equal hook, which replaces the default equality")
	}

	seen := map[string]bool{}
	for _, declaredProperty := range declared.Properties {
		property, err := loadProperty(declaredProperty)
		if err != nil {
			return nil, err
		}
		if seen[property.ID] {
			return nil, fmt.Errorf("property %s: the id is declared twice", property.ID)
		}
		seen[property.ID] = true
		loaded.Properties = append(loaded.Properties, property)
	}

	if loaded.Launch.Binary == "" {
		return nil, errors.New("launch.binary is required")
	}

	if loaded.Timeouts, err = timeouts(declared.Timeouts); err != nil {
		return nil, err
	}
	if loaded.Thresholds, err = thresholds(declared.Thresholds); err != nil {
		return nil, err
	}
	return loaded, nil
}

func readyPredicate(declared string) (ReadyFunc, error) {
	if name, isHook := hookName(declared); isHook {
		return readyHook(name)
	}
	return compileReady(declared)
}

func loadProperty(declared propertyDeclaration) (Property, error) {
	if declared.ID == "" {
		return Property{}, errors.New("property: id is required")
	}
	when := PropertyWhen(declared.When)
	switch when {
	case "":
		when = Checkpoint
	case Always, Checkpoint, End:
	default:
		return Property{}, fmt.Errorf("property %s: when %q is not one of %s, %s, %s",
			declared.ID, declared.When, Always, Checkpoint, End)
	}
	if declared.CEL == "" {
		return Property{}, fmt.Errorf("property %s: cel is required", declared.ID)
	}
	eval, err := compileProperty(declared.ID, declared.CEL)
	if err != nil {
		return Property{}, fmt.Errorf("property %s: %w", declared.ID, err)
	}
	return Property{ID: declared.ID, Description: declared.Description, Eval: eval, When: when}, nil
}

func timeouts(declared timeoutsDeclaration) (Timeouts, error) {
	parsed := DefaultTimeouts
	var err error
	if parsed.Settle, err = duration("settle", declared.Settle, parsed.Settle); err != nil {
		return Timeouts{}, err
	}
	if parsed.Stable, err = duration("stable", declared.Stable, parsed.Stable); err != nil {
		return Timeouts{}, err
	}
	if parsed.Delete, err = duration("delete", declared.Delete, parsed.Delete); err != nil {
		return Timeouts{}, err
	}
	// A settle wait ends once the Ready predicate holds and nothing has
	// changed for stable, within settle (DESIGN.md §5.5). Where stable is the
	// wider of the two, no wait can end that way, and every run reports G4
	// against a target that did nothing wrong.
	if parsed.Stable >= parsed.Settle {
		return Timeouts{}, fmt.Errorf("timeouts: stable %s is not shorter than settle %s, and a settle wait has to observe stable of quiet inside settle",
			parsed.Stable, parsed.Settle)
	}
	return parsed, nil
}

func duration(field, declared string, fallback time.Duration) (time.Duration, error) {
	if declared == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(declared)
	if err != nil {
		return 0, fmt.Errorf("timeouts: %s %q: %w", field, declared, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("timeouts: %s %q must be positive", field, declared)
	}
	return parsed, nil
}

func thresholds(declared thresholdsDeclaration) (Thresholds, error) {
	parsed := DefaultThresholds
	if declared.ErrLoop != nil {
		parsed.ErrLoop = *declared.ErrLoop
	}
	if parsed.ErrLoop <= 0 {
		return Thresholds{}, fmt.Errorf("thresholds: errloop %d must be positive", parsed.ErrLoop)
	}
	return parsed, nil
}

// resolve makes path relative to dir, the directory holding target.yaml.
func resolve(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

// apiVersion matches the version names Kubernetes accepts, so that a
// group/Kind typo does not read as a core-group version.
var apiVersion = regexp.MustCompile(`^v[1-9][0-9]*((alpha|beta)[1-9][0-9]*)?$`)

// parseGVK reads group/version/Kind, or version/Kind for the core group
// (DESIGN.md §8.1).
func parseGVK(declared string) (schema.GroupVersionKind, error) {
	malformed := errors.New("want group/version/Kind or version/Kind")
	parts := strings.Split(declared, "/")
	var gvk schema.GroupVersionKind
	switch len(parts) {
	case 2:
		gvk = schema.GroupVersionKind{Version: parts[0], Kind: parts[1]}
	case 3:
		gvk = schema.GroupVersionKind{Group: parts[0], Version: parts[1], Kind: parts[2]}
	default:
		return schema.GroupVersionKind{}, malformed
	}
	if gvk.Kind == "" || (len(parts) == 3 && gvk.Group == "") {
		return schema.GroupVersionKind{}, malformed
	}
	if !apiVersion.MatchString(gvk.Version) {
		return schema.GroupVersionKind{}, fmt.Errorf("%q is not an API version", gvk.Version)
	}
	return gvk, nil
}

// loadObjects reads every document of a YAML file.
func loadObjects(path string) ([]*unstructured.Unstructured, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	documents := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var objects []*unstructured.Unstructured
	for {
		document, err := documents.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		asJSON, err := yaml.YAMLToJSON(document)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if string(asJSON) == "null" { // a document holding only comments
			continue
		}
		object := &unstructured.Unstructured{}
		if err := object.UnmarshalJSON(asJSON); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		objects = append(objects, object)
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("%s: holds no object", path)
	}
	return objects, nil
}
