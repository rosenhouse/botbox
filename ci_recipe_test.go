package botbox_test

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

const ciRecipe = "examples/ci/github-actions.yml"

// workflow holds what these tests read of a GitHub Actions workflow. YAML 1.2
// keeps the key on a string, as Actions does.
type workflow struct {
	On   map[string]any    `yaml:"on"`
	Env  map[string]string `yaml:"env"`
	Jobs map[string]struct {
		Steps []step `yaml:"steps"`
	} `yaml:"jobs"`
}

type step struct {
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

func readWorkflow(t *testing.T, path string) workflow {
	t.Helper()
	var w workflow
	if err := yaml.Unmarshal([]byte(readFile(t, path)), &w); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return w
}

// recipeSteps are the steps of the recipe's one job.
func recipeSteps(t *testing.T) []step {
	t.Helper()
	w := readWorkflow(t, ciRecipe)
	if len(w.Jobs) != 1 {
		t.Fatalf("%s has %d jobs, and an adopter copies one", ciRecipe, len(w.Jobs))
	}
	for _, job := range w.Jobs {
		return job.Steps
	}
	return nil
}

// stepUsing returns the one step that uses the action, at any version.
func stepUsing(t *testing.T, steps []step, action string) step {
	t.Helper()
	var found []step
	for _, s := range steps {
		if strings.HasPrefix(s.Uses, action+"@") {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s has %d steps that use %s, not one", ciRecipe, len(found), action)
	}
	return found[0]
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestTheCIRecipeAndTheREADMEInstallTheMakefilesPins(t *testing.T) {
	pins := makefilePins(t)
	for _, drift := range pinDrift(runText(recipeSteps(t)), readWorkflow(t, ciRecipe).Env, pins) {
		t.Errorf("%s: %s", ciRecipe, drift)
	}

	install := section(t, readFile(t, "README.md"), "## Install")
	assignments := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^(\w+)=(\S+)$`).FindAllStringSubmatch(install, -1) {
		assignments[m[1]] = m[2]
	}
	for _, drift := range pinDrift(install, assignments, pins) {
		t.Errorf("README.md, Install: %s", drift)
	}
}

// pinDrift says where commands name a pin other than the Makefile's, reading a
// $name through vars. It also says which pins they never name.
func pinDrift(commands string, vars, pins map[string]string) []string {
	var drift []string
	for _, pin := range []struct{ name, pattern string }{
		{"SETUP_ENVTEST_VERSION", `setup-envtest@(\S+)`},
		{"ENVTEST_K8S_VERSION", `setup-envtest use (\S+)`},
		{"ENVTEST_INDEX_URL", `--index (\S+)`},
	} {
		matches := regexp.MustCompile(pin.pattern).FindAllStringSubmatch(commands, -1)
		if len(matches) == 0 {
			drift = append(drift, "no command names "+pin.name)
		}
		for _, m := range matches {
			value := m[1]
			if name, isVar := strings.CutPrefix(value, "$"); isVar {
				value = vars[name]
			}
			if value != pins[pin.name] {
				drift = append(drift, fmt.Sprintf("%q names %q, and the Makefile's %s is %q", m[0], value, pin.name, pins[pin.name]))
			}
		}
	}
	return drift
}

// makefilePins are the Makefile's variables, each $(NAME) in them expanded.
func makefilePins(t *testing.T) map[string]string {
	t.Helper()
	pins := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^(\w+) \?= (.*)$`).FindAllStringSubmatch(readFile(t, "Makefile"), -1) {
		pins[m[1]] = regexp.MustCompile(`\$\((\w+)\)`).ReplaceAllStringFunc(m[2], func(ref string) string {
			return pins[ref[2:len(ref)-1]]
		})
	}
	return pins
}

// section is the text under a Markdown heading, up to the next heading.
func section(t *testing.T, doc, heading string) string {
	t.Helper()
	_, after, found := strings.Cut(doc, "\n"+heading+"\n")
	if !found {
		t.Fatalf("no %q heading", heading)
	}
	if next := regexp.MustCompile(`(?m)^#`).FindStringIndex(after); next != nil {
		return after[:next[0]]
	}
	return after
}

func TestTheCIRecipeCachesWhatItInstalls(t *testing.T) {
	steps := recipeSteps(t)
	restore := stepUsing(t, steps, "actions/cache/restore")
	save := stepUsing(t, steps, "actions/cache/save")
	commands := runText(steps)

	binDir := regexp.MustCompile(`setup-envtest use .*--bin-dir (\S+)`).FindStringSubmatch(commands)
	if binDir == nil {
		t.Fatal("setup-envtest installs the control plane with no --bin-dir, into a directory outside the workspace")
	}
	cached := strings.Fields(restore.With["path"])
	for _, dir := range []string{"~/go/bin", binDir[1]} {
		if !slices.Contains(cached, dir) {
			t.Errorf("the cache holds %q and leaves out %s, where the recipe installs", cached, dir)
		}
	}
	if save.With["path"] != restore.With["path"] {
		t.Errorf("the cache saves %q and restores %q", save.With["path"], restore.With["path"])
	}

	keyed := []string{"${{ runner.os }}", "${{ runner.arch }}"}
	for _, m := range regexp.MustCompile(`\$(\w+_VERSION)\b`).FindAllStringSubmatch(commands, -1) {
		keyed = append(keyed, "${{ env."+m[1]+" }}")
	}
	for _, part := range keyed {
		if !strings.Contains(restore.With["key"], part) {
			t.Errorf("the cache key %q leaves out %s, so a change to it restores a stale cache", restore.With["key"], part)
		}
	}

	missed := "steps." + restore.ID + ".outputs.cache-hit != 'true'"
	if save.If != missed {
		t.Errorf("the cache saves if %q, not only on a miss (%s)", save.If, missed)
	}
	if want := "${{ steps." + restore.ID + ".outputs.cache-primary-key }}"; save.With["key"] != want {
		t.Errorf("the cache saves under %q, not %q", save.With["key"], want)
	}
	saved := slices.IndexFunc(steps, func(s step) bool { return s.Uses == save.Uses })
	for i, s := range steps {
		switch {
		case strings.Contains(s.Run, "go install") && s.If != missed:
			t.Errorf("step %d builds with go install on a cache hit too: %q", i, s.Run)
		case (strings.Contains(s.Run, "go install") || strings.Contains(s.Run, "setup-envtest use")) && i > saved:
			t.Errorf("step %d installs after the cache is saved: %q", i, s.Run)
		case strings.Contains(s.Run, "botbox run") && i < saved:
			t.Errorf("step %d runs botbox before the cache is saved, so a find would leave it unsaved: %q", i, s.Run)
		}
	}
}

func runText(steps []step) string {
	var commands []string
	for _, s := range steps {
		commands = append(commands, s.Run)
	}
	return strings.Join(commands, "\n")
}

func TestTheCIRecipeNeedsNoGoMod(t *testing.T) {
	setupGo := stepUsing(t, recipeSteps(t), "actions/setup-go")
	if file, ok := setupGo.With["go-version-file"]; ok {
		t.Errorf("setup-go reads the Go version from %s, which an operator in another language lacks", file)
	}
	if cache := setupGo.With["cache"]; cache != "false" {
		t.Errorf("setup-go has cache: %q, and its cache warns on every run without a go.sum", cache)
	}
	directive := regexp.MustCompile(`(?m)^go (\d+\.\d+)`).FindStringSubmatch(readFile(t, "go.mod"))
	if directive == nil {
		t.Fatal("go.mod has no go directive")
	}
	if version := setupGo.With["go-version"]; version != directive[1] {
		t.Errorf("setup-go installs Go %q, and botbox's go.mod asks for %s", version, directive[1])
	}
}
