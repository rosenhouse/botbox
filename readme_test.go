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
var limits = []limit{
	{"cannot reach a Pod or a Service", 0, "It runs on botbox's host, which routes to no Pod"},
	{"no admission or conversion webhook of yours runs", 0, "No admission or conversion webhooks."},
	{"on the version your CRD stores", 0, "A kubeconfig cluster keeps the webhook."},
	{"generates sequences only for a primary kind your `crds` define", 0, "botbox draws no sequence for a built-in primary kind"},
	{"tests namespaced kinds only", 38, "every managed kind and every fixture must be namespaced"},
	{"refuses a cluster-scoped primary, managed kind or fixture before the first run", 38, "botbox refuses the cluster-scoped ones before the first run"},
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
	parts := regexp.MustCompile(`\n *(?:[-*+]|[0-9]+[.)]) `).Split(section, -1)
	for i := range parts {
		parts[i] = oneLine(parts[i])
	}
	opening, bullets := parts[0], parts[1:]
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
