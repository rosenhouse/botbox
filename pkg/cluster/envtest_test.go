//go:build envtest

package cluster_test

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"github.com/rosenhouse/botbox/pkg/cluster"
)

var (
	thingKind     = schema.GroupVersionKind{Group: "test.botbox", Version: "v1", Kind: "Thing"}
	thingResource = schema.GroupVersionResource{Group: "test.botbox", Version: "v1", Resource: "things"}
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

	// A mapper built after Start knows the kinds the CRDs installed.
	mapper, err := cluster.NewRESTMapper(c.Config())
	if err != nil {
		t.Fatalf("Building the RESTMapper failed: %v", err)
	}
	mapping, err := mapper.RESTMapping(thingKind.GroupKind(), thingKind.Version)
	if err != nil {
		t.Fatalf("The mapper does not resolve the installed %s: %v", thingKind.Kind, err)
	}
	if mapping.Resource != thingResource {
		t.Errorf("The mapper resolves %s to %v, want %v.", thingKind.Kind, mapping.Resource, thingResource)
	}
}
