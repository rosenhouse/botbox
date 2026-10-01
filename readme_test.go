package botbox_test

import (
	"regexp"
	"strings"
	"testing"
)

// The golden draws pin what the Makefile's example seeds draw.
func TestTheREADMEsQuickstartsRunTheMakefilesExampleSeed(t *testing.T) {
	seed, readme := makefilePins(t)["EXAMPLE_SEED"], readFile(t, "README.md")
	runs := regexp.MustCompile(`(?m)^examples/\S+/quickstart\.sh .*--seed (\S+)`).FindAllStringSubmatch(readme, -1)
	if len(runs) == 0 {
		t.Fatal("README.md runs no quickstart with a seed, so this test checks nothing.")
	}
	for _, run := range runs {
		if run[1] != seed {
			t.Errorf("README.md runs %q, and the Makefile's EXAMPLE_SEED is %s.", run[0], seed)
		}
	}
	if want := "Seed " + seed + " draws a single op"; !strings.Contains(readme, want) {
		t.Errorf("README.md does not say %q.", want)
	}
}
