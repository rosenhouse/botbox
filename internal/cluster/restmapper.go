package cluster

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// NewRESTMapper resolves a kind to the resource the API server serves it at.
// Discovery runs once, so the target's CRDs must already be installed.
func NewRESTMapper(config *rest.Config) (meta.RESTMapper, error) {
	groups, err := discover(config)
	if err != nil {
		return nil, err
	}
	return restmapper.NewDiscoveryRESTMapper(groups), nil
}

// ServedResources lists the resources the API server serves now, at every
// version, leaving out subresources.
func ServedResources(config *rest.Config) ([]metav1.APIResource, error) {
	groups, err := discover(config)
	if err != nil {
		return nil, err
	}
	var served []metav1.APIResource
	for _, group := range groups {
		for _, resources := range group.VersionedResources {
			for _, resource := range resources {
				if !strings.Contains(resource.Name, "/") {
					served = append(served, resource)
				}
			}
		}
	}
	return served, nil
}

func discover(config *rest.Config) ([]*restmapper.APIGroupResources, error) {
	client, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("building botbox's discovery client: %w", err)
	}
	groups, err := restmapper.GetAPIGroupResources(client)
	if err != nil {
		return nil, fmt.Errorf("discovering the API resources: %w", err)
	}
	return groups, nil
}
