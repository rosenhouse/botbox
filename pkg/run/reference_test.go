package run

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse/botbox/pkg/proxy"
)

const (
	reference         = "../../docs/reference.md"
	referenceSequence = "../../docs/reference/sequence.json"
)

func TestTheReferenceListsEveryField(t *testing.T) {
	for _, test := range []struct {
		heading string
		fields  []string
	}{
		{"### Sequence fields", fieldKeys(reflect.TypeFor[Sequence](), "", reflect.TypeFor[Op]())},
		{"### Op fields", fieldKeys(reflect.TypeFor[Op](), "", reflect.TypeFor[Fault]())},
		{"### Fault fields", fieldKeys(reflect.TypeFor[Fault](), "", nil)},
	} {
		t.Run(test.heading, func(t *testing.T) {
			documented := referenceRows(t, test.heading)

			for _, field := range test.fields {
				if !slices.Contains(documented, field) {
					t.Errorf("%s has no row for %s.", reference, field)
				}
			}
			for _, row := range documented {
				if !slices.Contains(test.fields, row) {
					t.Errorf("%s has a row for %s, which is no such field.", reference, row)
				}
			}
		})
	}
}

func TestTheReferenceListsEveryOpType(t *testing.T) {
	var want []string
	for _, opType := range opTypes {
		want = append(want, string(opType))
	}

	if documented := referenceRows(t, "### Ops"); !slices.Equal(documented, want) {
		t.Errorf("%s lists the ops %v, want %v.", reference, documented, want)
	}
}

func TestTheReferenceListsEveryVerb(t *testing.T) {
	row := referenceRow(t, "match.verb")

	for _, verb := range proxy.Verbs {
		if !strings.Contains(row, "`"+verb+"`") {
			t.Errorf("The row for match.verb does not name %s: %s", verb, row)
		}
	}
}

func TestTheReferenceSequenceHoldsEveryOpAndSetsEveryField(t *testing.T) {
	sequence, err := ReadSequence(referenceSequence)
	if err != nil {
		t.Fatal(err)
	}
	held, opFields, faultFields := map[OpType]bool{}, map[string]bool{}, map[string]bool{}
	for _, op := range sequence.Ops {
		held[op.Type] = true
		for _, field := range setFieldKeys(reflect.ValueOf(op), "", reflect.TypeFor[Fault]()) {
			opFields[field] = true
		}
		if op.Fault != nil {
			for _, field := range setFieldKeys(reflect.ValueOf(*op.Fault), "", nil) {
				faultFields[field] = true
			}
		}
	}

	for _, opType := range opTypes {
		if !held[opType] {
			t.Errorf("%s holds no %s op.", referenceSequence, opType)
		}
	}
	for _, field := range fieldKeys(reflect.TypeFor[Op](), "", reflect.TypeFor[Fault]()) {
		if !opFields[field] {
			t.Errorf("No op in %s sets %s.", referenceSequence, field)
		}
	}
	for _, field := range fieldKeys(reflect.TypeFor[Fault](), "", nil) {
		if !faultFields[field] {
			t.Errorf("No fault in %s sets %s.", referenceSequence, field)
		}
	}
}

// fieldKeys lists the JSON fields a type takes, dotted. A nested object is not
// a field itself. The fields of stop, and of a type that marshals itself, are
// not listed.
func fieldKeys(declared reflect.Type, prefix string, stop reflect.Type) []string {
	var keys []string
	for field := range declared.Fields() {
		key := prefix + jsonField(field)
		if nested := nestedObject(field.Type, stop); nested != nil {
			keys = append(keys, fieldKeys(nested, key+".", stop)...)
			continue
		}
		keys = append(keys, key)
	}
	return keys
}

// setFieldKeys lists the fields fieldKeys names that a value sets.
func setFieldKeys(value reflect.Value, prefix string, stop reflect.Type) []string {
	var keys []string
	for i, field := range slices.Collect(value.Type().Fields()) {
		key, set := prefix+jsonField(field), value.Field(i)
		switch nested := nestedObject(field.Type, stop); {
		case set.IsZero():
		case nested != nil:
			keys = append(keys, setFieldKeys(reflect.Indirect(set), key+".", stop)...)
		default:
			keys = append(keys, key)
		}
	}
	return keys
}

// nestedObject is the struct a field holds, directly or through a pointer,
// unless it is stop or marshals itself. It is nil otherwise.
func nestedObject(field, stop reflect.Type) reflect.Type {
	if field.Kind() == reflect.Pointer {
		field = field.Elem()
	}
	marshaler := reflect.TypeFor[json.Marshaler]()
	if field.Kind() != reflect.Struct || field == stop || field.Implements(marshaler) || reflect.PointerTo(field).Implements(marshaler) {
		return nil
	}
	return field
}

func jsonField(field reflect.StructField) string {
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	return name
}

// referenceRows lists the first code span of each table row in the section of
// the reference that heading opens.
func referenceRows(t *testing.T, heading string) []string {
	t.Helper()
	_, section, found := strings.Cut(readReference(t), "\n"+heading+"\n")
	if !found {
		t.Fatalf("%s has no heading %q.", reference, heading)
	}
	var rows []string
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
			break
		}
		if cell, isRow := strings.CutPrefix(line, "| `"); isRow {
			key, _, _ := strings.Cut(cell, "`")
			rows = append(rows, key)
		}
	}
	return rows
}

// referenceRow is the table row whose first cell is key.
func referenceRow(t *testing.T, key string) string {
	t.Helper()
	for _, line := range strings.Split(readReference(t), "\n") {
		if strings.HasPrefix(line, "| `"+key+"` |") {
			return line
		}
	}
	t.Fatalf("%s has no row for %s.", reference, key)
	return ""
}

func readReference(t *testing.T) string {
	t.Helper()
	doc, err := os.ReadFile(reference)
	if err != nil {
		t.Fatal(err)
	}
	return string(doc)
}
