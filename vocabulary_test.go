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
// DESIGN.md to be understood. Nor may the command's doc, which go doc prints.
func TestNoStringLiteralUsesTheDesignsVocabulary(t *testing.T) {
	files := productionGoFiles(t)
	if !slices.Contains(files, "pkg/invariant/g2.go") {
		t.Fatalf("The scan listed %v, which skips the checks.", files)
	}
	for _, path := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(path, "cmd/") && parsed.Doc != nil {
			if found := designVocabulary.FindString(parsed.Doc.Text()); found != "" {
				t.Errorf("%s: the command's doc uses %q, which only DESIGN.md explains.", path, found)
			}
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
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
}

// productionGoFiles lists the root module's Go files outside tests.
func productionGoFiles(t *testing.T) []string {
	t.Helper()
	skipped := append(ignoredDirs(t), ".git", "docs")
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
