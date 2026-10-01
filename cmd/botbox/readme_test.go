package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// quickstartRun is the README's first cert-manager quickstart command and the
// block that shows what it prints.
var quickstartRun = regexp.MustCompile("(?s)```sh\nexamples/cert-manager/quickstart\\.sh ([^\n]*)\n```\n\n```\n(.*?)```\n")

// What the cert-manager quickstart prints depends on what its seeds draw.
func TestTheREADMEShowsWhatTheCertManagerQuickstartPrints(t *testing.T) {
	t.Chdir("../..")
	m := quickstartRun.FindStringSubmatch(readFile(t, "README.md"))
	if m == nil {
		t.Fatal("The README runs no cert-manager quickstart and shows no output after it.")
	}
	args := append(quickstartArgs(t, strings.Fields(m[1])), "--out", t.TempDir())

	// The fake session passes every run, as cert-manager does.
	code, stdout, stderr := invokeWith(t, &fakeSession{}, rapidGenerator, args...)
	if code != exitOK {
		t.Fatalf("botbox %s exited %d: %s", strings.Join(args, " "), code, stderr)
	}
	if stdout != m[2] {
		t.Errorf("The README shows\n%s\nafter quickstart.sh %s, and botbox printed\n%s", m[2], m[1], stdout)
	}
}

// quickstartArgs are what examples/cert-manager/quickstart.sh passes botbox
// when it is given args.
func quickstartArgs(t *testing.T, args []string) []string {
	t.Helper()
	_, invocation, found := strings.Cut(readFile(t, "examples/cert-manager/quickstart.sh"), "./bin/botbox ")
	if !found {
		t.Fatal("quickstart.sh runs no ./bin/botbox.")
	}
	invocation, _, _ = strings.Cut(strings.ReplaceAll(invocation, "\\\n", " "), "\n")
	var botbox []string
	for _, arg := range strings.Fields(invocation) {
		if arg == `"$@"` {
			botbox = append(botbox, args...)
		} else {
			botbox = append(botbox, arg)
		}
	}
	return botbox
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
