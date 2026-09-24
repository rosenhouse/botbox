package botbox_test

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

const ciRecipe = "examples/ci/github-actions.yml"

// workflow holds what these tests read of a GitHub Actions workflow. yaml.v3
// reads the on key as a string, as Actions does. A YAML 1.1 parser reads it as
// true.
type workflow struct {
	On          any               `yaml:"on"`
	Env         map[string]string `yaml:"env"`
	Permissions any               `yaml:"permissions"`
	Defaults    any               `yaml:"defaults"`
	Jobs        map[string]job    `yaml:"jobs"`
}

type job struct {
	If              string            `yaml:"if"`
	RunsOn          any               `yaml:"runs-on"`
	Env             map[string]string `yaml:"env"`
	Defaults        any               `yaml:"defaults"`
	ContinueOnError any               `yaml:"continue-on-error"`
	TimeoutMinutes  any               `yaml:"timeout-minutes"`
	Steps           []step            `yaml:"steps"`
}

type step struct {
	ID              string            `yaml:"id"`
	If              string            `yaml:"if"`
	ContinueOnError any               `yaml:"continue-on-error"`
	TimeoutMinutes  any               `yaml:"timeout-minutes"`
	Uses            string            `yaml:"uses"`
	With            map[string]string `yaml:"with"`
	Env             map[string]string `yaml:"env"`
	Shell           string            `yaml:"shell"`
	WorkingDir      string            `yaml:"working-directory"`
	Run             string            `yaml:"run"`
}

func readWorkflow(t *testing.T, path string) workflow {
	t.Helper()
	w, err := parseWorkflow(readFile(t, path))
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return w
}

func parseWorkflow(content string) (workflow, error) {
	var w workflow
	err := yaml.Unmarshal([]byte(content), &w)
	return w, err
}

func TestParseWorkflowReadsEachFormOfOnPermissionsAndRunsOn(t *testing.T) {
	for _, content := range []string{
		"on: push\npermissions: read-all\njobs: {a: {runs-on: ubuntu-latest}}\n",
		"on: [push]\njobs: {a: {runs-on: [self-hosted, linux]}}\n",
		"on: {push: {}}\npermissions: {contents: read}\njobs: {a: {runs-on: {group: large}}}\n",
	} {
		if _, err := parseWorkflow(content); err != nil {
			t.Errorf("parsing %q: %v", content, err)
		}
	}
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
	if prefixes, ok := restore.With["restore-keys"]; ok {
		t.Errorf("the cache also restores keys %q, which carry old control planes into each new cache", prefixes)
	}

	want := []string{"runner.arch", "runner.os"}
	for _, m := range regexp.MustCompile(`\$\{?(\w+_VERSION)\b`).FindAllStringSubmatch(commands, -1) {
		want = append(want, "env."+m[1])
	}
	var keyed []string
	for _, m := range regexp.MustCompile(`\$\{\{\s*(.*?)\s*\}\}`).FindAllStringSubmatch(restore.With["key"], -1) {
		keyed = append(keyed, m[1])
	}
	slices.Sort(want)
	slices.Sort(keyed)
	if want = slices.Compact(want); !slices.Equal(keyed, want) {
		t.Errorf("the cache key %q reads %q, not %q. A pin it leaves out restores a stale cache, and anything else can miss on every run", restore.With["key"], keyed, want)
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
		case strings.Contains(s.Run, "botbox run") && i < saved:
			t.Errorf("step %d runs botbox before the cache is saved, so a find would leave it unsaved: %q", i, s.Run)
		}
	}
}

