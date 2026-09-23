package target

import "testing"

// Load parses target.yaml before checkEnv does, so only a direct call reaches
// this error.
func TestCheckEnvReportsYAMLItCannotParse(t *testing.T) {
	if err := checkEnv([]byte("launch: ["), nil); err == nil {
		t.Error("checkEnv accepted YAML it could not parse.")
	}
}
