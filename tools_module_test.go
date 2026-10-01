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
	if len(changed) > 0 {
		t.Errorf("%s changes %q in the shell that runs it.", toolsRecipe, changed)
	}
}

func TestVariablesChangedByFindsEachWayToChangeOne(t *testing.T) {
	for set, want := range map[string]string{"GOWORK=off; export GOWORK": "GOWORK", "  export GOWORK=off": "GOWORK",
		"set -a; GOWORK=off; set +a": "GOWORK", "GOWORK=$(echo off)": "GOWORK", "GOWORK=off; mkdir -p tools/botbox": "GOWORK",
		"unset GOWORK": "GOWORK", "PATH=$PATH:/x": "PATH", "GOTOOLCHAIN=auto": "GOTOOLCHAIN", "unset GOFLAGS": "GOFLAGS",
		"GOFLAGS=": "GOFLAGS", "unset gopath": "gopath",
		`: "${GOFLAGS:=-mod=mod}"`: "GOFLAGS", "GOTOOLCHAIN=${GOTOOLCHAIN:-auto}": "GOTOOLCHAIN", "export GOFLAGS": "GOFLAGS", "unset GO111MODULE": "GO111MODULE"} {
		if changed, err := variablesChangedBy(t, set+"\n"+readFile(t, toolsRecipe)); err != nil || !slices.Equal(changed, []string{want}) {
			t.Errorf("variablesChangedBy found %q and %v where the recipe begins %q, want %s.", changed, err, set, want)
		}
	}
	if changed, err := variablesChangedBy(t, `: "$PWD"`+"\n"+readFile(t, toolsRecipe)); err != nil || len(changed) > 0 {
		t.Errorf("variablesChangedBy found %q and %v where the recipe reads PWD, want none.", changed, err)
	}
	if _, err := variablesChangedBy(t, "false\n"+readFile(t, toolsRecipe)); err == nil {
		t.Error("variablesChangedBy returned no error where the recipe fails.")
	}
}

// word could name a shell variable.
var word = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\b`)

// shellVariable is a variable that set lists, and its value.
var shellVariable = regexp.MustCompile(`(?m)^(\w+)=(.*)$`)

// exportedName is a variable that export -p lists.
var exportedName = regexp.MustCompile(`(?m)^export (\w+)`)

// variablesChangedBy runs script, with go and bin/botbox stubbed, and returns
// each variable it sets, changes, exports or unsets, but OLDPWD, which cd sets.
// A script names each variable it changes, so the script runs twice: in a
// shell that holds an unexported value for each word of it, and in one that
// holds none of them.
func variablesChangedBy(t *testing.T, script string) ([]string, error) {
	t.Helper()
	var held []string
	for _, name := range word.FindAllString(script, -1) {
		if name != "PATH" && name != "PWD" {
			held = append(held, name+"=seed")
		}
	}
	changed := map[string]bool{}
	for _, assignments := range []string{strings.Join(held, "\n"), ""} {
		before, after, err := shellStates(t, assignments, script)
		if err != nil {
			return nil, err
		}
		for _, state := range []map[string]string{before, after} {
			for name := range state {
				if before[name] != after[name] && name != "OLDPWD" {
					changed[name] = true
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(changed)), nil
}

// shellStates runs script after assignments in a shell, and maps each variable
// of that shell before and after script to its value and whether it is exported.
func shellStates(t *testing.T, assignments, script string) (before, after map[string]string, err error) {
	t.Helper()
	work, stubs := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, filepath.Join(work, "bin", "botbox"), "")
	writeStub(t, filepath.Join(stubs, "go"), "")
	writeFile(t, filepath.Join(stubs, "script.sh"), script)
	state := "set; echo ----; export -p; echo ===="
	cmd := exec.Command("sh", "-e", "-c", assignments+"\n"+state+"\n. \"$0\"\n"+state, filepath.Join(stubs, "script.sh"))
	cmd.Dir, cmd.Env = work, []string{"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, nil, fmt.Errorf("the script returned %v:\n%s", err, out)
	}
	states := strings.Split(string(out), "====\n")
	return shellState(states[0]), shellState(states[1]), nil
}

// shellState maps each variable that set and export -p list in out to its
// value and whether it is exported.
func shellState(out string) map[string]string {
	variables, exports, _ := strings.Cut(out, "----\n")
	state := map[string]string{}
	for _, variable := range shellVariable.FindAllStringSubmatch(variables, -1) {
		state[variable[1]] = variable[2]
	}
	for _, exported := range exportedName.FindAllStringSubmatch(exports, -1) {
		state[exported[1]] += " exported"
	}
	return state
}
