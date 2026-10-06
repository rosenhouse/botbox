package main

import (
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/rosenhouse/reconciler-fuzzer/internal/reference"
)

const skillPage = "../../skills/adopt-reconciler-fuzzer/SKILL.md"

var yamlBlock = regexp.MustCompile("(?s)```yaml\n(.*?)```")

var skillCommand = regexp.MustCompile(`(?m)reconciler-fuzzer (run|replay)( [^\n` + "`" + `]*)?$`)

// The adoption skill runs only flags reconciler-fuzzer takes.
func TestTheAdoptionSkillPassesOnlyFlagsReconcilerFuzzerTakes(t *testing.T) {
	commands := skillCommand.FindAllStringSubmatch(readFile(t, skillPage), -1)
	if len(commands) == 0 {
		t.Fatal("The skill runs no reconciler-fuzzer command, so this test checks nothing.")
	}
	for _, m := range commands {
		o := &options{command: m[1]}
		if err := o.flags().Parse(strings.Fields(m[2])); err != nil {
			t.Errorf("The skill runs %q: %v", m[0], err)
		}
	}
}

// A target.yaml the skill shows uses only keys reconciler-fuzzer takes.
func TestTheAdoptionSkillsTargetUsesOnlyKeysTheReferenceLists(t *testing.T) {
	documented := reference.Listed(t, "## target.yaml")
	blocks := yamlBlock.FindAllStringSubmatch(readFile(t, skillPage), -1)
	if len(blocks) == 0 {
		t.Fatal("The skill shows no target.yaml, so this test checks nothing.")
	}
	for _, m := range blocks {
		block := m[1]
		var declared map[string]any
		if err := yaml.Unmarshal([]byte(block), &declared); err != nil {
			t.Fatalf("The skill shows a target.yaml that is not YAML: %v\n%s", err, block)
		}
		for _, key := range undocumentedKeys(declared, "", documented) {
			t.Errorf("The skill's target.yaml sets %s, which docs/reference.md does not list.", key)
		}
	}
}

func undocumentedKeys(value any, prefix string, documented []string) []string {
	if slices.Contains(documented, prefix) {
		return nil
	}
	switch v := value.(type) {
	case map[string]any:
		var missing []string
		for key, child := range v {
			if prefix != "" {
				key = prefix + "." + key
			}
			missing = append(missing, undocumentedKeys(child, key, documented)...)
		}
		return missing
	case []any:
		var missing []string
		for _, item := range v {
			missing = append(missing, undocumentedKeys(item, prefix+"[*]", documented)...)
		}
		return missing
	}
	return []string{prefix}
}

func TestUndocumentedKeysDescendsToTheKeysTheReferenceLists(t *testing.T) {
	declared := map[string]any{
		"name":       "x",
		"launch":     map[string]any{"env": map[string]any{"ANY": "1"}, "bogus": 1},
		"properties": []any{map[string]any{"id": "P1", "nope": true}},
	}
	got := undocumentedKeys(declared, "", []string{"name", "launch.env", "properties[*].id"})
	slices.Sort(got)
	if want := []string{"launch.bogus", "properties[*].nope"}; !slices.Equal(got, want) {
		t.Errorf("undocumentedKeys gave %q, want %q.", got, want)
	}
}

var versionSubstitution = regexp.MustCompile(`\$\(reconciler-fuzzer version[^)]*\)`)

// The skill and the README name the installed module's directory by the
// version that reconciler-fuzzer version prints.
func TestTheSkillAndTheReadmeReadTheVersionReconcilerFuzzerPrints(t *testing.T) {
	_, printed, _ := invoke(t, &fakeSession{}, "version")
	for _, page := range []string{skillPage, "../../README.md"} {
		substitutions := versionSubstitution.FindAllString(readFile(t, page), -1)
		if len(substitutions) == 0 {
			t.Errorf("%s reads no version, so this test checks nothing there.", page)
		}
		for _, substitution := range substitutions {
			bash := exec.Command("bash", "-c", `reconciler-fuzzer() { printf %s "$PRINTED"; }; printf %s "`+substitution+`"`)
			bash.Env = append(os.Environ(), "PRINTED="+printed)
			got, err := bash.Output()
			if err != nil {
				t.Fatalf("bash: %v", err)
			}
			if string(got) != version() {
				t.Errorf("%s reads the version as %s, which gives %q, want %q.", page, substitution, got, version())
			}
		}
	}
}
