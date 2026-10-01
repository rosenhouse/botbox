package botbox_test

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const limitsHeading = "## What botbox cannot test yet"

// limits are what botbox cannot test yet: words the README says of each, the
// issue that tracks it, or 0, and words DESIGN.md says of it. A change that
// lifts a limit deletes its rows and both statements.
var limits = []struct {
	says   string
	issue  int
	design string
}{
	{"Pod", 0, "It runs on botbox's host, which routes to no Pod"},
	{"webhook", 0, "No admission or conversion webhooks."},
	{"the version your CRD stores", 0, "A kubeconfig cluster keeps the webhook."},
	{"built-in", 0, "botbox draws no sequence for a built-in primary kind"},
	{"cluster-scoped", 38, "botbox refuses the cluster-scoped ones before the first run"},
	{"reads from another namespace", 38, "A fixture sets no `metadata.namespace`"},
	{"RBAC", 45, "the target's RBAC is never exercised"},
	{"fault", 47, "The generator draws no `Fault`"},
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
	parts := strings.Split(section, "\n- ")
	for i := range parts {
		parts[i] = oneLine(parts[i])
	}
	opening, bullets := parts[0], parts[1:]
	issue := regexp.MustCompile(`\]\(https://github\.com/rosenhouse/botbox/issues/[0-9]+\)`)
	for _, bullet := range bullets {
		if !issue.MatchString(bullet) {
			t.Errorf("README.md lists a limit that links no botbox issue: - %s", bullet)
		}
	}
	for _, limit := range limits {
		if !strings.Contains(design, limit.design) {
			t.Errorf("DESIGN.md does not say %q of the limit the README states with %q", limit.design, limit.says)
		}
		if limit.issue == 0 {
			if !strings.Contains(opening, limit.says) {
				t.Errorf("README.md does not open %q with %q", limitsHeading, limit.says)
			}
			continue
		}
		link := fmt.Sprintf("](https://github.com/rosenhouse/botbox/issues/%d)", limit.issue)
		if !slices.ContainsFunc(bullets, func(bullet string) bool {
			return strings.Contains(bullet, limit.says) && strings.Contains(bullet, link)
		}) {
			t.Errorf("README.md lists no limit that says %q and links #%d", limit.says, limit.issue)
		}
	}
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// readmeOrder pairs each README section with the words DESIGN.md orders it by.
var readmeOrder = []struct{ heading, design string }{
	{"What botbox cannot test yet", "what it cannot test yet"},
	{"Install", "install"},
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
