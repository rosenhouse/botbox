package botbox_test

import (
	"regexp"
	"slices"
	"strconv"
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

func TestTheREADMEShowsRunsOfTheMakefilesExampleSeeds(t *testing.T) {
	first, err := strconv.Atoi(makefilePins(t)["EXAMPLE_SEED"])
	if err != nil {
		t.Fatal(err)
	}
	runs := regexp.MustCompile(`(?m)^run (\d+): seed (\d+), generated$`).FindAllStringSubmatch(readFile(t, "README.md"), -1)
	if len(runs) == 0 {
		t.Fatal("README.md shows no drawn run, so this test checks nothing.")
	}
	for _, run := range runs {
		n, _ := strconv.Atoi(run[1])
		if want := strconv.Itoa(first + n - 1); run[2] != want {
			t.Errorf("README.md shows %q, and from the Makefile's EXAMPLE_SEED run %d draws seed %s.", run[0], n, want)
		}
	}
}

// Requiring botbox raises a module to these versions, as go prints them.
func TestTheREADMEInstallQuotesGoModsVersions(t *testing.T) {
	readme, required := readFile(t, "README.md"), goModVersions(t)
	install := section(t, readme, "## Install") + section(t, readme, "### Keep botbox out of your go.mod")
	quoted := map[string]bool{}
	check := func(text, module, version string) {
		quoted[module] = true
		if version != required[module] {
			t.Errorf("README.md quotes %q, and go.mod requires %s %s.", text, module, required[module])
		}
	}
	for _, m := range regexp.MustCompile(`(?:takes Go |requires go >= |go mod edit -go=|upgraded go \S+ => )([\d.]+)`).FindAllStringSubmatch(install, -1) {
		check(m[0], "go", m[1])
	}
	for _, m := range regexp.MustCompile(`upgraded (\S+) v\S+ => (v[\w.-]+)`).FindAllStringSubmatch(install, -1) {
		check(m[0], m[1], m[2])
	}
	for _, module := range []string{"go", "k8s.io/api", "sigs.k8s.io/controller-runtime"} {
		if !quoted[module] {
			t.Errorf("README.md's Install section quotes no version of %s.", module)
		}
	}
}

// go test caches a pass, and cannot see the controller or target.yaml that
// botbox reads.
func TestTheREADMERunsTheGoTestRecipeUncachedUnderItsBuildTag(t *testing.T) {
	const recipe = "targets/toy-widget/botbox_test.go"
	tag := regexp.MustCompile(`^//go:build (\w+)\n`).FindStringSubmatch(readFile(t, recipe))
	if tag == nil {
		t.Fatalf("%s has no build tag of one word.", recipe)
	}
	commands := regexp.MustCompile("run\\s+`(go test [^`]*)`").FindAllStringSubmatch(section(t, readFile(t, "README.md"), "### From go test"), -1)
	if len(commands) == 0 {
		t.Fatal("README.md's From go test section runs no go test command.")
	}
	for _, command := range commands {
		flags := strings.Fields(command[1])
		tags := slices.Index(flags, "-tags")
		if !slices.Contains(flags, "-count=1") || tags < 0 || tags+1 == len(flags) || flags[tags+1] != tag[1] {
			t.Errorf("README.md runs %q, and the recipe needs -count=1 and -tags %s.", command[1], tag[1])
		}
	}
}

// goModVersions maps "go" to go.mod's go directive, and each module go.mod
// requires to its version.
func goModVersions(t *testing.T) map[string]string {
	t.Helper()
	versions := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\t?(\S+) (v\S+|[\d.]+)(?: // indirect)?$`).FindAllStringSubmatch(readFile(t, "go.mod"), -1) {
		versions[m[1]] = m[2]
	}
	if versions["go"] == "" || versions["k8s.io/api"] == "" {
		t.Fatalf("goModVersions read %v from go.mod, with no go directive or no k8s.io/api.", versions)
	}
	return versions
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
