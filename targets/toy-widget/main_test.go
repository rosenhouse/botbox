package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rosenhouse/botbox/pkg/target"
	"github.com/rosenhouse/botbox/targets/toy-widget/controller"
)

func writeKubeconfig(t *testing.T, server string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	contents := `apiVersion: v1
kind: Config
current-context: test
clusters:
  - name: test
    cluster: {server: ` + server + `}
contexts:
  - name: test
    context: {cluster: test, user: test}
users:
  - name: test
    user: {}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRestConfigPrefersTheFlagOverTheEnvironment(t *testing.T) {
	t.Setenv("KUBECONFIG", writeKubeconfig(t, "https://environment.example"))

	config, err := restConfig(writeKubeconfig(t, "https://flag.example"))
	if err != nil {
		t.Fatalf("restConfig returned an error: %v", err)
	}
	if config.Host != "https://flag.example" {
		t.Errorf("restConfig used the server %q, want the one from --kubeconfig.", config.Host)
	}
}

func TestRestConfigFallsBackToTheEnvironment(t *testing.T) {
	t.Setenv("KUBECONFIG", writeKubeconfig(t, "https://environment.example"))

	config, err := restConfig("")
	if err != nil {
		t.Fatalf("restConfig returned an error: %v", err)
	}
	if config.Host != "https://environment.example" {
		t.Errorf("restConfig used the server %q, want the one from $KUBECONFIG.", config.Host)
	}
}

// B1's hold is what makes its children land after the checkpoint that reads
// its premature status and inside the quiet window that follows, so it is
// timed against the T_stable the target declares (DESIGN.md §9.1).
func TestTheB1HoldLandsTheChildrenInTheQuietWindow(t *testing.T) {
	declared, err := target.Load("target.yaml")
	if err != nil {
		t.Fatalf("Loading the toy's declaration failed: %v", err)
	}

	stable := declared.Timeouts.Stable

	if b1Hold <= stable || b1Hold >= 2*stable {
		t.Errorf("B1 holds its premature status for %v against a T_stable of %v, want §9.1's T_stable < hold < 2 × T_stable.",
			b1Hold, stable)
	}
}

func TestRunRejectsABugOutsideTheCatalog(t *testing.T) {
	// The unreadable kubeconfig keeps a run that fails to reject the bug from
	// reaching an API server.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-such-kubeconfig"))
	outside := strconv.Itoa(controller.MaxBug + 1)

	err := run([]string{"--bug=" + outside}, io.Discard)

	if err == nil {
		t.Fatalf("run accepted --bug=%s.", outside)
	}
	if !strings.Contains(err.Error(), outside) {
		t.Errorf("run returned %q, which does not name the rejected value.", err)
	}
}

func TestRunRejectsOnlyANegativeResync(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-such-kubeconfig"))

	for resync, rejected := range map[string]bool{"-1s": true, "0s": false} {
		err := run([]string{"--resync=" + resync}, io.Discard)

		if got := err != nil && strings.Contains(err.Error(), "--resync="+resync); got != rejected {
			t.Errorf("run returned %v for --resync=%s, want it rejected: %v.", err, resync, rejected)
		}
	}
}

func TestRunPrintsTheUsageForHelp(t *testing.T) {
	printed := &strings.Builder{}

	err := run([]string{"--help"}, printed)

	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("run returned %v for --help, want flag.ErrHelp so that main exits 0.", err)
	}
	if !strings.Contains(printed.String(), "-bug") {
		t.Errorf("run printed %q for --help, want the usage.", printed.String())
	}
}
