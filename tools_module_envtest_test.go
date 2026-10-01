//go:build envtest

package botbox_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const toolsRecipe = "examples/tools-module.sh"

// The README's recipe pins botbox in a module of its own. This runs it in a
// fresh operator module, on the oldest go that the README says fetches a newer
// one.
func TestTheToolsModuleRecipe(t *testing.T) {
	install := section(t, readFile(t, "README.md"), "## Install")
	oldest := regexp.MustCompile(`from Go (1\.\d+) on`).FindStringSubmatch(install)
	remedy := regexp.MustCompile("run the commands with\\s+`GOTOOLCHAIN=(\\w+)`").FindStringSubmatch(install)
	stopped := regexp.MustCompile("tools module below with\\s+`([^`]+)`").FindStringSubmatch(install)
	if oldest == nil || remedy == nil || stopped == nil {
		t.Fatalf("README.md's Install section names no oldest go, no GOTOOLCHAIN to run the commands with, or no error that stops the tools module:\n%s", install)
	}
	oldGo := oldest[1] + ".0"
	lookup := exec.Command("go", "env", "GOROOT")
	lookup.Env = goEnv("", "go"+oldGo)
	goroot, err := lookup.Output()
	if err != nil {
		t.Fatalf("Finding go%s failed: %v", oldGo, err)
	}
	checkout, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	recipe := replacingBotbox(t, readFile(t, toolsRecipe), checkout)
	operatorGoMod := "module example.com/operator\n\ngo " + oldGo + "\n"

	// The operator pins its own tools in tools/tools.go, a common place for them.
	run := func(t *testing.T, gotoolchain string) (operator string, env []string, output string, err error) {
		operator = t.TempDir()
		writeFile(t, filepath.Join(operator, "go.mod"), operatorGoMod)
		writeFile(t, filepath.Join(operator, "go.sum"), "")
		writeFile(t, filepath.Join(operator, "tools", "tools.go"), "//go:build tools\n\npackage tools\n")
		cmd := exec.Command("sh", "-e", "-c", recipe)
		cmd.Dir = operator
		cmd.Env = goEnv(strings.TrimSpace(string(goroot)), gotoolchain)
		out, err := cmd.CombinedOutput()
		return operator, cmd.Env, string(out), err
	}

	t.Run("builds bin/botbox and leaves the operator's module alone", func(t *testing.T) {
		operator, env, out, err := run(t, remedy[1])
		if err != nil {
			t.Fatalf("The recipe returned %v:\n%s", err, out)
		}
		for name, before := range map[string]string{"go.mod": operatorGoMod, "go.sum": ""} {
			if after := readFile(t, filepath.Join(operator, name)); after != before {
				t.Errorf("The recipe changed the operator's %s to:\n%s", name, after)
			}
		}
		list := exec.Command("go", "list", "-tags", "tools", "./tools")
		list.Dir, list.Env = operator, env
		if out, err := list.CombinedOutput(); err != nil || string(out) != "example.com/operator/tools\n" {
			t.Errorf("The recipe took the operator's tools package out of its module: %v\n%s", err, out)
		}
		version, err := exec.Command(filepath.Join(operator, "bin", "botbox"), "version").CombinedOutput()
		if err != nil {
			t.Errorf("The recipe built no bin/botbox: %v\n%s", err, version)
		} else if !slices.Contains(strings.Split(out, "\n"), strings.TrimSuffix(string(version), "\n")) {
			t.Errorf("The recipe does not run bin/botbox, which prints %q:\n%s", version, out)
		}
		tool := exec.Command("go", "-C", "tools/botbox", "tool", "botbox", "version")
		tool.Dir, tool.Env = operator, env
		if out, err := tool.CombinedOutput(); err != nil {
			t.Errorf("tools/botbox/go.mod does not pin botbox as a tool: %v\n%s", err, out)
		}
	})

	t.Run("fails under GOTOOLCHAIN=local as the README says", func(t *testing.T) {
		_, _, out, err := run(t, "local")
		if err == nil || !strings.Contains(out, stopped[1]) {
			t.Errorf("The recipe returned %v, and must fail with %q:\n%s", err, stopped[1], out)
		}
	})
}

// replacingBotbox has the tools module build this checkout's botbox.
func replacingBotbox(t *testing.T, recipe, checkout string) string {
	t.Helper()
	modInit := regexp.MustCompile(`(?m)^go mod init .*\n`)
	if n := len(modInit.FindAllString(recipe, -1)); n != 1 {
		t.Fatalf("%s runs go mod init %d times, not once.", toolsRecipe, n)
	}
	return modInit.ReplaceAllStringFunc(recipe, func(line string) string {
		return line + "go mod edit -replace=github.com/rosenhouse/botbox=" + strconv.Quote(checkout) + "\n"
	})
}

// goEnv puts goroot's go first on PATH, when goroot is set, and sets GOTOOLCHAIN.
func goEnv(goroot, gotoolchain string) []string {
	env := slices.DeleteFunc(os.Environ(), func(variable string) bool {
		return strings.HasPrefix(variable, "GOROOT=") || strings.HasPrefix(variable, "GOTOOLCHAIN=")
	})
	if goroot != "" {
		env = append(env, "PATH="+filepath.Join(goroot, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return append(env, "GOTOOLCHAIN="+gotoolchain)
}
