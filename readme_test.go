package botbox_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

const limitsHeading = "## What botbox cannot test yet"

// A team learns whether its controller fits before it installs anything. Each
// limit links the issue that tracks it, whose fix deletes the bullet.
func TestTheReadmeListsWhatBotboxCannotTestBeforeInstall(t *testing.T) {
	readme := readFile(t, "README.md")
	bullets := strings.Split(section(t, readme, limitsHeading), "\n- ")[1:]
	if strings.Index(readme, "\n"+limitsHeading+"\n") > strings.Index(readme, "\n## Install\n") {
		t.Errorf("README.md lists what botbox cannot test after Install")
	}
	if len(bullets) == 0 {
		t.Errorf("README.md lists no limit under %q", limitsHeading)
	}
	issue := regexp.MustCompile(`\]\(https://github\.com/rosenhouse/botbox/issues/[0-9]+\)`)
	for _, bullet := range bullets {
		if !issue.MatchString(bullet) {
			t.Errorf("README.md lists a limit that links no botbox issue: - %s", bullet)
		}
	}
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
	design := strings.Join(strings.Fields(readFile(t, "DESIGN.md")), " ")
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
