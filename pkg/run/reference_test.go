package run

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse/botbox/internal/reference"
	"github.com/rosenhouse/botbox/pkg/proxy"
)

const (
	opsHeading        = "### Ops"
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
			documented := reference.Keys(t, test.heading)

			for _, field := range test.fields {
				if !slices.Contains(documented, field) {
					t.Errorf("%s has no row for %s.", reference.Path, field)
				}
			}
			for _, row := range documented {
				if !slices.Contains(test.fields, row) {
					t.Errorf("%s has a row for %s, which is no such field.", reference.Path, row)
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

	if documented := reference.Keys(t, opsHeading); !slices.Equal(documented, want) {
		t.Errorf("%s lists the ops %v, want %v.", reference.Path, documented, want)
	}
}

func TestTheReferenceSaysWhatEachOpTakesAndWhetherItSettles(t *testing.T) {
	for _, opType := range opTypes {
		row := reference.Row(t, opsHeading, string(opType))
		var needs, mayCarry []string
		for field := range maps.Keys(fieldsOf(opType)) {
			if field == "until" {
				field = "until.op"
			}
			needs = append(needs, field)
		}
		if slices.Contains(namingOps, opType) {
			mayCarry = append(mayCarry, "cr")
		}
		if opType.OnCR() {
			mayCarry = append(mayCarry, "noSettle")
		}
		settles := map[bool]string{true: "yes", false: "no"}[Op{Type: opType}.Settles()]

		if got := sorted(reference.Spans(row[0])); !slices.Equal(got, sorted(needs)) {
			t.Errorf("The reference says %s needs %v, want %v.", opType, got, sorted(needs))
		}
		if got := sorted(reference.Spans(row[1])); !slices.Equal(got, sorted(mayCarry)) {
			t.Errorf("The reference says %s may carry %v, want %v.", opType, got, sorted(mayCarry))
		}
		if row[2] != settles {
			t.Errorf("The reference says whether %s settles: %s, want %s.", opType, row[2], settles)
		}
	}
}

func sorted(fields []string) []string {
	return slices.Sorted(slices.Values(fields))
}

func TestTheReferenceListsEveryVerb(t *testing.T) {
	documented := reference.Spans(reference.Row(t, "### Fault fields", "match.verb")[1])

	if !slices.Equal(documented, proxy.Verbs) {
		t.Errorf("The row for match.verb names %v, want %v.", documented, proxy.Verbs)
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
