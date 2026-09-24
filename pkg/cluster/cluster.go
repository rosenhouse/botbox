// Package cluster provides the test cluster a botbox run executes against: an
// envtest control plane it starts, or an existing cluster a kubeconfig names.
// envtest reads KUBEBUILDER_ASSETS itself; `make setup` installs the binaries.
//
// This is the one harness package allowed to import controller-runtime
// (DESIGN.md §11).
package cluster

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Options configure a test cluster.
type Options struct {
	// CRDPaths are files or directories holding CRD manifests to install.
	CRDPaths []string
}

// Validate reports the first CRD path that cannot be stat'ed, or the first
// control plane binary envtest cannot run, before a control plane starts.
func (o Options) Validate() error {
	if err := o.checkCRDPaths(); err != nil {
		return err
	}
	for _, binary := range []string{"etcd", "kube-apiserver"} {
		if err := findBinary(binary); err != nil {
			return err
		}
	}
	return nil
}

func (o Options) checkCRDPaths() error {
	for _, path := range o.CRDPaths {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("CRD path: %w", err)
		}
	}
	return nil
}

// findBinary looks for a control plane binary where envtest does.
func findBinary(name string) error {
	override := "TEST_ASSET_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	variable, fix := override, "fix or unset "+override
	value, set := os.LookupEnv(override)
	path := value
	if !set {
		variable, fix = "KUBEBUILDER_ASSETS", "install the control plane with setup-envtest, and set KUBEBUILDER_ASSETS to the directory it prints"
		value, set = os.LookupEnv(variable)
		path = filepath.Join(value, name)
		if !set {
			path = filepath.Join("/usr/local/kubebuilder/bin", name)
		}
	}
	problem := cannotRun(name, path)
	switch {
	case problem == "":
		return nil
	case !set:
		return fmt.Errorf("%s is not set, and %s; %s", variable, problem, fix)
	case path == "":
		return fmt.Errorf("%s is empty; %s", variable, fix)
	case value == "":
		return fmt.Errorf("%s is empty, and %s; %s", variable, problem, fix)
	}
	return fmt.Errorf("%s is %s, and %s; %s", variable, value, problem, fix)
}

// cannotRun says why envtest cannot run path, or is empty if it can. Like
// os/exec, it looks a name with no slash up on PATH.
func cannotRun(name, path string) string {
	if filepath.Base(path) == path {
		if _, err := exec.LookPath(path); err != nil {
			return fmt.Sprintf("envtest found no %s on PATH", path)
		}
		return ""
	}
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return fmt.Sprintf("envtest found no %s at %s", name, path)
	case info.IsDir() || info.Mode().Perm()&0o111 == 0:
		return path + " is not executable"
	}
	return ""
}

// Cluster is a running test cluster.
type Cluster struct {
	// env is nil for a cluster botbox did not start.
	env    *envtest.Environment
	config *rest.Config
}

// Start brings up a control plane and installs the CRDs in opts. The caller
// must call Stop.
func Start(opts Options) (*Cluster, error) {
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("starting the test cluster: %w", err)
	}
	// envtest would otherwise read USE_EXISTING_CLUSTER and reach whatever
	// cluster KUBECONFIG names.
	existing := false
	env := &envtest.Environment{CRDDirectoryPaths: opts.CRDPaths, UseExistingCluster: &existing}
	// Only kube-controller-manager removes the finalizers these add. Append
	// keeps envtest's own entry, which disables ServiceAccount.
	apiServer := env.ControlPlane.GetAPIServer().Configure()
	apiServer.Append("disable-admission-plugins", "StorageObjectInUseProtection")
	apiServer.Set("enable-garbage-collector", "false")
	config, err := env.Start()
	if err != nil {
		return nil, fmt.Errorf("starting the envtest control plane: %w", err)
	}
	return &Cluster{env: env, config: config}, nil
}

// Connect reaches the cluster a kubeconfig names and installs the CRDs in
// opts there. It creates or replaces each CRD, waits until the API server
// serves it, and leaves it installed.
func Connect(kubeconfig string, opts Options) (*Cluster, error) {
	if err := opts.checkCRDPaths(); err != nil {
		return nil, fmt.Errorf("connecting to the test cluster: %w", err)
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("reading the kubeconfig %s: %w", kubeconfig, err)
	}
	if _, err := envtest.InstallCRDs(config, envtest.CRDInstallOptions{Paths: opts.CRDPaths}); err != nil {
		return nil, fmt.Errorf("installing the CRDs on the cluster %s names: %w", kubeconfig, err)
	}
	return &Cluster{config: config}, nil
}

// Config returns the admin client configuration for the API server.
func (c *Cluster) Config() *rest.Config { return c.config }

// Stop shuts down a control plane Start brought up, and leaves any other
// cluster running.
func (c *Cluster) Stop() error {
	if c.env == nil {
		return nil
	}
	if err := c.env.Stop(); err != nil {
		return fmt.Errorf("stopping the envtest control plane: %w", err)
	}
	return nil
}
