package run

import (
	"os"
	"path/filepath"
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
