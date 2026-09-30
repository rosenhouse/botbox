// Package reference reads the tables of docs/reference.md, and the keys a Go
// type takes, for the tests that hold the page to the code.
package reference

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Path is docs/reference.md.
var Path = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "docs", "reference.md")
}()

// Listed lists the key in the first cell of each row of the table under
// heading.
func Listed(t testing.TB, heading string) []string {
	t.Helper()
	var keys []string
	for _, row := range rows(t, heading) {
		keys = append(keys, row[0])
	}
	return keys
}

// Row is the cells after the first of the row under heading for key.
func Row(t testing.TB, heading, key string) []string {
	t.Helper()
	for _, row := range rows(t, heading) {
		if row[0] == key {
			return row[1:]
		}
	}
	t.Fatalf("%s has no row for %s under %q.", Path, key, heading)
	return nil
}

// Spans lists the code spans in a cell.
func Spans(cell string) []string {
	var spans []string
	for i, part := range strings.Split(cell, "`") {
		if i%2 == 1 {
			spans = append(spans, part)
		}
	}
	return spans
}

// rows are the cells of each row under heading, up to the next heading, whose
// first cell is a code span. That cell is given without its backquotes.
func rows(t testing.TB, heading string) [][]string {
	t.Helper()
	page, err := os.ReadFile(Path)
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(page), "\n"+heading+"\n")
	if !found {
		t.Fatalf("%s has no heading %q.", Path, heading)
	}
	var rows [][]string
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
			break
		}
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		cells[0] = strings.Trim(cells[0], "`")
		rows = append(rows, cells)
	}
	return rows
}

// Keys lists the JSON keys a type takes, dotted, with [*] for each item of a
// list or value of a map. A struct is a block of keys and no key itself,
// unless it is one of leaves or marshals itself.
func Keys(declared reflect.Type, leaves ...reflect.Type) []string {
	return keys(declared, "", leaves)
}

func keys(declared reflect.Type, prefix string, leaves []reflect.Type) []string {
	var found []string
	for field := range declared.Fields() {
		key := prefix + jsonName(field)
		switch object, items := block(field.Type, leaves); {
		case object != nil:
			found = append(found, keys(object, key+".", leaves)...)
		case items != nil:
			found = append(found, key)
			found = append(found, keys(items, key+"[*].", leaves)...)
		default:
			found = append(found, key)
		}
	}
	return found
}

// SetKeys lists the keys Keys names that a value sets, in any item.
func SetKeys(value reflect.Value, leaves ...reflect.Type) []string {
	set := setKeys(value, "", leaves)
	slices.Sort(set)
	return slices.Compact(set)
}

func setKeys(value reflect.Value, prefix string, leaves []reflect.Type) []string {
	var found []string
	for i, field := range slices.Collect(value.Type().Fields()) {
		key, set := prefix+jsonName(field), value.Field(i)
		if set.IsZero() || (set.Kind() == reflect.Slice || set.Kind() == reflect.Map) && set.Len() == 0 {
			continue
		}
		switch object, items := block(field.Type, leaves); {
		case object != nil:
			found = append(found, setKeys(reflect.Indirect(set), key+".", leaves)...)
		case items != nil:
			found = append(found, key)
			for _, item := range set.Seq2() {
				found = append(found, setKeys(item, key+"[*].", leaves)...)
			}
		default:
			found = append(found, key)
		}
	}
	return found
}

// block is the struct a field holds, directly or through a pointer, or else
// the struct each item of a list or a map field holds.
func block(field reflect.Type, leaves []reflect.Type) (object, items reflect.Type) {
	isBlock := func(held reflect.Type) bool {
		marshaler := reflect.TypeFor[json.Marshaler]()
		return held.Kind() == reflect.Struct && !slices.Contains(leaves, held) &&
			!held.Implements(marshaler) && !reflect.PointerTo(held).Implements(marshaler)
	}
	switch kind := field.Kind(); {
	case isBlock(field):
		return field, nil
	case kind == reflect.Pointer && isBlock(field.Elem()):
		return field.Elem(), nil
	case (kind == reflect.Slice || kind == reflect.Map) && isBlock(field.Elem()):
		return nil, field.Elem()
	}
	return nil, nil
}

func jsonName(field reflect.StructField) string {
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	return name
}
