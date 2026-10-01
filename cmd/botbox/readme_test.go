package main

import (
	"os"
	"strings"
	"testing"
)

// What the cert-manager quickstart prints depends on what its seeds draw.
func TestTheREADMEShowsWhatTheCertManagerQuickstartPrints(t *testing.T) {
	// As `examples/cert-manager/quickstart.sh --seed 23` runs it, with every
	// run passing.
	code, stdout, stderr := invokeWith(t, &fakeSession{}, rapidGenerator,
		"run", "--target", "../../examples/cert-manager/target.yaml", "--runs", "5", "--seed", "23", "--out", t.TempDir())
	if code != exitOK {
		t.Fatalf("botbox run exited %d: %s", code, stderr)
	}

	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "```\n"+stdout+"```\n") {
		t.Errorf("The README shows no block that holds what botbox printed:\n%s", stdout)
	}
}
