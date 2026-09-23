//go:build envtest

package cluster_test

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

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

func TestStartIgnoresUseExistingCluster(t *testing.T) {
	t.Setenv("USE_EXISTING_CLUSTER", "true")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-such-kubeconfig"))

	c, err := cluster.Start(cluster.Options{})
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if err := c.Stop(); err != nil {
		t.Errorf("Stop returned an error: %v", err)
	}
}

func thingCRDDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "thing.yaml"), []byte(thingCRD), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStartServesAPIAndInstallsCRDs(t *testing.T) {
	c, err := cluster.Start(cluster.Options{CRDPaths: []string{thingCRDDir(t)}})
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
	requireThingServed(t, c.Config())
}

func TestConnectInstallsCRDsAndLeavesThem(t *testing.T) {
	bare, err := cluster.Start(cluster.Options{})
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := bare.Stop(); err != nil {
			t.Errorf("Stop returned an error: %v", err)
		}
	})
	kubeconfig := writeKubeconfig(t, bare.Config())
	crds := cluster.Options{CRDPaths: []string{thingCRDDir(t)}}

	// The second Connect finds the CRD installed and replaces it.
	for range 2 {
		c, err := cluster.Connect(kubeconfig, crds)
		if err != nil {
			t.Fatalf("Connect returned an error: %v", err)
		}
		if err := c.Stop(); err != nil {
			t.Fatalf("Stop returned an error: %v", err)
		}
	}

	requireThingServed(t, bare.Config())
}

func requireThingServed(t *testing.T, config *rest.Config) {
	t.Helper()
	mapper, err := cluster.NewRESTMapper(config)
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

func writeKubeconfig(t *testing.T, config *rest.Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	kubeconfig := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{"envtest": {
			Server:                   config.Host,
			CertificateAuthorityData: config.CAData,
		}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"admin": {
			ClientCertificateData: config.CertData,
			ClientKeyData:         config.KeyData,
		}},
		Contexts:       map[string]*clientcmdapi.Context{"envtest": {Cluster: "envtest", AuthInfo: "admin"}},
		CurrentContext: "envtest",
	}
	if err := clientcmd.WriteToFile(kubeconfig, path); err != nil {
		t.Fatal(err)
	}
	return path
}
