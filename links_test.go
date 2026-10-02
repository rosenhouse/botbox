package botbox_test

import (
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// A page that moved takes its headings with it, so each link from a page a
// reader follows must still land.
func TestThePagesLinksLand(t *testing.T) {
	link := regexp.MustCompile(`\]\(([^)#\s]*)(?:#([^)\s]+))?\)`)
	checked := 0
	for _, page := range append(slices.Clone(userPages), "docs/reference.md") {
		for _, m := range link.FindAllStringSubmatch(readFile(t, page), -1) {
			target, anchor := m[1], m[2]
			if strings.Contains(target, "://") {
				continue
			}
			if target == "" {
				target = page
			} else {
				target = path.Join(path.Dir(page), target)
			}
			checked++
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s links %q: %v", page, m[0], err)
				continue
			}
			if anchor != "" && !slices.Contains(anchors(readFile(t, target)), anchor) {
				t.Errorf("%s links %q, and %s has no heading with that anchor.", page, m[0], target)
			}
		}
	}
	if checked == 0 {
		t.Fatal("The pages hold no link, so this test checks nothing.")
	}
}

var heading = regexp.MustCompile(`^#+ (.*)$`)

// anchors are the fragments GitHub gives the headings of a Markdown page
// outside its fenced blocks.
func anchors(page string) []string {
	var found []string
	fenced := false
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		title := heading.FindStringSubmatch(line)
		if fenced || title == nil {
			continue
		}
		found = append(found, strings.Map(func(r rune) rune {
			switch {
			case r == ' ':
				return '-'
			case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
				return unicode.ToLower(r)
			}
			return -1
		}, title[1]))
	}
	return found
}

func TestAnchorsFollowGitHub(t *testing.T) {
	page := "# botbox\n\n## Keep botbox out of your go.mod\n\n```sh\n# not a heading\n```\n\n### 6. Generic invariants\n## `target.yaml` keys\n"
	if got, want := anchors(page), []string{"botbox", "keep-botbox-out-of-your-gomod", "6-generic-invariants", "targetyaml-keys"}; !slices.Equal(got, want) {
		t.Errorf("anchors gave %q, want %q.", got, want)
	}
}
