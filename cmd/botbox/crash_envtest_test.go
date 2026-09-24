//go:build envtest

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A controller that crashes on a spec value is a finding, even where it wrote
// its converged state first. botbox restarts it, it never converges, and the
// report quotes why it stopped.
func TestACrashLoopIsAFindingWithAReport(t *testing.T) {
	targetFile, err := filepath.Abs(toyTargetYAML)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := filepath.Abs("../../targets/toy-widget/sequences/b12.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "bin", "toy-widget"), "./targets/toy-widget")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("Building the toy target failed: %v\n%s", err, out)
	}
	t.Chdir(dir) // launch.binary is relative to the working directory.
	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr, open: openSession, newGenerator: rapidGenerator}

	code := c.main(t.Context(), []string{"replay", "--target", targetFile, "--out", "out", "--launch-arg", "--bug=12", sequence})

	if code != exitViolation {
		t.Fatalf("botbox exited %d, want %d.\nstdout:\n%s\nstderr:\n%s", code, exitViolation, &stdout, &stderr)
	}
	const exited = `the target exited during op 1 (update) with exit status 2 after writing "panic: runtime error: integer divide by zero`
	for _, want := range []string{"G4 the settle wait after op ", exited} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("botbox printed\n%s\nwhich does not say %q.", &stdout, want)
		}
	}
	reports, err := filepath.Glob(filepath.Join("out", "*", "run-1", "report.md"))
	if err != nil || len(reports) != 1 {
		t.Fatalf("botbox wrote the reports %v, want one for run 1: %v", reports, err)
	}
	report, err := os.ReadFile(reports[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := `last with exit status 2 after writing "panic: runtime error: integer divide by zero`; !strings.Contains(string(report), want) {
		t.Errorf("The report does not say %q:\n%s", want, report)
	}
}
