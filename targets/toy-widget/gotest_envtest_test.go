//go:build envtest

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	goBuild(t, "../..", filepath.Join(dir, "path", "botbox"), "./cmd/botbox")
	goBuild(t, ".", filepath.Join(repo, "bin", "toy-widget"), ".")
	recipe := filepath.Join(dir, "recipe.test")
	if out, err := exec.Command("go", "test", "-c", "-tags", "botbox", "-o", recipe, ".").CombinedOutput(); err != nil {
		t.Fatalf("Compiling the recipe failed: %v\n%s", err, out)
	}
	runRecipe := func() (string, error) {
		cmd := exec.Command(recipe, "-test.run", "^TestBotbox$", "-test.v")
		cmd.Dir = toy
		cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "path")+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("passes the toy with no bug", func(t *testing.T) {
		out, err := runRecipe()
		if err != nil || !strings.Contains(out, "--- PASS: TestBotbox") {
			t.Fatalf("The recipe returned %v, and must pass:\n%s", err, out)
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

		out, err := runRecipe()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !strings.Contains(out, "--- FAIL: TestBotbox") {
			t.Fatalf("The recipe returned %v, and must fail:\n%s", err, out)
		}
		if !strings.Contains(out, "run 1: G4 ") {
			t.Errorf("The failing test does not show botbox's G4:\n%s", out)
		}
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
