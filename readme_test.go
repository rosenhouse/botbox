package botbox_test

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const limitsHeading = "## What botbox cannot test yet"

// limits are what botbox cannot test yet: words the README says of each, the
// issue that tracks it, or 0, and words DESIGN.md says of it. A change that
// lifts a limit deletes its rows and both statements.
var limits = []limit{
	{"cannot reach a Pod or a Service", 0, "It runs on botbox's host, which routes to no Pod"},
	{"no admission or conversion webhook of yours runs", 0, "No admission or conversion webhooks."},
	{"on the version your CRD stores", 0, "A kubeconfig cluster keeps the webhook."},
	{"generates sequences only for a primary kind your `crds` define", 0, "botbox draws no sequence for a built-in primary kind"},
	{"tests namespaced kinds only", 38, "every managed kind and every fixture must be namespaced"},
	{"refuses a cluster-scoped primary, managed kind or fixture when it loads the target", 38, "The first runs when botbox loads the target."},
	{"passes a controller that leaks a child in another namespace", 38, "it does not see a child the target creates in another"},
	{"cannot supply an object your controller reads from another namespace", 38, "A fixture sets no `metadata.namespace`"},
	{"does not test your controller's RBAC", 45, "the target's RBAC is never exercised"},
	{"inject no faults", 47, "The generator draws no `Fault`"},
}

type limit struct {
	says   string
	issue  int
	design string
}

// A team learns whether its controller fits before it installs anything.
func TestTheReadmeSaysWhatBotboxCannotTestRightAfterWhatItDoes(t *testing.T) {
	design := oneLine(readFile(t, "DESIGN.md"))
	intro, rest, found := strings.Cut(readFile(t, "README.md"), "\n"+limitsHeading+"\n")
	if !found {
		t.Fatalf("README.md has no %q heading", limitsHeading)
	}
	if strings.Contains(intro, "\n## ") || strings.TrimSpace(strings.TrimPrefix(intro, "# botbox\n")) == "" {
		t.Errorf("README.md does not say what botbox cannot test right after what it does")
	}
	section, _, _ := strings.Cut(rest, "\n## ")
	opening, bullets := limitParts(section)
	for _, bullet := range bullets {
		if !slices.ContainsFunc(limits, func(l limit) bool { return l.isIn(bullet) }) {
			t.Errorf("No row of limits says this README.md limit and links its issue: %s", bullet)
		}
	}
	for _, l := range limits {
		if !strings.Contains(design, l.design) {
			t.Errorf("DESIGN.md does not say %q of the limit the README states with %q", l.design, l.says)
		}
		if l.issue == 0 {
			if !strings.Contains(opening, l.says) {
				t.Errorf("README.md does not open %q with %q", limitsHeading, l.says)
			}
			continue
		}
		if !slices.ContainsFunc(bullets, l.isIn) {
			t.Errorf("README.md lists no limit that says %q and links #%d", l.says, l.issue)
		}
	}
}

var listItem = regexp.MustCompile(`\n *(?:[-*+]|[0-9]+[.)])[ \t]+`)

// limitParts splits the limits section into its opening and its other parts:
// each bullet, and each paragraph after the list.
func limitParts(section string) (opening string, parts []string) {
	items := listItem.Split(section, -1)
	for i, marker := range listItem.FindAllString(section, -1) {
		// A paragraph indented less than the item's text ends the list.
		afterList := regexp.MustCompile(fmt.Sprintf(`\n\n {0,%d}(\S)`, column(strings.TrimPrefix(marker, "\n"))-1))
		parts = append(parts, strings.Split(afterList.ReplaceAllString(items[i+1], "\x00$1"), "\x00")...)
	}
	for i := range parts {
		parts[i] = oneLine(parts[i])
	}
	return oneLine(items[0]), parts
}

// column is the width of start, which begins a line, with tab stops of 4.
func column(start string) int {
	width := 0
	for _, r := range start {
		if r == '\t' {
			width += 4 - width%4
		} else {
			width++
		}
	}
	return width
}

func TestLimitPartsSplitsEachBulletAndEachParagraphAfterTheList(t *testing.T) {
	opening, parts := limitParts("\nOpening one.\n\nOpening two.\n\n- First\n  bullet.\n\n  First's paragraph.\n\n After the first list.\n" +
		"-\tSecond.\n\n    Second's paragraph.\n\n  After the second list.\n1.  Third.\n\n   Also after the list.\n\nAfter the list.\n")

	if want := "Opening one. Opening two."; opening != want {
		t.Errorf("limitParts gave the opening %q, want %q.", opening, want)
	}
	if want := []string{"First bullet. First's paragraph.", "After the first list.", "Second. Second's paragraph.", "After the second list.",
		"Third.", "Also after the list.", "After the list."}; !slices.Equal(parts, want) {
		t.Errorf("limitParts gave the parts %q, want %q.", parts, want)
	}
}

