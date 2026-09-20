// Package cluster starts and stops the envtest control plane a botbox run
// executes against (DESIGN.md §5.8). envtest reads KUBEBUILDER_ASSETS itself;
// `make setup` installs the binaries.
//
// This is the one harness package allowed to import controller-runtime
// (DESIGN.md §11).
package cluster

import (
	"context"
	"fmt"
	"os"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Options configure a test cluster.
type Options struct {
	// CRDPaths are files or directories holding CRD manifests to install.
	CRDPaths []string
}

// Validate reports the first CRD path that is not readable.
func (o Options) Validate() error {
	for _, path := range o.CRDPaths {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("CRD path %q is not readable: %w", path, err)
		}
	}
	return nil
}

// Cluster is a running test cluster.
type Cluster struct {
	env    *envtest.Environment
	config *rest.Config
}

// Start brings up a control plane and installs the CRDs in opts. The caller
// must call Stop. envtest's own start takes no context, so ctx only guards the
// work before it.
func Start(ctx context.Context, opts Options) (*Cluster, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("starting the test cluster: %w", err)
	}
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("starting the test cluster: %w", err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     opts.CRDPaths,
		ErrorIfCRDPathMissing: true,
	}
	config, err := env.Start()
	if err != nil {
		return nil, fmt.Errorf("starting the envtest control plane: %w", err)
	}
	return &Cluster{env: env, config: config}, nil
}

// Config returns the admin client configuration for the API server.
func (c *Cluster) Config() *rest.Config { return c.config }

// Stop shuts the control plane down.
func (c *Cluster) Stop() error {
	if err := c.env.Stop(); err != nil {
		return fmt.Errorf("stopping the envtest control plane: %w", err)
	}
	return nil
}
