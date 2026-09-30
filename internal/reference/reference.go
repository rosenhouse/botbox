// Package reference reads the tables of docs/reference.md, for the tests that
// hold the page to the code.
package reference

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Path is docs/reference.md.
var Path = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "docs", "reference.md")
}()

// Keys lists the first cell of each row of the table under heading, where that
// cell is a code span.
func Keys(t testing.TB, heading string) []string {
	t.Helper()
	var keys []string
	for _, row := range rows(t, heading) {
		keys = append(keys, row[0])
	}
	return keys
}

// Row is the cells after the first of the row under heading whose first cell
// is key.
func Row(t testing.TB, heading, key string) []string {
	t.Helper()
	for _, row := range rows(t, heading) {
		if row[0] == key {
			return row[1:]
		}
	}
	t.Fatalf("%s has no row for %s under %q.", Path, key, heading)
	return nil
}

// Spans lists the code spans in a cell.
func Spans(cell string) []string {
	var spans []string
	for i, part := range strings.Split(cell, "`") {
		if i%2 == 1 {
			spans = append(spans, part)
		}
	}
	return spans
}

// rows are the cells of each row under heading, up to the next heading, whose
// first cell is a code span. That cell is given without its backquotes.
func rows(t testing.TB, heading string) [][]string {
	t.Helper()
	page, err := os.ReadFile(Path)
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(page), "\n"+heading+"\n")
	if !found {
		t.Fatalf("%s has no heading %q.", Path, heading)
	}
	var rows [][]string
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
			break
		}
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		cells[0] = strings.Trim(cells[0], "`")
		rows = append(rows, cells)
	}
	return rows
}
