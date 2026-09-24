package botbox_test

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
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
	On          map[string]any    `yaml:"on"`
	Env         map[string]string `yaml:"env"`
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]job    `yaml:"jobs"`
}

type job struct {
	RunsOn string `yaml:"runs-on"`
	Steps  []step `yaml:"steps"`
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

func recipeJob(t *testing.T) job {
	t.Helper()
	w := readWorkflow(t, ciRecipe)
	if len(w.Jobs) != 1 {
		t.Fatalf("%s has %d jobs, and an adopter copies one", ciRecipe, len(w.Jobs))
	}
	for _, j := range w.Jobs {
		return j
	}
	return job{}
}

func recipeSteps(t *testing.T) []step {
	t.Helper()
	return recipeJob(t).Steps
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
		t.Fatalf("%d steps use %s, not one", len(found), action)
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
			value := strings.Trim(m[1], `"`)
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
	var text strings.Builder
	fenced := false
	for _, line := range strings.SplitAfter(after, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(line, "#") {
			break
		}
		text.WriteString(line)
	}
	return text.String()
}

func TestSectionEndsAtTheNextHeadingOutsideAFence(t *testing.T) {
	doc := "# Doc\n\n## Install\n\n```sh\n# a comment\ncommand\n```\n\n### Against kind\n\ntext\n"
	if got, want := section(t, doc, "## Install"), "\n```sh\n# a comment\ncommand\n```\n\n"; got != want {
		t.Errorf("section returned %q, not %q", got, want)
	}
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

func TestTheCIRecipeExportsTheControlPlaneOrFails(t *testing.T) {
	steps := recipeSteps(t)
	i := slices.IndexFunc(steps, func(s step) bool { return strings.Contains(s.Run, "KUBEBUILDER_ASSETS=") })
	if i < 0 {
		t.Fatal("no step exports KUBEBUILDER_ASSETS")
	}
	script := steps[i].Run

	for _, test := range []struct {
		name, setupEnvtest, wantEnv string
		wantErr                     bool
	}{
		{
			name:         "setup-envtest prints the path",
			setupEnvtest: `case " $* " in *" -p path "*) echo /assets ;; *) echo "Path: /assets" ;; esac`,
			wantEnv:      "KUBEBUILDER_ASSETS=/assets\n",
		},
		{name: "setup-envtest fails", setupEnvtest: "exit 1", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "setup-envtest"), []byte("#!/bin/sh\n"+test.setupEnvtest+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			githubEnv := filepath.Join(dir, "github-env")
			// Actions runs a step with no shell key as bash -e.
			step := exec.Command("bash", "-e", "-c", script)
			step.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "GITHUB_ENV="+githubEnv)
			output, err := step.CombinedOutput()
			if (err != nil) != test.wantErr {
				t.Fatalf("the step returned %v, and wanted an error: %t\n%s", err, test.wantErr, output)
			}
			if exported, _ := os.ReadFile(githubEnv); string(exported) != test.wantEnv {
				t.Errorf("the step exported %q, not %q", exported, test.wantEnv)
			}
		})
	}
}

func TestTheCIRecipeKeepsAFailingRunsEvidence(t *testing.T) {
	steps := recipeSteps(t)
	upload := stepUsing(t, steps, "actions/upload-artifact")
	if upload.If != "failure()" {
		t.Errorf("the evidence uploads if %q, not when botbox fails", upload.If)
	}
	uploaded := strings.TrimSuffix(upload.With["path"], "/")
	runs := 0
	for _, s := range steps {
		if !strings.Contains(s.Run, "botbox run") {
			continue
		}
		runs++
		if out := regexp.MustCompile(`--out (\S+)`).FindStringSubmatch(s.Run); out == nil || out[1] != uploaded {
			t.Errorf("%q writes elsewhere than %s, which the job uploads", s.Run, uploaded)
		}
	}
	if runs == 0 {
		t.Fatal("no step runs botbox")
	}

	hint := downloadHint.FindStringSubmatch(readFile(t, ciRecipe))
	if hint == nil {
		t.Fatal("the recipe does not say how to download the evidence")
	}
	if hint[2] != upload.With["name"] {
		t.Errorf("%q names an artifact other than %s", hint[0], upload.With["name"])
	}
	if hint[3] != uploaded {
		t.Errorf("%q puts the evidence elsewhere than %s, where the replay command in report.md reads it", hint[0], uploaded)
	}
}

