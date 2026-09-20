package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestRunRejectsABugOutsideTheCatalog(t *testing.T) {
	// The unreadable kubeconfig keeps a run that fails to reject the bug from
	// reaching an API server.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-such-kubeconfig"))

	err := run([]string{"--bug=11"}, io.Discard)

	if err == nil {
		t.Fatal("run accepted --bug=11.")
	}
	if !strings.Contains(err.Error(), "11") {
		t.Errorf("run returned %q, which does not name the rejected value.", err)
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