func (l limit) isIn(bullet string) bool {
	link := fmt.Sprintf("](https://github.com/rosenhouse/botbox/issues/%d)", l.issue)
	return strings.Contains(bullet, l.says) && strings.Contains(bullet, link)
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// readmeOrder pairs each README section with the words DESIGN.md orders it by.
var readmeOrder = []struct{ heading, design string }{
	{"What botbox cannot test yet", "what it cannot test yet"},
	{"Install", "install"},
	{"A find in half a minute", "a find in half a minute"},
	{"Quickstart: cert-manager", "quickstart against cert-manager"},
	{"A second example: external-secrets", "what the second example adds"},
	{"Your own controller", "writing `target.yaml` for your own controller"},
	{"Reading a report", "reading a report"},
	{"When botbox exits 2", "what to change when botbox exits 2"},
	{"Running in CI", "a CI recipe for adopters"},
	{"Invariants", "a one-line-per-invariant table"},
	{"Development and internals", `a closing "Development and internals" section`},
}

func TestTheReadmeFollowsTheOrderDesignGives(t *testing.T) {
	design := oneLine(readFile(t, "DESIGN.md"))
	_, order, _ := strings.Cut(design, "**README.** Usage-first; internals live here and in `docs/`. Order: ")
	order, _, _ = strings.Cut(order, ". ")
	var headings []string
	for _, line := range strings.Split(readFile(t, "README.md"), "\n") {
		if heading, found := strings.CutPrefix(line, "## "); found {
			headings = append(headings, heading)
		}
	}
	last := -1
	for _, section := range readmeOrder {
		heading := slices.Index(headings, section.heading)
		if heading <= last {
			t.Errorf("README.md has no %q section after the one before it in DESIGN.md's order", section.heading)
		}
		last = max(heading, last)
		_, after, found := strings.Cut(order, section.design)
		if !found {
			t.Errorf("DESIGN.md's README order does not name %q after the section before it", section.design)
			continue
		}
		order = after
	}
}

// A reader of the README needs no design document outside its Invariants
// section, which links DESIGN.md's statements, and its internals section.
func TestTheReadmeNeedsNoDesignDocument(t *testing.T) {
	milestone := regexp.MustCompile(`\bM[0-9]+\b`)
	section := ""
	for i, line := range strings.Split(readFile(t, "README.md"), "\n") {
		if heading, found := strings.CutPrefix(line, "## "); found {
			section = heading
		}
		if milestone.MatchString(line) {
			t.Errorf("README.md:%d names a milestone: %s", i+1, line)
		}
		said := line
		switch section {
		case "Development and internals":
			continue
		case "Invariants":
			said = strings.ReplaceAll(said, "DESIGN.md", "")
		}
		if found := designVocabulary.FindString(said); found != "" {
			t.Errorf("README.md:%d, under %q, uses %q, which only DESIGN.md explains: %s", i+1, section, found, line)
		}
	}
}

// quickstartSeed matches a quickstart command and captures the seed it passes.
var quickstartSeed = regexp.MustCompile(`(?m)^(?:\./)?examples/\S+/quickstart\.sh.*\s--?seed(?:[ \t]+|=)(\S+)`)

// quickstartSeeds finds each quickstart command, with the lines a backslash
// joins, and the seed it passes.
func quickstartSeeds(text string) [][]string {
	return quickstartSeed.FindAllStringSubmatch(strings.ReplaceAll(text, "\\\n", " "), -1)
}

// The golden draws pin what the Makefile's example seeds draw.
func TestTheREADMEsQuickstartsRunTheMakefilesExampleSeed(t *testing.T) {
	seed, readme := makefilePins(t)["EXAMPLE_SEED"], readFile(t, "README.md")
	runs := quickstartSeeds(readme)
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
	readme := section(t, readFile(t, "README.md"), "### From go test")
	if plain := regexp.MustCompile("out of a plain\\s+`(go test [^`]*)`").FindStringSubmatch(readme); plain == nil {
		t.Error("README.md does not say which go test the build tag keeps the recipe out of.")
	} else if strings.Contains(plain[1], "-tags") {
		t.Errorf("README.md says the build tag keeps the recipe out of %q, which sets -tags.", plain[1])
	}
	commands := regexp.MustCompile("run\\s+`(go test [^`]*)`").FindAllStringSubmatch(readme, -1)
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
		"examples/x/quickstart.sh --seed  24",
		"examples/x/quickstart.sh --runs 1 \\\n  --seed 24",
	} {
		if m := quickstartSeeds(command); len(m) != 1 || m[0][1] != "24" {
			t.Errorf("quickstartSeeds reads %q from %q, want 24.", m, command)
		}
	}
	if m := quickstartSeeds("examples/x/quickstart.sh --no-seed 24"); m != nil {
		t.Errorf("quickstartSeeds reads %q from a flag other than the seed.", m)
	}
}

// An invocation's directory ends in the seed of its first run.
func TestTheREADMEsEvidenceDirectoriesNameTheirFirstRunsSeed(t *testing.T) {
	firstSeed, dirSeed := regexp.MustCompile(`(?m)^run 1: seed (\d+),`), regexp.MustCompile(`Z-(\d+)/run-`)
	checked := 0
	for _, block := range strings.Split(readFile(t, "README.md"), "```") {
		first := firstSeed.FindStringSubmatch(block)
		for _, dir := range dirSeed.FindAllStringSubmatch(block, -1) {
			checked++
			if first == nil || dir[1] != first[1] {
				t.Errorf("README.md shows the evidence directory %q in a block whose first run shows %q.", dir[0], first)
			}
		}
	}
	if checked == 0 {
		t.Fatal("README.md shows no evidence directory, so this test checks nothing.")
	}
}
