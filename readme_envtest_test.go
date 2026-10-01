//go:build envtest

package botbox_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// A reader runs the command in a fresh clone and sees what the README shows, up
// to the instants and directory names a run stamps.
func TestTheREADMEsFirstFindRunsAsShown(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("The command needs make: %v", err)
	}
	blocks := fencedBlocks(section(t, readFile(t, "README.md"), "## A find in half a minute"))
	if len(blocks) != 2 {
		t.Fatalf("The section holds %d fenced blocks, not the command and what it prints.", len(blocks))
	}
	command, shown := blocks[0], blocks[1]
	for _, built := range []string{"bin/botbox", "bin/toy-widget"} {
		if err := os.Remove(built); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}

	earlier := invocations(t)
	t.Cleanup(func() { removeInvocationsSince(t, earlier) })

	cmd := exec.Command("sh", "-e", "-c", command)
	cmd.Env = slices.DeleteFunc(outsideMake(os.Environ()), func(variable string) bool {
		return strings.HasPrefix(variable, "KUBEBUILDER_ASSETS=")
	})
	output, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Errorf("The command returned %v, and a find exits 1:\n%s", err, output)
	}
	if printed := fromLine(unstamped(string(output)), "the deadline is "); printed != unstamped(shown) {
		t.Errorf("botbox printed\n%s\nand the README shows\n%s", printed, shown)
	}
}

// fromLine is output from its first line that starts with prefix, or empty.
func fromLine(output, prefix string) string {
	if _, after, found := strings.Cut("\n"+output, "\n"+prefix); found {
		return prefix + after
	}
	return ""
}

var (
	instant  = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z`)
	dirStamp = regexp.MustCompile(`\d{8}T\d{6}Z`)
)

func unstamped(output string) string {
	return dirStamp.ReplaceAllString(instant.ReplaceAllString(output, "<instant>"), "<stamp>")
}

const defaultOut = "botbox-out"

// invocations are the directories botbox has written under defaultOut.
func invocations(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(defaultOut)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func removeInvocationsSince(t *testing.T, earlier []string) {
	t.Helper()
	for _, name := range invocations(t) {
		if !slices.Contains(earlier, name) {
			if err := os.RemoveAll(filepath.Join(defaultOut, name)); err != nil {
				t.Error(err)
			}
		}
	}
	_ = os.Remove(defaultOut) // Only an empty directory goes.
}
