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

// A reader who followed Install has botbox on PATH and KUBEBUILDER_ASSETS
// set. Each command of the first run and the first find then prints what the
// README shows, up to the instants and directory names a run stamps.
func TestTheREADMEsFirstRunAndFirstFindRunAsShown(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("The test installs the control plane with make: %v", err)
	}
	blocks := fencedBlocks(section(t, readFile(t, "README.md"), "## A first run and a first find"))
	if len(blocks) != 4 {
		t.Fatalf("The section holds %d fenced blocks, not two commands, each with what it prints.", len(blocks))
	}
	env := installed(t)
	if err := os.Remove("bin/toy-widget"); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	earlier := invocations(t)
	t.Cleanup(func() { removeInvocationsSince(t, earlier) })

	for i := 0; i < len(blocks); i += 2 {
		command, shown := blocks[i], blocks[i+1]
		cmd := exec.Command("sh", "-e", "-c", command)
		cmd.Env = env
		output, err := cmd.CombinedOutput()

		want := 0
		if strings.Contains(shown, "the evidence is in ") {
			want = 1 // A find exits 1.
		}
		code := 0
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit):
			code = exit.ExitCode()
		case err != nil:
			t.Fatalf("%q did not run: %v", command, err)
		}
		if code != want {
			t.Errorf("%q exited %d, and the output the README shows exits %d:\n%s", command, code, want, output)
		}
		if printed := fromLine(unstamped(string(output)), "the deadline is "); printed != unstamped(shown) {
			t.Errorf("%q printed\n%s\nand the README shows\n%s", command, printed, shown)
		}
	}
}

// installed is the environment Install leaves: botbox, built from this
// checkout, first on PATH, and KUBEBUILDER_ASSETS naming the pinned control
// plane.
func installed(t *testing.T) []string {
	t.Helper()
	bin := t.TempDir()
	if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, "botbox"), "./cmd/botbox").CombinedOutput(); err != nil {
		t.Fatalf("Building botbox failed: %v\n%s", err, out)
	}
	assets := exec.Command("make", "--no-print-directory", "assets-path")
	assets.Env = outsideMake(os.Environ())
	path, err := assets.Output()
	if err != nil {
		t.Fatalf("make assets-path failed: %v", err)
	}
	env := slices.DeleteFunc(outsideMake(os.Environ()), func(variable string) bool {
		return strings.HasPrefix(variable, "KUBEBUILDER_ASSETS=") || strings.HasPrefix(variable, "PATH=")
	})
	return append(env, "KUBEBUILDER_ASSETS="+strings.TrimSpace(string(path)),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
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
