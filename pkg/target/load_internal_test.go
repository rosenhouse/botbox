package target

import "testing"

// Load parses target.yaml before checkEnv does, so only a direct call reaches
// this error.
func TestCheckEnvReportsYAMLItCannotParse(t *testing.T) {
	if err := checkEnv([]byte("launch: ["), nil); err == nil {
		t.Error("checkEnv accepted YAML it could not parse.")
	}
}

// Load asks only once decoding failed, so only a direct call reaches these.
func TestUnknownKeyFindsNothingInYAMLItCannotWalk(t *testing.T) {
	for _, data := range []string{"", "launch: ["} {
		if err := unknownKey([]byte(data)); err != nil {
			t.Errorf("unknownKey(%q) returned %v.", data, err)
		}
	}
}
