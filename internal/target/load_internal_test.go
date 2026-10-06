package target

import "testing"

// Load parses target.yaml before checkEnv does, so only a direct call reaches
// this error.
func TestCheckEnvReportsYAMLItCannotParse(t *testing.T) {
	if err := checkEnv([]byte("launch: ["), nil); err == nil {
		t.Error("checkEnv accepted YAML it could not parse.")
	}
}

// No plausible typo of a declared key tells a swap from a shared letter, so
// this asks editDistance directly.
func TestEditDistanceCountsOnlyATrueSwapAsOneEdit(t *testing.T) {
	for _, test := range []struct {
		a, b string
		want int
	}{
		{"satble", "stable", 1},
		{"ab", "xa", 2},
		{"ab", "bx", 2},
	} {
		if got := editDistance(test.a, test.b); got != test.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d.", test.a, test.b, got, test.want)
		}
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
