package botbox_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// designVocabulary is what a reader needs DESIGN.md to understand, and
// go install ships no DESIGN.md.
var designVocabulary = regexp.MustCompile(`§|DESIGN|T_settle|T_stable|T_delete|N_errloop|N_quiet|N_objects|\bD@?[0-9]+\b`)

// citesDesign are the strings that may cite DESIGN.md, by file. The bug
// matrix is a page for developers, and DESIGN.md holds the bug catalog.
var citesDesign = map[string]string{
	"cmd/botbox/matrix.go": "seeded bug of DESIGN.md §9.1",
}

// Every string botbox prints comes from a literal, so no literal may need
// DESIGN.md to be understood.
func TestNoStringLiteralUsesTheDesignsVocabulary(t *testing.T) {
	read := map[string]bool{}
	for _, path := range goFilesOutsideTests(t, "docs") {
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			read[path] = true
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if allowed, cites := citesDesign[path]; cites && strings.Contains(value, allowed) {
				value = strings.ReplaceAll(value, allowed, "")
			}
			if found := designVocabulary.FindString(value); found != "" {
				t.Errorf("%s: the string %q uses %q, which only DESIGN.md explains.", path, value, found)
			}
			return true
		})
	}
	requireRead(t, read, "cmd/botbox/main.go", "internal/invariant/g2.go", "targets/toy-widget/controller/bug.go")
}

// A comment must make sense without DESIGN.md open. go doc prints some of
// them, and go install ships no DESIGN.md.
func TestNoCommentUsesTheDesignsVocabulary(t *testing.T) {
	read := map[string]bool{}
	positions := token.NewFileSet()
	for _, path := range goFilesOutsideTests(t) {
		parsed, err := parser.ParseFile(positions, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, group := range parsed.Comments {
			read[path] = true
			for _, comment := range group.List {
				if found := designVocabulary.FindString(comment.Text); found != "" {
					t.Errorf("%s: %s uses %q, which only DESIGN.md explains.", positions.Position(comment.Pos()), comment.Text, found)
				}
			}
		}
	}
	requireRead(t, read, "cmd/botbox/main.go", "internal/invariant/g2.go", "targets/toy-widget/controller/bug.go", "docs/spikes/cert-manager-envtest/main.go")
}

// requireRead fails unless the scan read something in each of paths.
func requireRead(t *testing.T, read map[string]bool, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if !read[path] {
			t.Errorf("The scan read nothing in %s.", path)
		}
	}
}

// goFilesOutsideTests lists the repository's Go files outside tests, outside
// skipped and outside what .gitignore names.
func goFilesOutsideTests(t *testing.T, skipped ...string) []string {
	t.Helper()
	skipped = append(append(skipped, ignoredDirs(t)...), ".git")
	var paths []string
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir() && slices.Contains(skipped, path):
			return fs.SkipDir
		case !entry.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go"):
			paths = append(paths, filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}
