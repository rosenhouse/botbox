package invariant

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/rosenhouse/botbox/pkg/target"
)

// maxValue bounds a quoted value, in runes.
const maxValue = 80

// change is one field at which two objects differ. A side that lacks the
// field has no value.
type change struct {
	path          target.Path
	before, after any
	had, has      bool
}

// diff appends every field below path at which a and b differ, in the paths
// equalIgnore takes. It quotes the values of shownA and shownB, which hold a
// and b as a report may show them.
func diff(path target.Path, a, b, shownA, shownB any, changes []change) []change {
	if reflect.DeepEqual(a, b) {
		return changes
	}
	found := len(changes)
	aMap, aIsMap := a.(map[string]any)
	bMap, bIsMap := b.(map[string]any)
	aList, aIsList := a.([]any)
	bList, bIsList := b.([]any)
	switch {
	case len(path) > 0 && path[:len(path)-1].MapsToStrings():
		// A label or an annotation is one value.
	case aIsMap && bIsMap:
		for _, key := range sortedKeys(aMap, bMap) {
			field := append(slices.Clip(path), target.Step{Key: key})
			before, had := aMap[key]
			after, has := bMap[key]
			if had && has {
				changes = diff(field, before, after, below(shownA, key), below(shownB, key), changes)
				continue
			}
			changes = append(changes, change{field, below(shownA, key), below(shownB, key), had, has})
		}
	case aIsList && bIsList && len(aList) == len(bList):
		each := append(slices.Clip(path), target.Step{Each: true})
		for i := range aList {
			changes = diff(each, aList[i], bList[i], item(shownA, i), item(shownB, i), changes)
		}
	}
	if len(changes) == found { // No field below differs, so the value does.
		changes = append(changes, change{path, shownA, shownB, true, true})
	}
	return changes
}

func sortedKeys(a, b map[string]any) []string {
	both := maps.Clone(a)
	maps.Copy(both, b)
	return slices.Sorted(maps.Keys(both))
}

// below is the field a shown form holds at key. A form that redacted the
// whole field shows its marker for everything below it.
func below(shown any, key string) any {
	if fields, ok := shown.(map[string]any); ok {
		return fields[key]
	}
	return shown
}

func item(shown any, i int) any {
	if items, ok := shown.([]any); ok {
		return items[i]
	}
	return shown
}

func (c change) difference(object Difference) Difference {
	object.Path = c.path.String()
	object.Before, object.After = quote(c.before, c.had), quote(c.after, c.has)
	return object
}

// quote writes a value as compact JSON, cut at maxValue runes.
func quote(value any, present bool) string {
	if !present {
		return "(absent)"
	}
	var encoded strings.Builder
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value) // A comparable form holds only JSON values.
	text := []rune(strings.TrimSuffix(encoded.String(), "\n"))
	if len(text) > maxValue {
		return string(text[:maxValue]) + "…"
	}
	return string(text)
}
