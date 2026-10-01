package botbox_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const toolsRecipe = "examples/tools-module.sh"

// A reader may paste the recipe into a shell they go on using.
func TestTheToolsModuleRecipeLeavesItsShellsVariablesAlone(t *testing.T) {
	changed, err := variablesChangedBy(t, readFile(t, toolsRecipe))
	if err != nil {
		t.Fatal(err)
	}
	if changed != nil {
		t.Errorf("%s changes %q in the shell that runs it.", toolsRecipe, changed)
	}
}

func TestVariablesChangedByFindsEachWayToSetOne(t *testing.T) {
	t.Setenv("GOWORK", "off") // The script's shell must not inherit it.
	for _, set := range []string{"GOWORK=off; export GOWORK", "  export GOWORK=off", "set -a; GOWORK=off; set +a", "GOWORK=$(echo off)",
		"GOWORK=off; mkdir -p tools/botbox"} {
		if changed, err := variablesChangedBy(t, set+"\n"+readFile(t, toolsRecipe)); err != nil || len(changed) != 1 || changed[0] != "GOWORK" {
			t.Errorf("variablesChangedBy found %q and %v where the recipe begins %q, want GOWORK.", changed, err, set)
		}
	}
	if _, err := variablesChangedBy(t, "false\n"+readFile(t, toolsRecipe)); err == nil {
		t.Error("variablesChangedBy returned no error where the recipe fails.")
	}
}

// exportedValue is a variable that export -p lists, and its value.
var exportedValue = regexp.MustCompile(`(?m)^export (\w+)=(.*)$`)

// variablesChangedBy runs script in a shell that exports PATH alone, with go
// and bin/botbox stubbed. It names each variable the script leaves new or
// changed there but OLDPWD, which cd sets. Under set -a the shell exports each
// variable the script sets, so export -p lists it.
func variablesChangedBy(t *testing.T, script string) ([]string, error) {
	t.Helper()
	work, stubs := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, filepath.Join(work, "bin", "botbox"), "")
	writeStub(t, filepath.Join(stubs, "go"), "")
	writeFile(t, filepath.Join(stubs, "script.sh"), script)
	cmd := exec.Command("sh", "-e", "-c", `export -p; echo ====; set -a; . "$0"; export -p`, filepath.Join(stubs, "script.sh"))
	cmd.Dir, cmd.Env = work, []string{"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("the script returned %v:\n%s", err, out)
	}
	before, after, _ := strings.Cut(string(out), "\n====\n")
	was := map[string]string{}
	for _, variable := range exportedValue.FindAllStringSubmatch(before, -1) {
		was[variable[1]] = variable[2]
	}
	var changed []string
	for _, variable := range exportedValue.FindAllStringSubmatch(after, -1) {
		if was[variable[1]] != variable[2] && variable[1] != "OLDPWD" {
			changed = append(changed, variable[1])
		}
	}
	return changed, nil
}
