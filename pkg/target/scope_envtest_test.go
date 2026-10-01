//go:build envtest

package target_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"github.com/rosenhouse/botbox/pkg/cluster"
	"github.com/rosenhouse/botbox/pkg/target"
)

func TestLoadKnowsTheScopeOfEveryKindABareAPIServerServes(t *testing.T) {
	clusterScoped, namespaced := servedKinds(t)
	if len(clusterScoped) == 0 || len(namespaced) == 0 {
		t.Fatalf("The API server serves %d cluster-scoped and %d namespaced kinds.", len(clusterScoped), len(namespaced))
	}

	for _, kind := range clusterScoped {
		path := writeTarget(t, minimalTarget+"manages: ["+kind+"]\n", map[string]string{"widget.yaml": sampleWidget})
		if _, err := target.Load(path); err == nil || !strings.HasSuffix(err.Error(), "the managed "+kind) {
			t.Errorf("Load returned %v for a target that manages the cluster-scoped %s.", err, kind)
		}
	}
	path := writeTarget(t, minimalTarget+"manages: ["+strings.Join(namespaced, ", ")+"]\n", map[string]string{"widget.yaml": sampleWidget})
	if _, err := target.Load(path); err != nil {
		t.Errorf("Load refused a target that manages only namespaced kinds: %v", err)
	}
}

// servedKinds starts a control plane with no CRDs and lists the kinds it
// serves, at every version, as target.yaml names them.
func servedKinds(t *testing.T) (clusterScoped, namespaced []string) {
	t.Helper()
	c, err := cluster.Start(cluster.Options{})
	if err != nil {
		t.Fatalf("Starting the cluster failed: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Stop(); err != nil {
			t.Errorf("Stopping the cluster failed: %v", err)
		}
	})
	client, err := discovery.NewDiscoveryClientForConfig(c.Config())
	if err != nil {
		t.Fatal(err)
	}
	_, lists, err := client.ServerGroupsAndResources()
	if err != nil {
		t.Fatalf("Discovery failed: %v", err)
	}
	for _, list := range lists {
		groupVersion, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			t.Fatal(err)
		}
		for _, resource := range list.APIResources {
			if strings.Contains(resource.Name, "/") {
				continue
			}
			kind := groupVersion.String() + "/" + resource.Kind
			if resource.Namespaced {
				namespaced = append(namespaced, kind)
			} else {
				clusterScoped = append(clusterScoped, kind)
			}
		}
	}
	return clusterScoped, namespaced
}
