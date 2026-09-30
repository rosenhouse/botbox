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
	code, stdout, stderr := replayB12(t, "b12.json")

	if code != exitViolation {
		t.Fatalf("botbox exited %d, want %d.\nstdout:\n%s\nstderr:\n%s", code, exitViolation, stdout, stderr)
	}
	const exited = `the target exited during op 1 (update) with exit status 2 after writing "panic: runtime error: integer divide by zero`
	for _, want := range []string{"G4 the settle wait after op ", exited} {
		if !strings.Contains(stdout, want) {
			t.Errorf("botbox printed\n%s\nwhich does not say %q.", stdout, want)
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
	ran := readSummary(t, "out").Runs[0]
	if len(ran.Exits) == 0 {
		t.Fatalf("The summary lists run 1 as %+v, with none of the target's exits.", ran)
	}
	for _, exit := range ran.Exits {
		if exit.Error != "exit status 2" || !strings.HasPrefix(exit.Said, "panic: runtime error: integer divide by zero") {
			t.Errorf("The summary lists an exit as %+v, want the toy's panic.", exit)
		}
	}
}

// A fault excuses every exit while it is active, and the fault here never
// stops. The crash loop still fails G4 once the teardown clears the fault.
func TestACrashLoopUnderAFaultThatNeverStopsIsAFinding(t *testing.T) {
	code, stdout, stderr := replayB12(t, "b12-fault.json", "--deadline", "3m")

	if code != exitViolation {
		t.Fatalf("botbox exited %d, want %d.\nstdout:\n%s\nstderr:\n%s", code, exitViolation, stdout, stderr)
	}
	if want := "G4 the settle wait after the last fault stopped expired with no fault active"; !strings.Contains(stdout, want) {
		t.Errorf("botbox printed\n%s\nwhich does not say %q.", stdout, want)
	}
}

// replayB12 replays the toy's sequence of that name under B12, from a
// directory of its own, and returns what botbox exited with and printed.
func replayB12(t *testing.T, sequence string, args ...string) (int, string, string) {
	t.Helper()
	targetFile, err := filepath.Abs(toyTargetYAML)
	if err != nil {
		t.Fatal(err)
	}
	path, err := filepath.Abs(filepath.Join("../../targets/toy-widget/sequences", sequence))
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
	c := newCLI(&stdout, &stderr)
	args = append([]string{"replay", "--target", targetFile, "--out", "out", "--launch-arg", "--bug=12"}, args...)

	code := c.main(t.Context(), append(args, path))

	return code, stdout.String(), stderr.String()
}
