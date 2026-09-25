//go:build envtest

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestAMatrixRunThatErredKeepsTheLogItsErrorNames(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	targetFile, err := filepath.Abs(toyTargetYAML)
	if err != nil {
		t.Fatal(err)
	}
	sequences, err := filepath.Abs("../../targets/toy-widget/sequences")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	buildToyInto(t, "../../targets/toy-widget", filepath.Join(dir, "bin", "toy-widget"))
	t.Chdir(dir) // launch.binary is relative to the working directory.
	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr, open: openSession, newGenerator: rapidGenerator}

	code := c.main(t.Context(), []string{"matrix", "--target", targetFile, "--sequences", sequences,
		"--out", "bug-matrix.md", "--launch-arg", "--no-such-flag"})

	if code != exitError {
		t.Fatalf("botbox exited %d, want %d.\nstdout:\n%s\nstderr:\n%s", code, exitError, &stdout, &stderr)
	}
	named := regexp.MustCompile(`output is in (\S+)`).FindStringSubmatch(stderr.String())
	if named == nil {
		t.Fatalf("botbox reported\n%s\nwhich names no target.log.", &stderr)
	}
	logged, err := os.ReadFile(named[1])
	if err != nil {
		t.Fatalf("botbox named %s, which it did not keep: %v", named[1], err)
	}
	if !strings.Contains(string(logged), "no-such-flag") {
		t.Errorf("%s holds %q, want the toy's complaint about its flag.", named[1], logged)
	}
	if want := "the run's files are in " + filepath.Dir(named[1]); !strings.Contains(stderr.String(), want) {
		t.Errorf("botbox reported\n%s\nwant %q.", &stderr, want)
	}
}
