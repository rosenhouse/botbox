//go:build envtest

package cluster_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"

	"github.com/rosenhouse/botbox/pkg/cluster"
)

const thingCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: things.test.botbox
spec:
  group: test.botbox
  names:
    kind: Thing
    plural: things
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
`

func TestStartServesAPIAndInstallsCRDs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "thing.yaml"), []byte(thingCRD), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := cluster.Start(cluster.Options{CRDPaths: []string{dir}})
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Stop(); err != nil {
			t.Errorf("Stop returned an error: %v", err)
		}
	})

	dc, err := discovery.NewDiscoveryClientForConfig(c.Config())
	if err != nil {
		t.Fatalf("Building a discovery client failed: %v", err)
	}
	version, err := dc.ServerVersion()
	if err != nil {
		t.Fatalf("Fetching the server version failed: %v", err)
	}
	if version.GitVersion == "" {
		t.Error("The server reported an empty GitVersion.")
	}

	groups, err := dc.ServerGroups()
	if err != nil {
		t.Fatalf("Listing API groups failed: %v", err)
	}
	if !slices.ContainsFunc(groups.Groups, func(g metav1.APIGroup) bool { return g.Name == "test.botbox" }) {
		t.Error("The CRD group test.botbox was not installed.")
	}
}
