// Package cluster starts and stops the envtest control plane a botbox run
// executes against (DESIGN.md §5.8). envtest reads KUBEBUILDER_ASSETS itself;
// `make setup` installs the binaries.
//
// This is the one harness package allowed to import controller-runtime
// (DESIGN.md §11).
package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/client-go/rest"
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
	for _, path := range o.CRDPaths {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("CRD path: %w", err)
		}
	}
	for _, binary := range []string{"etcd", "kube-apiserver"} {
		if err := findBinary(binary); err != nil {
			return err
		}
	}
	return nil
}

// findBinary looks for a control plane binary where envtest does.
func findBinary(name string) error {
	override := "TEST_ASSET_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	var path, lookedUp string
	if value, set := os.LookupEnv(override); set {
		path, lookedUp = value, fmt.Sprintf("%s is %s", override, value)
	} else if dir, set := os.LookupEnv("KUBEBUILDER_ASSETS"); set {
		path, lookedUp = filepath.Join(dir, name), "KUBEBUILDER_ASSETS is "+dir
	} else {
		path, lookedUp = filepath.Join("/usr/local/kubebuilder/bin", name), "KUBEBUILDER_ASSETS is not set"
	}
	info, err := os.Stat(path)
	switch {
	case err != nil:
		lookedUp += fmt.Sprintf(", and envtest found no %s at %s", name, path)
	case info.IsDir() || info.Mode().Perm()&0o111 == 0:
		lookedUp += fmt.Sprintf(", and %s is not executable", path)
	default:
		return nil
	}
	return fmt.Errorf("%s; install the control plane with setup-envtest, and set KUBEBUILDER_ASSETS to the directory it prints", lookedUp)
}

// Cluster is a running test cluster.
type Cluster struct {
	env    *envtest.Environment
	config *rest.Config
}

// Start brings up a control plane and installs the CRDs in opts. The caller
// must call Stop.
func Start(opts Options) (*Cluster, error) {
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("starting the test cluster: %w", err)
	}
	env := &envtest.Environment{CRDDirectoryPaths: opts.CRDPaths}
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
