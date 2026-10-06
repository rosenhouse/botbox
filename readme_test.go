package botbox_test

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rosenhouse/botbox/internal/run"
	"github.com/rosenhouse/botbox/internal/target"
)

const limitsHeading = "## What botbox cannot test yet"

// limits are what botbox cannot test yet: words the README says of each, the
// issue that tracks it, or 0, and words DESIGN.md says of it. A change that
// lifts a limit deletes its rows and both statements.
var limits = []limit{
	{"cannot reach a Pod or a Service", 0, "It runs on botbox's host, which routes to no Pod"},
	{"no admission or conversion webhook of yours runs", 0, "No admission or conversion webhooks."},
	{"on the version your CRD stores", 0, "A kubeconfig cluster keeps the webhook."},
	{"generates sequences only for custom resources", 0, "botbox draws no sequence for a built-in primary kind"},
	{"tests namespaced kinds only", 38, "every managed kind and every fixture must be namespaced"},
	{"misses a child your controller leaks into another namespace", 38, "it does not see a child the target creates in another"},
	{"cannot supply an object your controller reads from another namespace", 38, "A fixture sets no `metadata.namespace`"},
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
	opening, bullets, err := limitParts(section)
	if err != nil {
		t.Fatalf("README.md: %v", err)
	}
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

// Each line of the limits section begins its text with a letter, a link or a
// parenthesis, which opens no block that could hide a limit.
var (
	openingLine = regexp.MustCompile(`^[A-Za-z\[(]`)
	bulletLine  = regexp.MustCompile(`^- [A-Za-z\[(]`)
	bulletMore  = regexp.MustCompile(`^  [A-Za-z\[(]`)
)

// HTML, a footnote or a link definition can hide text or move it out of the
// section. In a bullet, code or a backslash can also turn a link into text.
var (
	hides         = regexp.MustCompile(`<|\[\^|\]:`)
	hidesInBullet = regexp.MustCompile("[`\\\\]")
)

// limitParts splits the limits section into its opening paragraphs and its
// "- " bullets, and refuses any other line.
func limitParts(section string) (opening string, bullets []string, err error) {
	for _, line := range strings.Split(section, "\n") {
		switch {
		case strings.TrimSpace(line) == "":
		case hides.MatchString(line) || (bullets != nil || bulletLine.MatchString(line)) && hidesInBullet.MatchString(line):
			return "", nil, fmt.Errorf("the limits section holds no HTML, footnote or link definition, and a bullet holds no code or backslash: %q", line)
		case bulletLine.MatchString(line):
			bullets = append(bullets, strings.TrimPrefix(line, "- "))
		case bullets == nil && openingLine.MatchString(line):
			opening += " " + line
		case bullets != nil && bulletMore.MatchString(line):
			bullets[len(bullets)-1] += " " + line
		default:
			return "", nil, fmt.Errorf("the limits section is paragraphs, then \"- \" bullets whose other lines are indented two spaces, "+
				"and nothing follows the list. Each line's text begins with a letter, a link or a parenthesis. This line does not fit: %q", line)
		}
	}
	for i, bullet := range bullets {
		bullets[i] = oneLine(bullet)
	}
	return oneLine(opening), bullets, nil
}

func TestLimitPartsSplitsTheOpeningAndTheBullets(t *testing.T) {
	opening, bullets, err := limitParts("\nOpening `one`.\n[Link](u) two.\n   \n(Three.)\n\n- First\n  bullet.\n\n  First's paragraph.\n- [Second](u)\n  (#1).\n- (Third)\n  [#2](u).\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Opening `one`. [Link](u) two. (Three.)"; opening != want {
		t.Errorf("limitParts gave the opening %q, want %q.", opening, want)
	}
	if want := []string{"First bullet. First's paragraph.", "[Second](u) (#1).", "(Third) [#2](u)."}; !slices.Equal(bullets, want) {
		t.Errorf("limitParts gave the bullets %q, want %q.", bullets, want)
	}
}

// Each refused line could open a block that hides a limit, or puts one outside
// the bullets.
func TestLimitPartsRefusesAnyOtherShape(t *testing.T) {
	for _, section := range []string{
		"\n<!--\n- Item.\n", "\n```\n- Item.\n", "\n~~~\n", "\n> Quote.\n", "\n# Heading\n", "\n| Table |\n", "\n**Bold**.\n", "\n1. Other.\n",
		"\n Indented.\n", "\n  Indented.\n", "\n\tIndented.\n", "\n-  Item.\n", "\n- \n\n  Item.\n", "\n- - -\n", "\n- `Code`.\n", "\n* Other.\n",
		"\n- Item.\nLazy.\n", "\n- Item.\n\nAfter.\n", "\n- Item.\n Inside.\n", "\n- Item.\n   Inside.\n", "\n- Item.\n\tInside.\n",
		"\n- Item.\n  <!-- Comment -->\n", "\n- Item.\n  ```\n", "\n- Item.\n  ~~~\n", "\n-\tItem.\n", "\n+ Other.\n",
		"\nOpening <!-- hidden --> text.\n", "\n- Item <!-- ([#1](u)) -->.\n", "\n- Item <span hidden>([#1](u))</span>.\n", "\nOpening.[^1]\n", "\n[pod]: u \"Hidden.\"\n",
		"\n- Item `([#1](u))`.\n", "\n- Item \\([#1](u)).\n", "\n- Item\n  more `([#1](u))`.\n",
	} {
		if _, _, err := limitParts(section); err == nil {
			t.Errorf("limitParts accepted %q.", section)
		}
	}
}

func TestABulletSaysALimitWithItsWordsAndItsLink(t *testing.T) {
	l := limit{says: "botbox cannot", issue: 45}
	link := "([#45](https://github.com/rosenhouse/botbox/issues/45))"
	if !l.isIn("botbox cannot " + link) {
		t.Errorf("isIn misses the limit in %q.", "botbox cannot "+link)
	}
	for _, bullet := range []string{"botbox can " + link, "botbox cannot ([#46](https://github.com/rosenhouse/botbox/issues/46))"} {
		if l.isIn(bullet) {
			t.Errorf("isIn finds %q in %q.", l.says, bullet)
		}
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
	{"Quick start", "a quick start"},
	{"Your own controller", "writing `target.yaml` for your own controller"},
	{"Reading a failure", "reading a failure"},
	{"Running in CI", "running in CI"},
	{"Development and internals", `a closing "Development and internals" section`},
}

// A reader learns first that botbox tests against a real API server.
func TestTheReadmeOpensWithTheAPIServerBotboxRuns(t *testing.T) {
	intro, _, _ := strings.Cut(readFile(t, "README.md"), "\n## ")
	if says := "against a real kube-apiserver and etcd"; !strings.Contains(oneLine(intro), says) {
		t.Errorf("README.md's intro does not say botbox runs your controller %q.", says)
	}
}

// userPages are the README and the pages it sends a reader to for detail.
var userPages = []string{"README.md", "docs/targets.md", "docs/failures.md", "docs/examples.md", ciPage}

const ciPage = "docs/ci.md"

// A reader who copies a sequence from a page gets one botbox runs.
func TestEverySequenceAPageShowsLoads(t *testing.T) {
	shown := 0
	for _, page := range userPages {
		for _, block := range fencedBlocks(readFile(t, page)) {
			if !strings.HasPrefix(block, `{"seed"`) {
				continue
			}
			shown++
			if _, err := run.UnmarshalSequence([]byte(block)); err != nil {
				t.Errorf("%s shows a sequence botbox refuses: %v\n%s", page, err, block)
			}
		}
	}
	if shown < 3 {
		t.Fatalf("The pages show %d sequences, and the README and docs/targets.md show at least three.", shown)
	}
}

func TestTheReadmeFollowsTheOrderDesignGives(t *testing.T) {
	design := oneLine(readFile(t, "DESIGN.md"))
	_, order, _ := strings.Cut(design, "**README.** Usage-first and short; internals live here and in `docs/`. Order: ")
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

// A reader of the README, and of the pages it sends them to, needs no design
// document outside the README's internals section.
func TestTheReadmeNeedsNoDesignDocument(t *testing.T) {
	milestone := regexp.MustCompile(`\bM[0-9]+\b`)
	for _, page := range userPages {
		section := ""
		for i, line := range strings.Split(readFile(t, page), "\n") {
			if heading, found := strings.CutPrefix(line, "## "); found {
				section = heading
			}
			if milestone.MatchString(line) {
				t.Errorf("%s:%d names a milestone: %s", page, i+1, line)
			}
			said := line
			switch {
			case page != "README.md":
			case section == "Development and internals":
				continue
			}
			if found := designVocabulary.FindString(said); found != "" {
				t.Errorf("%s:%d, under %q, uses %q, which only DESIGN.md explains: %s", page, i+1, section, found, line)
			}
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

const examplesPage = "docs/examples.md"

// The golden draws pin what the Makefile's example seeds draw.
func TestTheExamplesPagesQuickstartsRunTheMakefilesExampleSeed(t *testing.T) {
	seed, page := makefilePins(t)["EXAMPLE_SEED"], readFile(t, examplesPage)
	runs := quickstartSeeds(page)
	if len(runs) == 0 {
		t.Fatalf("%s runs no quickstart with a seed, so this test checks nothing.", examplesPage)
	}
	for _, run := range runs {
		if run[1] != seed {
			t.Errorf("%s runs %q, and the Makefile's EXAMPLE_SEED is %s.", examplesPage, run[0], seed)
		}
	}
	if want := "Seed " + seed + " draws a single op"; !strings.Contains(page, want) {
		t.Errorf("%s does not say %q.", examplesPage, want)
	}
}

func TestTheExamplesPageShowsRunsOfTheMakefilesExampleSeeds(t *testing.T) {
	first, err := strconv.Atoi(makefilePins(t)["EXAMPLE_SEED"])
	if err != nil {
		t.Fatal(err)
	}
	runs := regexp.MustCompile(`(?m)^run (\d+): seed (\d+), generated$`).FindAllStringSubmatch(readFile(t, examplesPage), -1)
	if len(runs) == 0 {
		t.Fatalf("%s shows no drawn run, so this test checks nothing.", examplesPage)
	}
	for _, run := range runs {
		n, _ := strconv.Atoi(run[1])
		if want := strconv.Itoa(first + n - 1); run[2] != want {
			t.Errorf("%s shows %q, and from the Makefile's EXAMPLE_SEED run %d draws seed %s.", examplesPage, run[0], n, want)
		}
	}
}

// Requiring botbox raises a module to these versions, as go prints them.
func TestTheREADMEQuotesGoModsVersions(t *testing.T) {
	required := goModVersions(t)
	install := section(t, readFile(t, "README.md"), "## Install") + section(t, readFile(t, ciPage), "## Keep botbox out of your go.mod")
	quoted := map[string]bool{}
	check := func(text, module, version string) {
		quoted[module] = true
		if version != required[module] {
			t.Errorf("README.md or %s quotes %q, and go.mod requires %s %s.", ciPage, text, module, required[module])
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
			t.Errorf("README.md's Install section and %s's tools module section quote no version of %s.", ciPage, module)
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
	page := section(t, readFile(t, ciPage), "## From go test")
	if plain := regexp.MustCompile("out of a plain\\s+`(go test [^`]*)`").FindStringSubmatch(page); plain == nil {
		t.Errorf("%s does not say which go test the build tag keeps the recipe out of.", ciPage)
	} else if strings.Contains(plain[1], "-tags") {
		t.Errorf("%s says the build tag keeps the recipe out of %q, which sets -tags.", ciPage, plain[1])
	}
	commands := regexp.MustCompile("run\\s+`(go test [^`]*)`").FindAllStringSubmatch(page, -1)
	if len(commands) == 0 {
		t.Fatalf("%s's From go test section runs no go test command.", ciPage)
	}
	for _, command := range commands {
		flags := strings.Fields(command[1])
		tags := slices.Index(flags, "-tags")
		if !slices.Contains(flags, "-count=1") || tags < 0 || tags+1 == len(flags) || flags[tags+1] != tag[1] {
			t.Errorf("%s runs %q, and the recipe needs -count=1 and -tags %s.", ciPage, command[1], tag[1])
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

// Few real CRs compare a count as the toy's ready does. Most report a Ready
// condition, and the README gives a ready for one.
func TestTheReadmesReadyConditionExampleHoldsOnlyOnACurrentReadyCondition(t *testing.T) {
	item := strings.ReplaceAll(section(t, readFile(t, "README.md"), "### Write target.yaml"), "\n   ", "\n")
	var examples []string
	for _, block := range fencedBlocks(item) {
		if strings.HasPrefix(block, "ready:") {
			examples = append(examples, block)
		}
	}
	if len(examples) != 1 {
		t.Fatalf("README.md's Write target.yaml section shows %d ready examples, want one for a Ready condition.", len(examples))
	}
	toy, err := filepath.Abs("targets/toy-widget")
	if err != nil {
		t.Fatal(err)
	}
	declared := filepath.Join(t.TempDir(), "target.yaml")
	writeFile(t, declared, fmt.Sprintf("name: ready-example\ncrds: [%s/crds]\nprimary: toy.botbox/v1/Widget\nsample: %s/widget.yaml\nlaunch:\n  binary: controller\n%s",
		toy, toy, examples[0]))
	loaded, err := target.Load(declared)
	if err != nil {
		t.Fatalf("The README's ready does not load: %v\n%s", err, examples[0])
	}
	// condition is a status whose one condition is current and Ready, as edit
	// leaves it.
	condition := func(edit func(c map[string]any)) map[string]any {
		c := map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(2)}
		edit(c)
		return map[string]any{"conditions": []any{c}}
	}
	for _, test := range []struct {
		name   string
		status map[string]any
		want   bool
	}{
		{"a Ready condition of the current generation", condition(func(map[string]any) {}), true},
		{"no status", nil, false},
		{"a status with no conditions", map[string]any{"ready": int64(3)}, false},
		{"a Ready condition of an older generation", condition(func(c map[string]any) { c["observedGeneration"] = int64(1) }), false},
		{"a Ready condition with no observedGeneration", condition(func(c map[string]any) { delete(c, "observedGeneration") }), false},
		{"a Ready condition that is False", condition(func(c map[string]any) { c["status"] = "False" }), false},
		{"another condition that is True", condition(func(c map[string]any) { c["type"] = "Synced" }), false},
	} {
		cr := map[string]any{"metadata": map[string]any{"name": "widget", "generation": int64(2)}}
		if test.status != nil {
			cr["status"] = test.status
		}
		holds, err := loaded.Ready(&unstructured.Unstructured{Object: cr})
		if err != nil || holds != test.want {
			t.Errorf("The README's ready gives %t, %v on %s, want %t.", holds, err, test.name, test.want)
		}
	}
}

// botbox --help sends a reader to docs/failures.md for what each failure means.
func TestTheFailuresPageNamesEveryCheck(t *testing.T) {
	checks := regexp.MustCompile(`(?m)^\| (G[0-9]+) \|`).FindAllStringSubmatch(section(t, readFile(t, "README.md"), "## Reading a failure"), -1)
	if len(checks) == 0 {
		t.Fatal("README.md's Reading a failure section lists no check, so this test checks nothing.")
	}
	// A heading that names a check says nothing of what its failure means.
	text := regexp.MustCompile(`(?m)^#.*$`).ReplaceAllString(readFile(t, "docs/failures.md"), "")
	for _, check := range checks {
		if !regexp.MustCompile(`\b` + check[1] + `\b`).MatchString(text) {
			t.Errorf("docs/failures.md does not say what a %s failure means.", check[1])
		}
	}
}

// An invocation's directory ends in the seed of its first run.
func TestThePagesEvidenceDirectoriesNameTheirFirstRunsSeed(t *testing.T) {
	firstSeed, dirSeed := regexp.MustCompile(`(?m)^run 1: seed (\d+),`), regexp.MustCompile(`Z-(\d+)/run-`)
	for _, page := range []string{"README.md", examplesPage} {
		checked := 0
		for _, block := range strings.Split(readFile(t, page), "```") {
			first := firstSeed.FindStringSubmatch(block)
			for _, dir := range dirSeed.FindAllStringSubmatch(block, -1) {
				checked++
				if first == nil || dir[1] != first[1] {
					t.Errorf("%s shows the evidence directory %q in a block whose first run shows %q.", page, dir[0], first)
				}
			}
		}
		if checked == 0 {
			t.Errorf("%s shows no evidence directory, so this test checks nothing there.", page)
		}
	}
}
