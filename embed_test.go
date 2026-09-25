// Checks that span the whole repository rather than any one package. This one
// enforces the embed marker of DESIGN.md §11 over every Markdown file.
package botbox_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// embedMarker introduces a fenced block whose content, excluding the two fence
// lines, is the named file byte for byte.
const embedMarker = "<!-- embed:"

func TestMarkdownEmbedsMatchTheirFiles(t *testing.T) {
	files := markdownFiles(t)
	for _, wanted := range []string{"README.md", "docs/journal.md"} {
		if !slices.Contains(files, wanted) {
			t.Errorf("the listing skipped %s, so the scan is not repository-wide", wanted)
		}
	}
	for _, path := range files {
		doc, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, difference := range embedDifferences(string(doc)) {
			t.Errorf("%s:%s", path, difference)
		}
	}
}

// The README shows the files an adopter copies, and so cannot drift from them.
func TestREADMEEmbedsWhatAnAdopterCopies(t *testing.T) {
	doc := readFile(t, "README.md")
	for _, path := range []string{"examples/cert-manager/quickstart.sh", "examples/ci/github-actions.yml"} {
		if marker := embedMarker + " " + path + " -->"; !strings.Contains(doc, marker) {
			t.Errorf("README.md does not embed %s: no %q", path, marker)
		}
	}
}

func TestEmbedDifferences(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("hello.txt", []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		doc  string
		want string // A substring of the one expected difference; empty expects none.
	}{{
		name: "a matching block",
		doc:  "<!-- embed: hello.txt -->\n```sh\nhello\nworld\n```\n",
	}, {
		name: "a differing block",
		doc:  "text\n\n<!-- embed: hello.txt -->\n```sh\nhello\nthere\n```\n",
		want: `3: hello.txt: line 2 differs: the block has "there\n" and the file has "world\n"`,
	}, {
		name: "a block that stops short",
		doc:  "<!-- embed: hello.txt -->\n```sh\nhello\n```\n",
		want: `line 2 differs: the block has "" and the file has "world\n"`,
	}, {
		name: "a marker with no fenced block after it",
		doc:  "<!-- embed: hello.txt -->\nhello\nworld\n",
		want: "no closed fenced block follows the marker",
	}, {
		name: "a block the fence never closes",
		doc:  "<!-- embed: hello.txt -->\n```sh\nhello\nworld\n",
		want: "no closed fenced block follows the marker",
	}, {
		name: "a marker that does not close",
		doc:  "<!-- embed: hello.txt\n```sh\nhello\nworld\n```\n",
		want: "the marker does not end with -->",
	}, {
		name: "a marker naming a file that does not exist",
		doc:  "<!-- embed: gone.txt -->\n```sh\nhello\n```\n",
		want: "gone.txt: open gone.txt",
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			differences := embedDifferences(test.doc)
			if test.want == "" {
				if len(differences) > 0 {
					t.Fatalf("expected no difference, and got %q", differences)
				}
				return
			}
			if len(differences) != 1 || !strings.Contains(differences[0], test.want) {
				t.Fatalf("expected one difference containing %q, and got %q", test.want, differences)
			}
		})
	}
}

// embedDifferences reports how each marked fenced block in doc differs from the
// file it names. A path is relative to the working directory, which for the
// repository scan is the root.
func embedDifferences(doc string) []string {
	var differences []string
	lines := strings.SplitAfter(doc, "\n")
	for i, line := range lines {
		rest, marked := strings.CutPrefix(strings.TrimSpace(line), embedMarker)
		if !marked {
			continue
		}
		path, closed := strings.CutSuffix(rest, "-->")
		path = strings.TrimSpace(path)
		where := fmt.Sprintf("%d: %s: ", i+1, path)
		if !closed {
			differences = append(differences, where+"the marker does not end with -->")
			continue
		}
		block, fenced := fencedBlock(lines[i+1:])
		if !fenced {
			differences = append(differences, where+"no closed fenced block follows the marker")
			continue
		}
		file, err := os.ReadFile(path)
		if err != nil {
			differences = append(differences, where+err.Error())
			continue
		}
		if difference := firstDifference(block, string(file)); difference != "" {
			differences = append(differences, where+difference)
		}
	}
	return differences
}

// fencedBlock returns the content between the fences of the block opening on
// the first non-blank line, with every line ending intact.
func fencedBlock(lines []string) (string, bool) {
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fence := openingFence(line)
		if fence == "" {
			return "", false
		}
		content := lines[i+1:]
		end := slices.IndexFunc(content, func(line string) bool { return closesFence(line, fence) })
		if end < 0 {
			return "", false
		}
		return strings.Join(content[:end], ""), true
	}
	return "", false
}

// openingFence returns the run of backticks a fenced block opens with, or the
// empty string when the line does not open one.
func openingFence(line string) string {
	backticks := len(line) - len(strings.TrimLeft(line, "`"))
	if backticks < 3 {
		return ""
	}
	return line[:backticks]
}

func closesFence(line, fence string) bool {
	closing := strings.TrimSpace(line)
	return len(closing) >= len(fence) && strings.Trim(closing, "`") == ""
}

// firstDifference describes the first line on which a block and a file
// disagree, and is empty when they are identical.
func firstDifference(block, file string) string {
	blockLines, fileLines := strings.SplitAfter(block, "\n"), strings.SplitAfter(file, "\n")
	for i := range max(len(blockLines), len(fileLines)) {
		blockLine, fileLine := lineAt(blockLines, i), lineAt(fileLines, i)
		if blockLine != fileLine {
			return fmt.Sprintf("line %d differs: the block has %q and the file has %q", i+1, blockLine, fileLine)
		}
	}
	return ""
}

func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}

// markdownFiles lists the repository's Markdown. It walks the tree rather than
// asking git, because an export of a checkout has no git in it, and it skips
// the directories .gitignore names, which keeps the cert-manager clone under
// bin/ out of the scan.
func markdownFiles(t *testing.T) []string {
	t.Helper()
	skipped := append(ignoredDirs(t), ".git")
	var paths []string
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir() && slices.Contains(skipped, path):
			return fs.SkipDir
		case !entry.IsDir() && filepath.Ext(path) == ".md":
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("the repository holds no Markdown, so this check would pass vacuously")
	}
	return paths
}

// ignoredDirs are the directories .gitignore names, such as bin/.
func ignoredDirs(t *testing.T) []string {
	t.Helper()
	ignore, err := os.ReadFile(".gitignore")
	if err != nil {
		t.Fatalf("reading which directories to skip: %v", err)
	}
	var dirs []string
	for _, line := range strings.Split(string(ignore), "\n") {
		line = strings.TrimSpace(line)
		if dir, isDir := strings.CutSuffix(line, "/"); isDir && !strings.HasPrefix(line, "#") {
			dirs = append(dirs, strings.TrimPrefix(dir, "/"))
		}
	}
	return dirs
}

func TestMarkdownFilesSkipsWhatGitIgnores(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFile(t, ".gitignore", "# a comment\n/build/\nvendor/\n*.out\n")
	for _, path := range []string{"kept.md", "build/ignored.md", "vendor/ignored.md", ".git/ignored.md"} {
		writeFile(t, path, "")
	}

	if listed := markdownFiles(t); !slices.Equal(listed, []string{"kept.md"}) {
		t.Errorf("markdownFiles listed %q; the walk keeps to the repository's own Markdown.", listed)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
