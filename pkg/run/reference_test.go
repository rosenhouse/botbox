package run

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/rosenhouse/botbox/internal/reference"
	"github.com/rosenhouse/botbox/pkg/proxy"
)

const (
	opsHeading        = "### Ops"
	referenceSequence = "../../docs/reference/sequence.json"
)

var (
	sequenceKeys = reference.Keys(reflect.TypeFor[Sequence](), reflect.TypeFor[Op]())
	opKeys       = reference.Keys(reflect.TypeFor[Op](), reflect.TypeFor[Fault]())
	faultKeys    = reference.Keys(reflect.TypeFor[Fault]())
)

func TestTheReferenceListsEveryField(t *testing.T) {
	for heading, fields := range map[string][]string{
		"### Sequence fields": sequenceKeys,
		"### Op fields":       opKeys,
		"### Fault fields":    faultKeys,
	} {
		t.Run(heading, func(t *testing.T) {
			documented := reference.Listed(t, heading)

			for _, field := range fields {
				if !slices.Contains(documented, field) {
					t.Errorf("%s has no row for %s.", reference.Path, field)
				}
			}
			for _, row := range documented {
				if !slices.Contains(fields, row) {
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

	if documented := reference.Listed(t, opsHeading); !slices.Equal(documented, want) {
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
	var held []OpType
	var opsSet, faultsSet []string
	for _, op := range sequence.Ops {
		held = append(held, op.Type)
		opsSet = append(opsSet, reference.SetKeys(reflect.ValueOf(op), reflect.TypeFor[Fault]())...)
		if op.Fault != nil {
			faultsSet = append(faultsSet, reference.SetKeys(reflect.ValueOf(*op.Fault))...)
		}
	}

	for _, opType := range opTypes {
		if !slices.Contains(held, opType) {
			t.Errorf("%s holds no %s op.", referenceSequence, opType)
		}
	}
	for _, field := range opKeys {
		if !slices.Contains(opsSet, field) {
			t.Errorf("No op in %s sets %s.", referenceSequence, field)
		}
	}
	for _, field := range faultKeys {
		if !slices.Contains(faultsSet, field) {
			t.Errorf("No fault in %s sets %s.", referenceSequence, field)
		}
	}
}