func TestTheCIRecipeFixesSeedsOnPullRequestsAndDrawsThemNightly(t *testing.T) {
	w := readWorkflow(t, ciRecipe)
	for _, event := range []string{"pull_request", "schedule"} {
		if _, ok := w.On[event]; !ok {
			t.Errorf("the recipe does not run on %s", event)
		}
	}
	seeded := map[string]bool{}
	for _, s := range recipeSteps(t) {
		if !strings.Contains(s.Run, "botbox run") {
			continue
		}
		event := regexp.MustCompile(`^github\.event_name == '(\w+)'$`).FindStringSubmatch(s.If)
		if event == nil {
			t.Errorf("%q runs if %q, not on one event", s.Run, s.If)
			continue
		}
		seeded[event[1]] = strings.Contains(s.Run, "--seed ")
	}
	if want := map[string]bool{"pull_request": true, "schedule": false}; !maps.Equal(seeded, want) {
		t.Errorf("botbox run fixes a seed by event as %v, and should as %v", seeded, want)
	}
}

func TestNightlyFindsSayHowToRestoreTheirEvidence(t *testing.T) {
	reports := 0
	for name, job := range readWorkflow(t, ".github/workflows/nightly.yml").Jobs {
		for _, s := range job.Steps {
			if !strings.Contains(s.Run, "gh issue create") {
				continue
			}
			reports++
			upload := stepUsing(t, job.Steps, "actions/upload-artifact")
			hint := downloadHint.FindStringSubmatch(s.Run)
			if hint == nil {
				t.Errorf("job %s files an issue that does not say how to download its evidence", name)
				continue
			}
			expand := func(text string) string {
				return os.Expand(text, func(v string) string { return s.Env[v] })
			}
			if expand(hint[1]) != "${{ github.run_id }}" {
				t.Errorf("job %s: %q names a run other than the one that failed", name, hint[0])
			}
			if expand(hint[2]) != upload.With["name"] {
				t.Errorf("job %s: %q names an artifact other than %s", name, hint[0], upload.With["name"])
			}
			if expand(hint[3]) != strings.TrimSuffix(upload.With["path"], "/") {
				t.Errorf("job %s: %q puts the evidence elsewhere than %s, where a report's replay command reads it", name, hint[0], upload.With["path"])
			}
		}
	}
	if reports == 0 {
		t.Fatal("no nightly job files an issue")
	}
}

// downloadHint matches a gh command that downloads one artifact of a run into a directory.
var downloadHint = regexp.MustCompile(`gh run download (\S+) --name ([\w.$/-]+) --dir ([\w.$/-]+)`)

func TestTheCIRecipeSetsEveryVariableItReads(t *testing.T) {
	set := readWorkflow(t, ciRecipe).Env
	for _, s := range recipeSteps(t) {
		var read []string
		for _, m := range regexp.MustCompile(`\$\{?([A-Z][A-Z0-9_]*)`).FindAllStringSubmatch(s.Run, -1) {
			_, local := s.Env[m[1]]
			if !local && !strings.HasPrefix(m[1], "GITHUB_") && !strings.HasPrefix(m[1], "RUNNER_") {
				read = append(read, m[1])
			}
		}
		for _, value := range s.With {
			for _, m := range regexp.MustCompile(`\$\{\{ env\.(\w+) \}\}`).FindAllStringSubmatch(value, -1) {
				read = append(read, m[1])
			}
		}
		for _, name := range read {
			if _, ok := set[name]; !ok {
				t.Errorf("a step reads %s, and the recipe's env does not set it", name)
			}
		}
	}
}

func TestTheCIRecipeChecksOutTheRepositoryWithAReadOnlyToken(t *testing.T) {
	recipe := recipeJob(t)
	if recipe.RunsOn == "" {
		t.Error("the job names no runner")
	}
	if len(recipe.Steps) == 0 || !strings.HasPrefix(recipe.Steps[0].Uses, "actions/checkout@") {
		t.Error("the job's first step does not check out the repository")
	}
	if permissions := readWorkflow(t, ciRecipe).Permissions; !maps.Equal(permissions, map[string]string{"contents": "read"}) {
		t.Errorf("the job's token has %v, more than it needs to read the repository", permissions)
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
