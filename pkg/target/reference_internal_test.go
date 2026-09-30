package target

import (
	"os"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/rosenhouse/botbox/internal/reference"
)

const (
	keysHeading      = "## target.yaml"
	referenceExample = "../../docs/reference/target.yaml"
)

func TestTheReferenceListsEveryKey(t *testing.T) {
	keys := declaredKeys(reflect.TypeFor[declaration](), "")

	documented := reference.Keys(t, keysHeading)

	for _, key := range keys {
		if !slices.Contains(documented, key) {
			t.Errorf("%s has no row for the key %s.", reference.Path, key)
		}
	}
	for _, row := range documented {
		if !slices.Contains(keys, row) {
			t.Errorf("%s has a row for %s, which target.yaml does not take.", reference.Path, row)
		}
	}
}

func TestTheReferenceGivesEachDefault(t *testing.T) {
	for key, want := range map[string]time.Duration{
		"timeouts.settle": DefaultTimeouts.Settle,
		"timeouts.stable": DefaultTimeouts.Stable,
		"timeouts.delete": DefaultTimeouts.Delete,
	} {
		if got, err := time.ParseDuration(documentedDefault(t, key)); err != nil || got != want {
			t.Errorf("The reference gives %s the default %q, want %s.", key, documentedDefault(t, key), want)
		}
	}
	for key, want := range map[string]string{
		"ready":              DefaultReady,
		"properties[*].when": string(Checkpoint),
		"thresholds.errloop": strconv.Itoa(DefaultThresholds.ErrLoop),
		"thresholds.quiet":   strconv.Itoa(DefaultThresholds.Quiet),
	} {
		if got := documentedDefault(t, key); got != want {
			t.Errorf("The reference gives %s the default %q, want %q.", key, got, want)
		}
	}
}

// documentedDefault is the code span in the Default cell of key's row.
func documentedDefault(t *testing.T, key string) string {
	t.Helper()
	spans := reference.Spans(reference.Row(t, keysHeading, key)[0])
	if len(spans) != 1 {
		t.Fatalf("The Default cell of %s holds %d code spans, want 1.", key, len(spans))
	}
	return spans[0]
}

// equal replaces what equalIgnore feeds, and the loader refuses the two
// together.
func TestTheReferenceExampleSetsEveryKeyButEqual(t *testing.T) {
	data, err := os.ReadFile(referenceExample)
	if err != nil {
		t.Fatal(err)
	}
	var declared declaration
	if err := yaml.UnmarshalStrict(data, &declared); err != nil {
		t.Fatalf("%s does not decode: %v", referenceExample, err)
	}

	if unset := unsetKeys(reflect.ValueOf(declared), ""); !slices.Equal(unset, []string{"equal"}) {
		t.Errorf("%s leaves %v unset, want equal alone.", referenceExample, unset)
	}
	if _, err := Load(referenceExample); err != nil {
		t.Errorf("Load rejected the example: %v", err)
	}
}

// declaredKeys lists the keys a declaration type takes, dotted, with [*] for
// each item of a list or value of a map. A block of keys is not a key itself.
func declaredKeys(declared reflect.Type, prefix string) []string {
	var keys []string
	for field := range declared.Fields() {
		key := prefix + jsonName(field)
		switch element := itemStruct(field.Type); {
		case field.Type.Kind() == reflect.Struct:
			keys = append(keys, declaredKeys(field.Type, key+".")...)
		case element != nil:
			keys = append(keys, key)
			keys = append(keys, declaredKeys(element, key+"[*].")...)
		default:
			keys = append(keys, key)
		}
	}
	return keys
}

// unsetKeys lists the keys declaredKeys names that a declaration leaves unset,
// in any item of a list or value of a map.
func unsetKeys(declared reflect.Value, prefix string) []string {
	var unset []string
	for i, field := range slices.Collect(declared.Type().Fields()) {
		key, value := prefix+jsonName(field), declared.Field(i)
		switch {
		case value.Kind() == reflect.Struct:
			unset = append(unset, unsetKeys(value, key+".")...)
		case value.IsZero() || (value.Kind() == reflect.Slice || value.Kind() == reflect.Map) && value.Len() == 0:
			unset = append(unset, key)
		case itemStruct(field.Type) != nil:
			for _, item := range value.Seq2() {
				unset = append(unset, unsetKeys(item, key+"[*].")...)
			}
		}
	}
	slices.Sort(unset)
	return slices.Compact(unset)
}

// itemStruct is the struct a list or a map holds, or nil.
func itemStruct(declared reflect.Type) reflect.Type {
	if kind := declared.Kind(); (kind == reflect.Slice || kind == reflect.Map) && declared.Elem().Kind() == reflect.Struct {
		return declared.Elem()
	}
	return nil
}
