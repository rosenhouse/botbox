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
		`: "${GOFLAGS:=-mod=mod}"`: "GOFLAGS", "GOTOOLCHAIN=${GOTOOLCHAIN:-auto}": "GOTOOLCHAIN", "export GOFLAGS": "GOFLAGS", "unset GO111MODULE": "GO111MODULE",
		`: "${GOFLAGS=}"`: "GOFLAGS"} {
		if changed, err := variablesChangedBy(t, set+"\n"+readFile(t, toolsRecipe)); err != nil || !slices.Equal(changed, []string{want}) {
			t.Errorf("variablesChangedBy found %q and %v where the recipe begins %q, want %s.", changed, err, set, want)
		}
	}
	if changed, err := variablesChangedBy(t, readFile(t, toolsRecipe)+"cd tools/botbox\n"); err != nil || !slices.Equal(changed, []string{"PWD"}) {
		t.Errorf("variablesChangedBy found %q and %v where the recipe ends in another directory, want PWD.", changed, err)
	}
	for _, line := range []string{`: "$PWD"`, "# Run this from your repository root && with go on PATH.", "  # Or || not."} {
		if changed, err := variablesChangedBy(t, line+"\n"+readFile(t, toolsRecipe)); err != nil || len(changed) > 0 {
			t.Errorf("variablesChangedBy found %q and %v where the recipe begins %q, want none.", changed, err, line)
		}
	}
	if _, err := variablesChangedBy(t, "false\n"+readFile(t, toolsRecipe)); err == nil {
		t.Error("variablesChangedBy returned no error where the recipe fails.")
	}
}

// A line that runs only sometimes could change a variable where the check
// never runs it.
func TestVariablesChangedByRefusesALineThatRunsOnlySometimes(t *testing.T) {
	for _, line := range []string{`[ -z "$GOWORK" ] || unset GOWORK`, "true && GOWORK=off", "  if true; then GOWORK=off; fi",
		"case x in x) GOWORK=off ;; esac", "while false; do :; done", "until true; do :; done", "for x in y; do :; done"} {
		if _, err := variablesChangedBy(t, line+"\n"+readFile(t, toolsRecipe)); err == nil {
			t.Errorf("variablesChangedBy returned no error where the recipe begins %q.", line)
		}
	}
	for _, line := range []string{"true && GOWORK=off", "mkdir -p tools; if true; then GOWORK=off; fi"} {
		if _, err := variablesChangedBy(t, readFile(t, toolsRecipe)+line+"\n"); err == nil {
			t.Errorf("variablesChangedBy returned no error where the recipe ends %q.", line)
		}
	}
}

// A contributor's sh may be bash, which sets variables of its own as it runs.
func TestVariablesChangedByWorksWhereShIsBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("This machine has no bash.")
	}
	shells := t.TempDir()
	if err := os.Symlink(bash, filepath.Join(shells, "sh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shells+string(os.PathListSeparator)+os.Getenv("PATH"))
	if changed, err := variablesChangedBy(t, readFile(t, toolsRecipe)); err != nil || len(changed) > 0 {
		t.Errorf("variablesChangedBy found %q and %v under bash, want none.", changed, err)
	}
	if changed, err := variablesChangedBy(t, "unset GOFLAGS\n"+readFile(t, toolsRecipe)); err != nil || !slices.Equal(changed, []string{"GOFLAGS"}) {
		t.Errorf("variablesChangedBy found %q and %v under bash where the recipe unsets GOFLAGS, want GOFLAGS.", changed, err)
	}
}

// word could name a shell variable.
var word = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\b`)

// runsSometimes is a line that may skip a command: a list joined by || or &&,
// or a command that begins with if, case, while, until or for.
var runsSometimes = regexp.MustCompile(`\|\||&&|(?:^|;)\s*(?:if|case|while|until|for)\b`)

// exportedName is a variable that export -p lists.
var exportedName = regexp.MustCompile(`(?m)^export (\w+)`)

// variablesChangedBy runs script, with go and bin/botbox stubbed, and returns
// each variable it sets, changes, exports or unsets. A script names each
// variable it changes, but cd changes PWD and OLDPWD, so the check watches
// PATH, PWD and each word of the script. It runs the script twice: in a shell
// that holds an unexported value for each other word, and in one that holds
// none. Each line but a comment must run every time.
func variablesChangedBy(t *testing.T, script string) ([]string, error) {
	t.Helper()
	for _, line := range strings.Split(script, "\n") {
		if runsSometimes.MatchString(line) && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			return nil, fmt.Errorf("this line of the script may not run, so the check cannot see what it changes: %q", line)
		}
	}
	names, held := []string{"PATH", "PWD"}, []string{}
	for _, name := range word.FindAllString(script, -1) {
		if !slices.Contains(names, name) {
			names = append(names, name)
			held = append(held, name+"=seed")
		}
	}
	changed := map[string]bool{}
	for _, assignments := range []string{strings.Join(held, "\n"), ""} {
		before, after, err := shellStates(t, assignments, script, names)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if before[name] != after[name] {
				changed[name] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(changed)), nil
}

// shellStates runs script after assignments in a shell, and maps each of names
// before and after script to whether it is set, its value, and whether it is
// exported.
func shellStates(t *testing.T, assignments, script string, names []string) (before, after map[string]string, err error) {
	t.Helper()
	work, stubs := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, filepath.Join(work, "bin", "botbox"), "")
	writeStub(t, filepath.Join(stubs, "go"), "")
	writeFile(t, filepath.Join(stubs, "script.sh"), script)
	probe := "export -p\nprintf '\\0'\n"
	for _, name := range names {
		probe += fmt.Sprintf("printf '%%s %%s%%s\\0' %[1]s \"${%[1]s+=}\" \"${%[1]s-}\"\n", name)
	}
	probe += "printf '====\\0'\n"
	cmd := exec.Command("sh", "-e", "-c", assignments+"\n"+probe+". \"$0\"\n"+probe, filepath.Join(stubs, "script.sh"))
	cmd.Dir, cmd.Env = work, []string{"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, nil, fmt.Errorf("the script returned %v:\n%s", err, out)
	}
	states := strings.Split(string(out), "====\x00")
	return shellState(states[0]), shellState(states[1]), nil
}

// shellState maps each variable that a probe prints to whether it is set, its
// value, and whether it is exported.
func shellState(probed string) map[string]string {
	exports, variables, _ := strings.Cut(probed, "\x00")
	state := map[string]string{}
	for _, variable := range strings.Split(strings.TrimSuffix(variables, "\x00"), "\x00") {
		name, value, _ := strings.Cut(variable, " ")
		state[name] = value
	}
	for _, exported := range exportedName.FindAllStringSubmatch(exports, -1) {
		state[exported[1]] += " exported"
	}
	return state
}
