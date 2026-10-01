//go:build envtest

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The README embeds botbox_test.go for adopters to copy. This runs it from a
// copy of the repository's layout, so that it builds nothing into the checkout.
func TestTheGoTestRecipe(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	toy := filepath.Join(repo, "targets", "toy-widget")
	if err := os.CopyFS(toy, os.DirFS(".")); err != nil {
		t.Fatal(err)
	}
	goBuild(t, "../..", filepath.Join(repo, "bin", "botbox"), "./cmd/botbox")
	controller := filepath.Join(repo, "bin", "toy-widget")
	goBuild(t, ".", controller, ".")
	recipe := filepath.Join(dir, "recipe.test")
	if out, err := exec.Command("go", "test", "-c", "-tags", "botbox", "-o", recipe, ".").CombinedOutput(); err != nil {
		t.Fatalf("Compiling the recipe failed: %v\n%s", err, out)
	}
	runRecipe := func(timeout string) (string, error) {
		cmd := exec.Command(recipe, "-test.run", "^TestBotbox$", "-test.v", "-test.timeout", timeout)
		cmd.Dir = toy
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	fails := func(t *testing.T, timeout string, shown ...string) string {
		t.Helper()
		out, err := runRecipe(timeout)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !strings.Contains(out, "--- FAIL: TestBotbox") {
			t.Fatalf("The recipe returned %v, and must fail:\n%s", err, out)
		}
		for _, want := range shown {
			if !strings.Contains(out, want) {
				t.Errorf("The failing test does not show %q:\n%s", want, out)
			}
		}
		return out
	}

	t.Run("passes the toy with no bug", func(t *testing.T) {
		out, err := runRecipe("10m")
		if err != nil || !strings.Contains(out, "--- PASS: TestBotbox") {
			t.Fatalf("The recipe returned %v, and must pass:\n%s", err, out)
		}
	})

	// The three runs take longer than the timeout leaves botbox.
	t.Run("stops botbox before go test's timeout", func(t *testing.T) {
		out := fails(t, "40s", "botbox: exit status 2")
		if !regexp.MustCompile(`the --deadline of \d+s ended`).MatchString(out) {
			t.Errorf("botbox names no deadline in whole seconds:\n%s", out)
		}
	})

	t.Run("fails when go test's timeout leaves botbox no time", func(t *testing.T) {
		if out := fails(t, "31s", "leaves botbox no time"); strings.Contains(out, "botbox: exit status") {
			t.Errorf("The recipe ran botbox:\n%s", out)
		}
	})

	t.Run("fails B4 and shows what botbox found", func(t *testing.T) {
		declared := filepath.Join(toy, "target.yaml")
		yaml, err := os.ReadFile(declared)
		if err != nil {
			t.Fatal(err)
		}
		buggy := strings.Replace(string(yaml), "--bug=0", "--bug=4", 1)
		if buggy == string(yaml) {
			t.Fatal("target.yaml launches the toy with no --bug=0 to replace.")
		}
		if err := os.WriteFile(declared, []byte(buggy), 0o644); err != nil {
			t.Fatal(err)
		}
		fails(t, "10m", "run 1: G4 ", "botbox: exit status 1")
	})

	t.Run("fails a controller botbox cannot launch, and shows why", func(t *testing.T) {
		if err := os.Remove(controller); err != nil {
			t.Fatal(err)
		}
		fails(t, "10m", "launch.binary", "botbox: exit status 2")
	})
}

func goBuild(t *testing.T, dir, binary, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", binary, pkg)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Building %s failed: %v\n%s", pkg, err, out)
	}
}