// The walk takes every step to pass, and so runs no step if: failure().
func TestTheCIRecipeGivesEachStepWhatItNeedsOnACacheHitAndAMiss(t *testing.T) {
	cached := []string{"botbox", "setup-envtest", "the control plane"}
	effects := []struct {
		usesOrRuns   *regexp.Regexp
		needs, gives []string
	}{
		{regexp.MustCompile(`^actions/setup-go@`), nil, []string{"go"}},
		{regexp.MustCompile(`(?m)^\s*go `), []string{"go"}, nil},
		{regexp.MustCompile(`/botbox@`), nil, []string{"botbox"}},
		{regexp.MustCompile(`/setup-envtest@`), nil, []string{"setup-envtest"}},
		{regexp.MustCompile(`setup-envtest use`), []string{"setup-envtest"}, []string{"the control plane"}},
		{regexp.MustCompile(`KUBEBUILDER_ASSETS=`), nil, []string{"KUBEBUILDER_ASSETS"}},
		{regexp.MustCompile(`^actions/cache/save@`), cached, nil},
		{regexp.MustCompile(`go build -o`), nil, []string{"the controller"}},
		{regexp.MustCompile(`botbox run`), []string{"botbox", "KUBEBUILDER_ASSETS", "the controller"}, []string{"a botbox run"}},
	}
	recipe := recipeJob(t)
	restore := stepUsing(t, recipe.Steps, "actions/cache/restore")
	for _, event := range slices.Sorted(maps.Keys(recipeEvents(t))) {
		for _, hit := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s with cache-hit %t", event, hit), func(t *testing.T) {
				runs := func(condition string) bool {
					if on := regexp.MustCompile(`^github\.event_name == '(\w+)'$`).FindStringSubmatch(condition); on != nil {
						return on[1] == event
					}
					switch condition {
					case "":
						return true
					case "failure()":
						return false
					case "steps." + restore.ID + ".outputs.cache-hit != 'true'":
						return !hit
					}
					t.Fatalf("the walk cannot evaluate if: %s", condition)
					return false
				}
				has := map[string]bool{}
				for i, s := range recipe.Steps {
					if !runs(recipe.If) || !runs(s.If) {
						continue
					}
					var gives []string
					if hit && s.Uses == restore.Uses {
						gives = append(gives, cached...)
					}
					for _, effect := range effects {
						if !effect.usesOrRuns.MatchString(s.Uses + "\n" + s.Run) {
							continue
						}
						for _, need := range effect.needs {
							if !has[need] {
								t.Errorf("step %d needs %s, and no step before it gives it", i, need)
							}
						}
						gives = append(gives, effect.gives...)
					}
					for _, g := range gives {
						has[g] = true
					}
				}
				if !has["a botbox run"] {
					t.Error("no step runs botbox")
				}
			})
		}
	}
}

func TestTheCIRecipeInstallsEachToolItRunsAtAPin(t *testing.T) {
	module := regexp.MustCompile(`(?m)^module (\S+)$`).FindStringSubmatch(readFile(t, "go.mod"))
	setupEnvtest := regexp.MustCompile(`go install (\S+/setup-envtest)@\$\(SETUP_ENVTEST_VERSION\)`).FindStringSubmatch(readFile(t, "Makefile"))
	if module == nil || setupEnvtest == nil {
		t.Fatal("go.mod names no module, or the Makefile does not go install setup-envtest")
	}
	want := []string{
		"install " + module[1] + "/cmd/botbox@" + readWorkflow(t, ciRecipe).Env["BOTBOX_VERSION"],
		"install " + setupEnvtest[1] + "@" + makefilePins(t)["SETUP_ENVTEST_VERSION"],
	}
	var got []string
	for _, s := range recipeSteps(t) {
		if !strings.Contains(s.Run, "go install") {
			continue
		}
		_, output, err := runStep(t, s, "go", `echo "$*${GOBIN:+ into $GOBIN}${GOPATH:+ under $GOPATH}"`)
		if err != nil {
			t.Fatalf("%q: %v\n%s", s.Run, err, output)
		}
		got = append(got, strings.Split(strings.TrimSpace(string(output)), "\n")...)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the recipe runs go %q, not %q, which puts each tool in ~/go/bin at its pin", got, want)
	}
}

func TestTheCIRecipeExportsTheControlPlaneOrFails(t *testing.T) {
	steps := recipeSteps(t)
	i := slices.IndexFunc(steps, func(s step) bool { return strings.Contains(s.Run, "KUBEBUILDER_ASSETS=") })
	if i < 0 {
		t.Fatal("no step exports KUBEBUILDER_ASSETS")
	}
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
			exported, output, err := runStep(t, steps[i], "setup-envtest", test.setupEnvtest)
			if (err != nil) != test.wantErr {
				t.Fatalf("the step returned %v, and wanted an error: %t\n%s", err, test.wantErr, output)
			}
			if exported != test.wantEnv {
				t.Errorf("the step exported %q, not %q", exported, test.wantEnv)
			}
		})
	}
}

