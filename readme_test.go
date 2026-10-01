package botbox_test

import (
	"regexp"
	"strings"
	"testing"
)

// quickstartSeed matches a quickstart command and captures the seed it passes.
var quickstartSeed = regexp.MustCompile(`(?m)^(?:\./)?examples/\S+/quickstart\.sh.*\s--?seed[ =](\S+)`)

// The golden draws pin what the Makefile's example seeds draw.
func TestTheREADMEsQuickstartsRunTheMakefilesExampleSeed(t *testing.T) {
	seed, readme := makefilePins(t)["EXAMPLE_SEED"], readFile(t, "README.md")
	runs := quickstartSeed.FindAllStringSubmatch(readme, -1)
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

func TestQuickstartSeedReadsEveryWayToPassTheSeed(t *testing.T) {
	for _, command := range []string{
		"examples/x/quickstart.sh --seed 24",
		"examples/x/quickstart.sh --runs 1 --seed=24",
		"examples/x/quickstart.sh -seed 24",
		"examples/x/quickstart.sh -seed=24",
		"./examples/x/quickstart.sh --seed 24",
	} {
		if m := quickstartSeed.FindStringSubmatch(command); m == nil || m[1] != "24" {
			t.Errorf("quickstartSeed reads %q from %q, want 24.", m, command)
		}
	}
	if m := quickstartSeed.FindStringSubmatch("examples/x/quickstart.sh --no-seed 24"); m != nil {
		t.Errorf("quickstartSeed reads %q from a flag other than the seed.", m)
	}
}
