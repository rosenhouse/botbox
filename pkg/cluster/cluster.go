// Package cluster provides the test cluster a botbox run executes against: an
// envtest control plane it starts, or an existing cluster a kubeconfig names
// (DESIGN.md §5.8). envtest reads KUBEBUILDER_ASSETS itself; `make setup`
// installs the binaries.
//
// This is the one harness package allowed to import controller-runtime
// (DESIGN.md §11).
package cluster

import (
	"fmt"
	"os"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Options configure a test cluster.
type Options struct {
	// CRDPaths are files or directories holding CRD manifests to install.
	CRDPaths []string
}

// Validate reports the first CRD path that cannot be stat'ed, so that a typo
// fails before a control plane starts.
func (o Options) Validate() error {
	for _, path := range o.CRDPaths {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("CRD path: %w", err)
		}
	}
	return nil
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
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("connecting to the test cluster: %w", err)
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("reading the kubeconfig %s: %w", kubeconfig, err)
	}
	install := envtest.CRDInstallOptions{Paths: opts.CRDPaths, ErrorIfPathMissing: true}
	if _, err := envtest.InstallCRDs(config, install); err != nil {
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
