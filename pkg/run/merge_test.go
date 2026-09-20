package run

import (
	"encoding/json"
	"testing"
)

// The cases of RFC 7386 §3 whose target and patch are both objects, which is
// the shape an update op carries (DESIGN.md §7).
func TestMergePatchFollowsRFC7386(t *testing.T) {
	for _, test := range []struct{ object, patch, want string }{
		{object: `{"a":"b"}`, patch: `{"a":"c"}`, want: `{"a":"c"}`},
		{object: `{"a":"b"}`, patch: `{"b":"c"}`, want: `{"a":"b","b":"c"}`},
		{object: `{"a":"b"}`, patch: `{"a":null}`, want: `{}`},
		{object: `{"a":"b","b":"c"}`, patch: `{"a":null}`, want: `{"b":"c"}`},
		{object: `{"a":["b"]}`, patch: `{"a":"c"}`, want: `{"a":"c"}`},
		{object: `{"a":"c"}`, patch: `{"a":["b"]}`, want: `{"a":["b"]}`},
		{object: `{"a":{"b":"c"}}`, patch: `{"a":{"b":"d","c":null}}`, want: `{"a":{"b":"d"}}`},
		{object: `{"a":[{"b":"c"}]}`, patch: `{"a":[1]}`, want: `{"a":[1]}`},
		{object: `{"e":null}`, patch: `{"a":1}`, want: `{"e":null,"a":1}`},
		{object: `{}`, patch: `{"a":{"bb":{"ccc":null}}}`, want: `{"a":{"bb":{}}}`},
		{object: `{"spec":{"count":3,"name":"w"}}`, patch: `{"spec":{"count":5}}`, want: `{"spec":{"count":5,"name":"w"}}`},
	} {
		t.Run(test.object+" + "+test.patch, func(t *testing.T) {
			got := mergePatch(decode(t, test.object), decode(t, test.patch))

			if encode(t, got) != encode(t, decode(t, test.want)) {
				t.Errorf("The merge gave %s, want %s.", encode(t, got), test.want)
			}
		})
	}
}

func TestMergePatchLeavesThePatchAlone(t *testing.T) {
	patch := decode(t, `{"spec":{"count":5}}`)

	mergePatch(decode(t, `{"spec":{"count":3}}`), patch)
	mergePatch(decode(t, `{"spec":{"count":9}}`), patch)

	if encode(t, patch) != `{"spec":{"count":5}}` {
		t.Errorf("Applying the patch changed it to %s.", encode(t, patch))
	}
}

func decode(t *testing.T, text string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(text), &object); err != nil {
		t.Fatalf("Decoding %s failed: %v", text, err)
	}
	return object
}

func encode(t *testing.T, object map[string]any) string {
	t.Helper()
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("Encoding %v failed: %v", object, err)
	}
	return string(data)
}
