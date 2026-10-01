package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// quickstartRun is the README's first cert-manager quickstart command and the
// block that shows what botbox prints.
var quickstartRun = regexp.MustCompile("(?s)```sh\nexamples/cert-manager/quickstart\\.sh ([^\n]*)\n```\n\n```\n(.*?)```\n")

// What botbox prints in the cert-manager quickstart depends on what its seeds
// draw.
func TestTheREADMEShowsWhatBotboxPrintsInTheCertManagerQuickstart(t *testing.T) {
	t.Chdir("../..")
	args, shown := quickstart(t, readFile(t, "README.md"), readFile(t, "examples/cert-manager/quickstart.sh"))
	args = append(args, "--out", t.TempDir())

	// The fake session passes every run, as cert-manager does.
	code, stdout, stderr := invokeWith(t, &fakeSession{}, rapidGenerator, args...)
	if code != exitOK {
		t.Fatalf("botbox %s exited %d: %s", strings.Join(args, " "), code, stderr)
	}
	if stdout != shown {
		t.Errorf("The README shows\n%s\nand botbox %s printed\n%s", shown, strings.Join(args, " "), stdout)
	}
}

func TestQuickstartPassesBotboxTheREADMEsArgs(t *testing.T) {
	readme := "```sh\nexamples/cert-manager/quickstart.sh --seed 7\n```\n\n```\nprinted\n```\n"
	script := "KUBEBUILDER_ASSETS=x ./bin/botbox run \\\n  --runs 5 \"$@\"\necho done\n"
	args, shown := quickstart(t, readme, script)
	if want := []string{"run", "--runs", "5", "--seed", "7"}; !slices.Equal(args, want) || shown != "printed\n" {
		t.Errorf("quickstart returned %q and %q, want %q and %q.", args, shown, want, "printed\n")
	}
}

// quickstart returns what script passes botbox when it runs the README's first
// cert-manager quickstart command, and the block after that command.
func quickstart(t *testing.T, readme, script string) (args []string, shown string) {
	t.Helper()
	m := quickstartRun.FindStringSubmatch(readme)
	if m == nil {
		t.Fatal("The README runs no cert-manager quickstart and shows no output after it.")
	}
	_, invocation, found := strings.Cut(script, "./bin/botbox ")
	if !found {
		t.Fatal("quickstart.sh runs no ./bin/botbox.")
	}
	invocation, _, _ = strings.Cut(strings.ReplaceAll(invocation, "\\\n", " "), "\n")
	for _, arg := range strings.Fields(invocation) {
		if arg == `"$@"` {
			args = append(args, strings.Fields(m[1])...)
		} else {
			args = append(args, arg)
		}
	}
	return args, m[2]
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
