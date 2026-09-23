package cluster_test

import (
	"os"
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
	installControlPlane(t, "etcd", "kube-apiserver")

	if err := (cluster.Options{CRDPaths: []string{t.TempDir()}}).Validate(); err != nil {
		t.Errorf("Validate rejected an existing directory: %v", err)
	}
}

// installControlPlane points envtest at a directory holding executables of
// these names, and at nothing else.
func installControlPlane(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	unsetenv(t, "TEST_ASSET_ETCD")
	unsetenv(t, "TEST_ASSET_KUBE_APISERVER")
	t.Setenv("KUBEBUILDER_ASSETS", dir)
	return dir
}

// unsetenv removes a variable for the test, since envtest reads one set to
// nothing as a path.
func unsetenv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func TestValidateNamesTheControlPlaneBinaryItCannotRun(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T) (want []string)
	}{
		{"KUBEBUILDER_ASSETS names an empty directory", func(t *testing.T) []string {
			dir := installControlPlane(t)
			return []string{"KUBEBUILDER_ASSETS", filepath.Join(dir, "etcd"), "setup-envtest"}
		}},
		{"KUBEBUILDER_ASSETS is not set", func(t *testing.T) []string {
			installControlPlane(t)
			unsetenv(t, "KUBEBUILDER_ASSETS")
			if _, err := os.Stat("/usr/local/kubebuilder/bin/etcd"); err == nil {
				t.Skip("etcd is installed where envtest looks by default.")
			}
			return []string{"KUBEBUILDER_ASSETS is not set", "/usr/local/kubebuilder/bin/etcd", "setup-envtest"}
		}},
		{"the directory holds etcd alone", func(t *testing.T) []string {
			dir := installControlPlane(t, "etcd")
			return []string{filepath.Join(dir, "kube-apiserver")}
		}},
		{"etcd is not executable", func(t *testing.T) []string {
			dir := installControlPlane(t, "kube-apiserver")
			if err := os.WriteFile(filepath.Join(dir, "etcd"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			return []string{filepath.Join(dir, "etcd"), "not executable"}
		}},
		{"etcd is a directory", func(t *testing.T) []string {
			dir := installControlPlane(t, "kube-apiserver")
			if err := os.Mkdir(filepath.Join(dir, "etcd"), 0o755); err != nil {
				t.Fatal(err)
			}
			return []string{filepath.Join(dir, "etcd"), "not executable"}
		}},
		{"TEST_ASSET_ETCD names a missing file", func(t *testing.T) []string {
			installControlPlane(t, "etcd", "kube-apiserver")
			missing := filepath.Join(t.TempDir(), "etcd")
			t.Setenv("TEST_ASSET_ETCD", missing)
			return []string{"TEST_ASSET_ETCD", missing}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := test.setup(t)

			err := cluster.Options{}.Validate()

			if err == nil {
				t.Fatal("Validate accepted a control plane envtest cannot run.")
			}
			for _, said := range want {
				if !strings.Contains(err.Error(), said) {
					t.Errorf("Validate returned %q, which does not say %q.", err, said)
				}
			}
		})
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
