package botbox_test

import (
	"regexp"
	"strings"
	"testing"
)

// A reader of the README needs no design document until its last two
// sections: the invariant table links DESIGN.md's statements, and the
// internals point at the rest.
func TestTheReadmeNeedsNoDesignDocument(t *testing.T) {
	var (
		milestone = regexp.MustCompile(`\bM[0-9]+\b`)
		citation  = regexp.MustCompile(`§|\bD[0-9]+\b`)
	)
	section := ""
	for i, line := range strings.Split(readFile(t, "README.md"), "\n") {
		if heading, found := strings.CutPrefix(line, "## "); found {
			section = heading
		}
		where := func(problem string) { t.Errorf("README.md:%d, under %q, %s: %s", i+1, section, problem, line) }
		if milestone.MatchString(line) {
			where("names a milestone")
		}
		if section == "Development and internals" {
			continue
		}
		if citation.MatchString(line) {
			where("cites a section or a decision of DESIGN.md")
		}
		if section != "Invariants" && strings.Contains(line, "DESIGN") {
			where("sends the reader to DESIGN.md")
		}
	}
}
