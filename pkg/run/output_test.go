package run

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOutputNamesTheInvocationByTimestampAndSeed(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 9, 20, 18, 45, 30, 0, time.UTC)

	out, err := OpenOutput(root, 8675309, at)
	if err != nil {
		t.Fatalf("OpenOutput failed: %v", err)
	}

	if want := filepath.Join(root, "20260920T184530Z-8675309"); out.Dir() != want {
		t.Errorf("The invocation wrote to %s, want %s.", out.Dir(), want)
	}
	if _, err := os.Stat(out.Dir()); err != nil {
		t.Errorf("The invocation directory is missing: %v", err)
	}
}

// Two invocations of the same seed in the same second would otherwise write
// into one directory, and each would tell its reader the other's evidence is
// theirs (DESIGN.md §11).
func TestOutputNeverSharesADirectoryWithAnotherInvocation(t *testing.T) {
	// The root is what --out names, which a first run creates.
	root := filepath.Join(t.TempDir(), "botbox-out")
	at := time.Date(2026, 9, 20, 18, 45, 30, 0, time.UTC)

	first, err := OpenOutput(root, 1, at)
	if err != nil {
		t.Fatalf("OpenOutput failed: %v", err)
	}
	second, err := OpenOutput(root, 1, at)
	if err != nil {
		t.Fatalf("The second OpenOutput failed: %v", err)
	}

	if first.Dir() == second.Dir() {
		t.Fatalf("Both invocations wrote to %s.", first.Dir())
	}
	for _, dir := range []string{first.Dir(), second.Dir()} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("The invocation directory is missing: %v", err)
		}
	}
}

func TestWriteAtomicReplacesTheFileWhole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "summary.json")
	if err := WriteAtomic(path, []byte("old\n")); err != nil {
		t.Fatalf("WriteAtomic failed: %v", err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	if err := WriteAtomic(path, []byte("new\n")); err != nil {
		t.Fatalf("The second WriteAtomic failed: %v", err)
	}

	if held, _ := io.ReadAll(reader); string(held) != "old\n" {
		t.Errorf("A reader that opened the file before the write read %q, want the old file whole.", held)
	}
	if now, _ := os.ReadFile(path); string(now) != "new\n" {
		t.Errorf("The file holds %q, want the new content.", now)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("The file's mode is %v (%v), want -rw-r--r--.", info.Mode(), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("The directory holds %v, want the file alone.", entries)
	}
}

func TestAFailedWriteAtomicLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	// A directory in the way fails the rename, after the content is written.
	taken := filepath.Join(dir, "summary.json")
	if err := os.MkdirAll(filepath.Join(taken, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := WriteAtomic(taken, []byte("new\n"))

	if err == nil || !strings.Contains(err.Error(), taken) {
		t.Errorf("WriteAtomic returned %v, want an error naming %s.", err, taken)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("The directory holds %v, want only what was there.", entries)
	}
}

func TestOutputKeepsAFailingRunAndDiscardsAPassingOne(t *testing.T) {
	out, err := OpenOutput(t.TempDir(), 1, time.Now())
	if err != nil {
		t.Fatalf("OpenOutput failed: %v", err)
	}

	kept, discarded := out.RunDir(1), out.RunDir(2)
	for _, dir := range []string{kept, discarded} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("Creating the run directory failed: %v", err)
		}
	}
	if err := out.Discard(2); err != nil {
		t.Fatalf("Discarding the passing run failed: %v", err)
	}

	if want := filepath.Join(out.Dir(), "run-1"); kept != want {
		t.Errorf("Run 1 wrote to %s, want %s.", kept, want)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("The failing run's directory is missing: %v", err)
	}
	if _, err := os.Stat(discarded); !os.IsNotExist(err) {
		t.Errorf("The passing run's directory is still there: %v", err)
	}
}
