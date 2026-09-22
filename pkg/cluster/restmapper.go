package cluster

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// NewRESTMapper resolves a kind to the resource the API server serves it at.
// Discovery runs once, so the target's CRDs must already be installed.
func NewRESTMapper(config *rest.Config) (meta.RESTMapper, error) {
	client, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("building botbox's discovery client: %w", err)
	}
	groups, err := restmapper.GetAPIGroupResources(client)
	if err != nil {
		return nil, fmt.Errorf("discovering the API resources: %w", err)
	}
	return restmapper.NewDiscoveryRESTMapper(groups), nil
}
