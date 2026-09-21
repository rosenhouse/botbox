package generate

import (
	"encoding/json"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func parseSchema(t *testing.T, declared string) *schema {
	t.Helper()
	var parsed schema
	if err := json.Unmarshal([]byte(declared), &parsed); err != nil {
		t.Fatalf("The test's own schema %s does not parse: %v.", declared, err)
	}
	return &parsed
}

func TestIntegerRangeReadsTheSchemasBounds(t *testing.T) {
	for _, testCase := range []struct {
		declared string
		lo, hi   int64
	}{
		{`{"type":"integer","format":"int32","minimum":0,"maximum":10}`, 0, 10},
		{`{"type":"integer","minimum":2,"maximum":5}`, 2, 5},
		{`{"type":"integer","minimum":2,"maximum":5,"exclusiveMinimum":true,"exclusiveMaximum":true}`, 3, 4},
		{`{"type":"integer","minimum":2}`, 2, 2 + defaultSpan},
		{`{"type":"integer","maximum":5}`, 5 - defaultSpan, 5},
		{`{"type":"integer"}`, 0, defaultSpan},
		{`{"type":"integer","format":"int32","maximum":2147483647}`, 2147483647 - defaultSpan, 2147483647},
		{`{"type":"integer","minimum":-9e18}`, -safeInteger, -safeInteger + defaultSpan},
	} {
		t.Run(testCase.declared, func(t *testing.T) {
			lo, hi := integerRange(parseSchema(t, testCase.declared))
			if lo != testCase.lo || hi != testCase.hi {
				t.Errorf("integerRange is %d to %d, want %d to %d.", lo, hi, testCase.lo, testCase.hi)
			}
		})
	}
}

func TestListAndStringLengthsReadTheSchemasBounds(t *testing.T) {
	for _, testCase := range []struct {
		declared string
		lo, hi   int
	}{
		{`{"type":"array"}`, 0, defaultMaxItems},
		{`{"type":"array","minItems":5}`, 5, 5},
		{`{"type":"array","maxItems":1}`, 0, 1},
		{`{"type":"array","minItems":1,"maxItems":3}`, 1, 3},
	} {
		t.Run(testCase.declared, func(t *testing.T) {
			if lo, hi := itemRange(parseSchema(t, testCase.declared)); lo != testCase.lo || hi != testCase.hi {
				t.Errorf("itemRange is %d to %d, want %d to %d.", lo, hi, testCase.lo, testCase.hi)
			}
		})
	}
	for _, testCase := range []struct {
		declared string
		lo, hi   int
	}{
		{`{"type":"string"}`, defaultMinLength, defaultMaxLength},
		{`{"type":"string","minLength":20}`, 20, 20},
		{`{"type":"string","maxLength":2}`, defaultMinLength, 2},
		{`{"type":"string","minLength":3,"maxLength":4}`, 3, 4},
	} {
		t.Run(testCase.declared, func(t *testing.T) {
			if lo, hi := lengthRange(parseSchema(t, testCase.declared)); lo != testCase.lo || hi != testCase.hi {
				t.Errorf("lengthRange is %d to %d, want %d to %d.", lo, hi, testCase.lo, testCase.hi)
			}
		})
	}
}

func TestGeneratedValuesMatchTheirSchema(t *testing.T) {
	for _, declared := range []string{
		`{"type":"boolean"}`,
		`{"type":"integer","minimum":-3,"maximum":3}`,
		`{"type":"number","minimum":-1.5,"maximum":2.5}`,
		`{"type":"number","minimum":0,"maximum":1,"exclusiveMinimum":true,"exclusiveMaximum":true}`,
		`{"type":"string","minLength":3,"maxLength":4}`,
		`{"type":"string","pattern":"^ab[0-9]{2}$"}`,
		`{"type":"string","enum":["1h","24h"]}`,
		`{"type":"array","minItems":1,"maxItems":2,"items":{"type":"integer","minimum":1,"maximum":9}}`,
		`{"type":"array","x-kubernetes-list-type":"set","minItems":2,"maxItems":3,
		  "items":{"type":"string","minLength":5,"maxLength":8}}`,
		`{"type":"object","required":["a"],"properties":{"a":{"type":"integer","minimum":1,"maximum":2},
		  "c":{"type":"boolean"}}}`,
	} {
		t.Run(declared, func(t *testing.T) {
			parsed := parseSchema(t, declared)
			values, err := valuesOf(parsed)
			if err != nil {
				t.Fatalf("valuesOf failed: %v.", err)
			}
			rapid.Check(t, func(rt *rapid.T) {
				value := values.Draw(rt, "value")
				if err := validate(parsed, value, "the value"); err != nil {
					rt.Fatalf("%v.", err)
				}
			})
		})
	}
}

func TestOptionalPropertiesTheSchemaSaysTooLittleAboutStayOut(t *testing.T) {
	parsed := parseSchema(t, `{"type":"object","required":["a"],"properties":{
	  "a":{"type":"integer","minimum":1,"maximum":2},
	  "when":{"type":"string","format":"date-time"}}}`)
	values, err := valuesOf(parsed)
	if err != nil {
		t.Fatalf("valuesOf failed: %v.", err)
	}
	rapid.Check(t, func(rt *rapid.T) {
		object := values.Draw(rt, "value").(map[string]any)
		if _, present := object["when"]; present {
			t.Errorf("The object carries a when of %#v, and the schema says only that it is a date-time.", object["when"])
		}
	})
}

func TestSchemasThatSayTooLittleAreConfigurationErrors(t *testing.T) {
	for _, testCase := range []struct{ declared, reports string }{
		{`{"type":"string","format":"date-time"}`, "format"},
		{`{"x-kubernetes-int-or-string":true}`, "does not say what values"},
		{`{"type":"object","x-kubernetes-preserve-unknown-fields":true}`, "no property"},
		{`{"type":"array"}`, "what the array holds"},
		{`{"type":"integer","minimum":5,"maximum":3}`, "no integer"},
		{`{"type":"string","minLength":5,"maxLength":3}`, "no string"},
		{`{"type":"array","minItems":5,"maxItems":3,"items":{"type":"boolean"}}`, "no list"},
		{`{"type":"array","x-kubernetes-list-type":"map","x-kubernetes-list-map-keys":["name"],
		  "items":{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}}`, "keyed"},
		{`{"type":"object","required":["when"],"properties":{"when":{"type":"string","format":"date-time"}}}`, "when"},
	} {
		t.Run(testCase.declared, func(t *testing.T) {
			_, err := valuesOf(parseSchema(t, testCase.declared))
			if err == nil {
				t.Fatal("valuesOf generated from a schema that does not say what the value may be.")
			}
			if !strings.Contains(err.Error(), testCase.reports) {
				t.Errorf("valuesOf reported %q, which does not mention %q.", err, testCase.reports)
			}
		})
	}
}
