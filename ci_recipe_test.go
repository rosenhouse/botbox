package botbox_test

import (
	"os"
	"regexp"
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
