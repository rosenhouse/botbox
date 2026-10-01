package botbox_test

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const toolsRecipe = "examples/tools-module.sh"

func TestTheReadmeSaysWhereTheToolsModuleRecipePinsBotbox(t *testing.T) {
	script := readFile(t, toolsRecipe)
	lines := strings.Split(strings.TrimSpace(script), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "bin/botbox ") {
		t.Errorf("%s ends with %q, and README.md says its last line runs bin/botbox.", toolsRecipe, last)
	}
	dir := toolsModuleDir(t, script)
	keep := oneLine(section(t, readFile(t, "README.md"), "### Keep botbox out of your go.mod"))
	for _, says := range []string{"`" + dir + "/go.mod` then pins botbox", "not `go -C " + dir + " tool botbox`", "runs botbox in `" + dir + "/`",
		"Your own go.mod and go.work, and any package of yours in `" + path.Dir(dir) + "/`, stay as they were."} {
		if !strings.Contains(keep, says) {
			t.Errorf("README.md does not say %q, and %s pins botbox in %s.", says, toolsRecipe, dir)
		}
	}
}

// toolsModuleDir is the directory where script builds bin/botbox.
func toolsModuleDir(t *testing.T, script string) string {
	t.Helper()
	dir := regexp.MustCompile(`(?m)^(?:\S+=\S+ )*go -C (\S+) build `).FindStringSubmatch(script)
	if dir == nil {
		t.Fatalf("%s builds bin/botbox with no go -C <dir> build.", toolsRecipe)
	}
	return dir[1]
}

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

func TestVariablesChangedByFindsEachWayToChangeOne(t *testing.T) {
	t.Setenv("GOWORK", "off") // The script's shell must not inherit it.
	for set, want := range map[string]string{"GOWORK=off; export GOWORK": "GOWORK", "  export GOWORK=off": "GOWORK",
		"set -a; GOWORK=off; set +a": "GOWORK", "GOWORK=$(echo off)": "GOWORK", "GOWORK=off; mkdir -p tools/botbox": "GOWORK",
		"unset GOWORK": "GOWORK", "PATH=$PATH:/x": "PATH", "GOTOOLCHAIN=auto": "GOTOOLCHAIN"} {
		if changed, err := variablesChangedBy(t, set+"\n"+readFile(t, toolsRecipe)); err != nil || !slices.Equal(changed, []string{want}) {
			t.Errorf("variablesChangedBy found %q and %v where the recipe begins %q, want %s.", changed, err, set, want)
		}
	}
	if _, err := variablesChangedBy(t, "false\n"+readFile(t, toolsRecipe)); err == nil {
		t.Error("variablesChangedBy returned no error where the recipe fails.")
	}
}

// exportedValue is a variable that export -p lists, and its value.
var exportedValue = regexp.MustCompile(`(?m)^export (\w+)=(.*)$`)

// variablesChangedBy runs script, with go and bin/botbox stubbed, in a shell
// that exports PATH and GOWORK alone. It names each variable the script sets,
// changes or unsets there but OLDPWD, which cd sets. Under set -a the shell
// exports each variable the script sets, so export -p lists it.
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
	cmd.Dir, cmd.Env = work, []string{"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GOWORK=" + filepath.Join(work, "go.work")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("the script returned %v:\n%s", err, out)
	}
	before, after, _ := strings.Cut(string(out), "\n====\n")
	was, is := exportedValues(before), exportedValues(after)
	names := maps.Clone(was)
	maps.Copy(names, is)
	var changed []string
	for name := range names {
		if was[name] != is[name] && name != "OLDPWD" {
			changed = append(changed, name)
		}
	}
	return changed, nil
}

// exportedValues maps each exportedValue in exports to its value.
func exportedValues(exports string) map[string]string {
	values := map[string]string{}
	for _, variable := range exportedValue.FindAllStringSubmatch(exports, -1) {
		values[variable[1]] = variable[2]
	}
	return values
}
