package cluster_test

import (
	"context"
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

func TestValidateAcceptsZeroOptions(t *testing.T) {
	if err := (cluster.Options{}).Validate(); err != nil {
		t.Errorf("Validate rejected the zero Options: %v", err)
	}
}

// pointAssetsNowhere makes sure a regression cannot start a control plane in
// the unit tier, which runs no API server (DESIGN.md §11).
func pointAssetsNowhere(t *testing.T) {
	t.Setenv("KUBEBUILDER_ASSETS", filepath.Join(t.TempDir(), "no-such-assets"))
}

func TestStartValidatesBeforeStartingEnvtest(t *testing.T) {
	pointAssetsNowhere(t)
	missing := filepath.Join(t.TempDir(), "no-such-dir")

	c, err := cluster.Start(context.Background(), cluster.Options{CRDPaths: []string{missing}})
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

func TestStartHonoursACancelledContext(t *testing.T) {
	pointAssetsNowhere(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c, err := cluster.Start(ctx, cluster.Options{})
	if err == nil {
		_ = c.Stop()
		t.Fatal("Start ignored a cancelled context.")
	}
	if c != nil {
		t.Error("Start returned a non-nil Cluster together with an error.")
	}
}
