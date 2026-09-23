package cluster_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse/botbox/pkg/cluster"
)

func TestValidateRejectsMissingCRDPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")

	err := cluster.Options{CRDPaths: []string{missing}}.Validate()
	if err == nil {
		t.Fatal("Validate accepted a CRD path that does not exist.")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("Validate returned %q, which does not name the missing path %q.", err, missing)
	}
}

func TestValidateAcceptsExistingCRDPath(t *testing.T) {
	if err := (cluster.Options{CRDPaths: []string{t.TempDir()}}).Validate(); err != nil {
		t.Errorf("Validate rejected an existing directory: %v", err)
	}
}

// pointAssetsNowhere makes sure a regression cannot start a control plane in
// the unit tier, which runs no API server (DESIGN.md §11).
func pointAssetsNowhere(t *testing.T) {
	t.Setenv("KUBEBUILDER_ASSETS", filepath.Join(t.TempDir(), "no-such-assets"))
}

func TestConnectValidatesBeforeReadingTheKubeconfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")

	c, err := cluster.Connect(filepath.Join(t.TempDir(), "no-such-kubeconfig"), cluster.Options{CRDPaths: []string{missing}})
	if err == nil {
		_ = c.Stop()
		t.Fatal("Connect accepted a CRD path that does not exist.")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("Connect returned %q, which does not name the missing path %q.", err, missing)
	}
}

func TestConnectNamesAKubeconfigItCannotRead(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "no-such-kubeconfig")

	_, err := cluster.Connect(kubeconfig, cluster.Options{})
	if err == nil || !strings.Contains(err.Error(), kubeconfig) {
		t.Errorf("Connect returned %v, want an error naming %s.", err, kubeconfig)
	}
}

func TestStartValidatesBeforeStartingEnvtest(t *testing.T) {
	pointAssetsNowhere(t)
	missing := filepath.Join(t.TempDir(), "no-such-dir")

	c, err := cluster.Start(cluster.Options{CRDPaths: []string{missing}})
	if err == nil {
		_ = c.Stop()
		t.Fatal("Start accepted a CRD path that does not exist.")
	}
	if c != nil {
		t.Error("Start returned a non-nil Cluster together with an error.")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("Start returned %q, which does not name the missing path %q.", err, missing)
	}
}
