//go:build envtest

package run_test

import (
	"os"
	"strings"
	"testing"

	"github.com/rosenhouse/reconciler-fuzzer/internal/run"
)

// The README tells a reader to pin a sequence that deletes an object of each
// managed kind and updates what each property reads. Its example passes the
// toy with no bug, and each of those ops catches a bug the drawn runs seldom
// reach.
func TestTheREADMEsPinnedSequenceCatchesWhatItSays(t *testing.T) {
	t.Parallel()
	binary := buildToy(t)
	sequence := readSequence(t, pinnedSequence(t))
	testCluster := startCluster(t, loadTarget(t, binary).CRDs)

	for _, test := range []struct {
		name, bug, check, says string
	}{
		{name: "the toy with no bug", bug: "--bug=0"},
		{name: "a toy that does not watch its ConfigMaps", bug: "--bug=8", check: "G7", says: "op 1 (deleteManaged)"},
		{name: "a toy that keeps the ConfigMaps a lower count drops", bug: "--bug=7", check: "G4", says: "op 2 (update)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			toy := loadTarget(t, binary)
			toy.Launch.Args = append(toy.Launch.Args, test.bug)

			result, err := run.Run(t.Context(), toy, sequence, run.Options{Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{}})

			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			got := result.Violation
			switch {
			case test.check == "" && got != nil:
				t.Errorf("The run reported %s: %s", got.ID, got.Statement)
			case test.check == "":
			case got == nil:
				t.Errorf("The run passed, want %s.", test.check)
			case got.ID != test.check || !strings.Contains(got.Statement, test.says):
				t.Errorf("The run reported %s: %s\nwant %s saying %q.", got.ID, got.Statement, test.check, test.says)
			}
		})
	}
}

// pinnedSequence is the sequence the README's Pin sequences section shows.
func pinnedSequence(t *testing.T) string {
	t.Helper()
	readme, err := os.ReadFile(repoRoot + "/README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(readme), "\n### Pin sequences\n")
	section, _, _ = strings.Cut(section, "\n## ")
	_, block, shown := strings.Cut(section, "\n```json\n")
	block, _, _ = strings.Cut(block, "```")
	if !found || !shown {
		t.Fatal("README.md has no Pin sequences section with a sequence in it.")
	}
	return block
}