// runStep runs a recipe step's script under the recipe's env alone, with a
// stub for one command, and returns what the script wrote to GITHUB_ENV.
func runStep(t *testing.T, s step, command, stub string) (exported string, output []byte, err error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, command), []byte("#!/bin/sh\n"+stub+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	githubEnv := filepath.Join(dir, "github-env")
	// Actions runs a step with no shell key as bash -e.
	cmd := exec.Command("bash", "-e", "-c", s.Run)
	cmd.Env = []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "GITHUB_ENV=" + githubEnv}
	for _, env := range []map[string]string{readWorkflow(t, ciRecipe).Env, recipeJob(t).Env, s.Env} {
		for name, value := range env {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	output, err = cmd.CombinedOutput()
	env, _ := os.ReadFile(githubEnv)
	return string(env), output, err
}

func TestTheCIRecipeFailsWhenBotboxFinds(t *testing.T) {
	recipe := recipeJob(t)
	if recipe.ContinueOnError != nil || slices.ContainsFunc(recipe.Steps, func(s step) bool { return s.ContinueOnError != nil }) {
		t.Error("the recipe sets continue-on-error, which can pass a find and skip the upload")
	}
	for _, s := range botboxRuns(t, recipe.Steps) {
		if _, output, err := runStep(t, s, "botbox", "exit 1"); err == nil {
			t.Errorf("%q passes when botbox fails\n%s", s.Run, output)
		}
	}
}

func TestTheCIRecipeRunsEachStepInBashFromTheWorkspaceRoot(t *testing.T) {
	recipe := recipeJob(t)
	if readWorkflow(t, ciRecipe).Defaults != nil || recipe.Defaults != nil {
		t.Error("the recipe sets defaults, which can change the shell or the directory of every step")
	}
	for i, s := range recipe.Steps {
		if s.Shell != "" {
			t.Errorf("step %d runs under shell %q, and these tests run it as bash -e, as Actions runs a step with no shell key", i, s.Shell)
		}
		if s.WorkingDir != "" {
			t.Errorf("step %d runs in %s, and the upload and the replay command in report.md read paths from the workspace root", i, s.WorkingDir)
		}
	}
}

// botboxRuns are the steps that run botbox.
func botboxRuns(t *testing.T, steps []step) []step {
	t.Helper()
	var runs []step
	for _, s := range steps {
		if strings.Contains(s.Run, "botbox run") {
			runs = append(runs, s)
		}
	}
	if len(runs) == 0 {
		t.Fatal("no step runs botbox")
	}
	return runs
}

func TestTheCIRecipeKeepsAFailingRunsEvidence(t *testing.T) {
	recipe := recipeJob(t)
	steps := recipe.Steps
	upload := stepUsing(t, steps, "actions/upload-artifact")
	if upload.If != "failure()" {
		t.Errorf("the evidence uploads if %q, not when botbox fails", upload.If)
	}
	if upload.With["if-no-files-found"] == "error" {
		t.Error("the upload adds an error to a job that failed before botbox wrote anything")
	}
	uploaded := strings.TrimSuffix(upload.With["path"], "/")
	for _, s := range botboxRuns(t, steps) {
		if out := regexp.MustCompile(`--out (\S+)`).FindStringSubmatch(s.Run); out == nil || out[1] != uploaded {
			t.Errorf("%q writes elsewhere than %s, which the job uploads", s.Run, uploaded)
		}
		_, deadline := botboxBudget(t, s)
		for _, timeout := range []any{recipe.TimeoutMinutes, s.TimeoutMinutes} {
			minutes, err := strconv.ParseFloat(fmt.Sprint(timeout), 64)
			if timeout != nil && (err != nil || time.Duration(minutes*float64(time.Minute)) <= deadline) {
				t.Errorf("timeout-minutes %v can stop %q before its --deadline makes it write a report", timeout, s.Run)
			}
		}
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

// recipeEvents are the events the recipe runs on, each with its settings.
func recipeEvents(t *testing.T) map[string]any {
	t.Helper()
	on := readWorkflow(t, ciRecipe).On
	events, ok := on.(map[string]any)
	if !ok {
		t.Fatalf("the recipe's on key is %v, not a map of events", on)
	}
	return events
}

func TestTheCIRecipeFixesSeedsOnPullRequestsAndDrawsThemNightly(t *testing.T) {
	on := recipeEvents(t)
	if _, ok := on["pull_request"]; !ok {
		t.Error("the recipe does not run on pull_request")
	}
	schedule, _ := on["schedule"].([]any)
	if len(schedule) == 0 || slices.ContainsFunc(schedule, func(entry any) bool {
		timing, _ := entry.(map[string]any)
		cron, _ := timing["cron"].(string)
		return len(strings.Fields(cron)) != 5
	}) {
		t.Errorf("the recipe's schedule is %v, not a list of five-field crons, and Actions rejects such a workflow", on["schedule"])
	}
	seeded := map[string]bool{}
	for _, s := range botboxRuns(t, recipeSteps(t)) {
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

func TestTheCIRecipeSizesEachBotboxRun(t *testing.T) {
	pins := makefilePins(t)
	perRun := func(tier string) time.Duration {
		runs, _ := strconv.Atoi(pins[tier+"_RUNS"])
		deadline, err := time.ParseDuration(pins[tier+"_DEADLINE"])
		if err != nil || runs == 0 {
			t.Fatalf("the Makefile's %s_RUNS is %q and its %s_DEADLINE is %q", tier, pins[tier+"_RUNS"], tier, pins[tier+"_DEADLINE"])
		}
		return deadline / time.Duration(runs)
	}
	floor := min(perRun("EXAMPLE"), perRun("NIGHTLY"))
	for _, s := range botboxRuns(t, recipeSteps(t)) {
		switch runs, deadline := botboxBudget(t, s); {
		case runs == 0 || deadline == 0:
			t.Errorf("%q leaves --runs or --deadline to botbox's defaults, and an adopter sizes the two together", s.Run)
		case runs < 2:
			t.Errorf("%q runs %d sequences, and a tier should run several", s.Run, runs)
		case deadline/time.Duration(runs) < floor:
			t.Errorf("%q gives each run %s, less than the %s that this repository's example tiers give each run", s.Run, deadline/time.Duration(runs), floor)
		}
	}
}

// botboxBudget is the --runs and --deadline a botbox step passes, or zero.
func botboxBudget(t *testing.T, s step) (runs int, deadline time.Duration) {
	t.Helper()
	if m := regexp.MustCompile(`--runs (\d+)`).FindStringSubmatch(s.Run); m != nil {
		runs, _ = strconv.Atoi(m[1])
	}
	if m := regexp.MustCompile(`--deadline (\S+)`).FindStringSubmatch(s.Run); m != nil {
		var err error
		if deadline, err = time.ParseDuration(m[1]); err != nil {
			t.Fatalf("%q: %v", s.Run, err)
		}
	}
	return runs, deadline
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
				return os.Expand(text, func(v string) string {
					if value, ok := s.Env[v]; ok {
						return value
					}
					return "$" + v
				})
			}
			if expand(hint[1]) != "$GITHUB_RUN_ID" {
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

func TestTheCIRecipeSetsEachPinForEveryStep(t *testing.T) {
	pins := readWorkflow(t, ciRecipe).Env
	recipe := recipeJob(t)
	envs := map[string]map[string]string{"the job": recipe.Env}
	for i, s := range recipe.Steps {
		envs[fmt.Sprintf("step %d", i)] = s.Env
	}
	for where, env := range envs {
		for name := range env {
			if _, ok := pins[name]; ok {
				t.Errorf("%s sets %s over the pin in the workflow's env", where, name)
			}
		}
	}
}

func TestTheCIRecipeRunsWhereThisRepositorysWorkflowsRun(t *testing.T) {
	paths, err := filepath.Glob(".github/workflows/*.yml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("found workflows %v: %v", paths, err)
	}
	runners, releases := map[string]bool{}, map[string]bool{}
	for _, path := range paths {
		for _, j := range readWorkflow(t, path).Jobs {
			runners[fmt.Sprint(j.RunsOn)] = true
			for _, s := range j.Steps {
				releases[actionRelease(s.Uses)] = true
			}
		}
	}
	recipe := recipeJob(t)
	if !runners[fmt.Sprint(recipe.RunsOn)] {
		t.Errorf("the recipe runs its bash steps on %v, and this repository's workflows run only on %v", recipe.RunsOn, slices.Sorted(maps.Keys(runners)))
	}
	for _, s := range recipe.Steps {
		if s.Uses != "" && !releases[actionRelease(s.Uses)] {
			t.Errorf("the recipe uses %s, a release this repository's workflows do not run", s.Uses)
		}
	}
}

// actionRelease names an action's repository and version, so that
// actions/cache/restore@v4 is actions/cache@v4.
func actionRelease(uses string) string {
	name, version, _ := strings.Cut(uses, "@")
	parts := strings.SplitN(name, "/", 3)
	return strings.Join(parts[:min(len(parts), 2)], "/") + "@" + version
}

func TestTheCIRecipeChecksOutTheRepositoryWithAReadOnlyToken(t *testing.T) {
	recipe := recipeJob(t)
	if len(recipe.Steps) == 0 || !strings.HasPrefix(recipe.Steps[0].Uses, "actions/checkout@") {
		t.Error("the job's first step does not check out the repository")
	}
	if permissions := readWorkflow(t, ciRecipe).Permissions; !reflect.DeepEqual(permissions, map[string]any{"contents": "read"}) {
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
