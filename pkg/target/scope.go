package target

import "k8s.io/apimachinery/pkg/runtime/schema"

// builtinScopes maps each kind a Kubernetes 1.37 API server serves by default
// to whether it is namespaced.
var builtinScopes = map[schema.GroupKind]bool{
	{Kind: "Binding"}:                           true,
	{Kind: "ConfigMap"}:                         true,
	{Kind: "Endpoints"}:                         true,
	{Kind: "Event"}:                             true,
	{Kind: "LimitRange"}:                        true,
	{Kind: "PersistentVolumeClaim"}:             true,
	{Kind: "Pod"}:                               true,
	{Kind: "PodTemplate"}:                       true,
	{Kind: "ReplicationController"}:             true,
	{Kind: "ResourceQuota"}:                     true,
	{Kind: "Secret"}:                            true,
	{Kind: "Service"}:                           true,
	{Kind: "ServiceAccount"}:                    true,
	{Group: "apps", Kind: "ControllerRevision"}: true,
	{Group: "apps", Kind: "DaemonSet"}:          true,
	{Group: "apps", Kind: "Deployment"}:         true,
	{Group: "apps", Kind: "ReplicaSet"}:         true,
	{Group: "apps", Kind: "StatefulSet"}:        true,
	{Group: "authorization.k8s.io", Kind: "LocalSubjectAccessReview"}: true,
	{Group: "autoscaling", Kind: "HorizontalPodAutoscaler"}:           true,
	{Group: "batch", Kind: "CronJob"}:                                 true,
	{Group: "batch", Kind: "Job"}:                                     true,
	{Group: "certificates.k8s.io", Kind: "PodCertificateRequest"}:     true,
	{Group: "coordination.k8s.io", Kind: "Lease"}:                     true,
	{Group: "discovery.k8s.io", Kind: "EndpointSlice"}:                true,
	{Group: "events.k8s.io", Kind: "Event"}:                           true,
	{Group: "networking.k8s.io", Kind: "Ingress"}:                     true,
	{Group: "networking.k8s.io", Kind: "NetworkPolicy"}:               true,
	{Group: "policy", Kind: "PodDisruptionBudget"}:                    true,
	{Group: "rbac.authorization.k8s.io", Kind: "Role"}:                true,
	{Group: "rbac.authorization.k8s.io", Kind: "RoleBinding"}:         true,
	{Group: "resource.k8s.io", Kind: "ResourceClaim"}:                 true,
	{Group: "resource.k8s.io", Kind: "ResourceClaimTemplate"}:         true,
	{Group: "storage.k8s.io", Kind: "CSIStorageCapacity"}:             true,

	{Kind: "ComponentStatus"}:  false,
	{Kind: "Namespace"}:        false,
	{Kind: "Node"}:             false,
	{Kind: "PersistentVolume"}: false,
	{Group: "admissionregistration.k8s.io", Kind: "MutatingAdmissionPolicy"}:          false,
	{Group: "admissionregistration.k8s.io", Kind: "MutatingAdmissionPolicyBinding"}:   false,
	{Group: "admissionregistration.k8s.io", Kind: "MutatingWebhookConfiguration"}:     false,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingAdmissionPolicy"}:        false,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingAdmissionPolicyBinding"}: false,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingWebhookConfiguration"}:   false,
	{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}:                 false,
	{Group: "apiregistration.k8s.io", Kind: "APIService"}:                             false,
	{Group: "authentication.k8s.io", Kind: "SelfSubjectReview"}:                       false,
	{Group: "authentication.k8s.io", Kind: "TokenReview"}:                             false,
	{Group: "authorization.k8s.io", Kind: "SelfSubjectAccessReview"}:                  false,
	{Group: "authorization.k8s.io", Kind: "SelfSubjectRulesReview"}:                   false,
	{Group: "authorization.k8s.io", Kind: "SubjectAccessReview"}:                      false,
	{Group: "certificates.k8s.io", Kind: "CertificateSigningRequest"}:                 false,
	{Group: "certificates.k8s.io", Kind: "ClusterTrustBundle"}:                        false,
	{Group: "flowcontrol.apiserver.k8s.io", Kind: "FlowSchema"}:                       false,
	{Group: "flowcontrol.apiserver.k8s.io", Kind: "PriorityLevelConfiguration"}:       false,
	{Group: "networking.k8s.io", Kind: "IPAddress"}:                                   false,
	{Group: "networking.k8s.io", Kind: "IngressClass"}:                                false,
	{Group: "networking.k8s.io", Kind: "ServiceCIDR"}:                                 false,
	{Group: "node.k8s.io", Kind: "RuntimeClass"}:                                      false,
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}:                         false,
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding"}:                  false,
	{Group: "resource.k8s.io", Kind: "DeviceClass"}:                                   false,
	{Group: "resource.k8s.io", Kind: "DeviceTaintRule"}:                               false,
	{Group: "resource.k8s.io", Kind: "ResourceSlice"}:                                 false,
	{Group: "scheduling.k8s.io", Kind: "PriorityClass"}:                               false,
	{Group: "storage.k8s.io", Kind: "CSIDriver"}:                                      false,
	{Group: "storage.k8s.io", Kind: "CSINode"}:                                        false,
	{Group: "storage.k8s.io", Kind: "StorageClass"}:                                   false,
	{Group: "storage.k8s.io", Kind: "VolumeAttachment"}:                               false,
	{Group: "storage.k8s.io", Kind: "VolumeAttributesClass"}:                          false,
	{Group: "storagemigration.k8s.io", Kind: "StorageVersionMigration"}:               false,
}

// BuiltinPlural returns the resource plural of a built-in Kubernetes kind, or
// empty if the kind is not in the table.
func BuiltinPlural(gk schema.GroupKind) string {
	return builtinPlurals[gk]
}

// builtinPlurals maps each kind in builtinScopes to its resource plural.
var builtinPlurals = map[schema.GroupKind]string{
	{Kind: "Binding"}:               "bindings",
	{Kind: "ConfigMap"}:             "configmaps",
	{Kind: "Endpoints"}:             "endpoints",
	{Kind: "Event"}:                 "events",
	{Kind: "LimitRange"}:            "limitranges",
	{Kind: "PersistentVolumeClaim"}: "persistentvolumeclaims",
	{Kind: "Pod"}:                   "pods",
	{Kind: "PodTemplate"}:           "podtemplates",
	{Kind: "ReplicationController"}: "replicationcontrollers",
	{Kind: "ResourceQuota"}:         "resourcequotas",
	{Kind: "Secret"}:                "secrets",
	{Kind: "Service"}:               "services",
	{Kind: "ServiceAccount"}:        "serviceaccounts",

	{Kind: "ComponentStatus"}:  "componentstatuses",
	{Kind: "Namespace"}:        "namespaces",
	{Kind: "Node"}:             "nodes",
	{Kind: "PersistentVolume"}: "persistentvolumes",

	{Group: "apps", Kind: "ControllerRevision"}: "controllerrevisions",
	{Group: "apps", Kind: "DaemonSet"}:          "daemonsets",
	{Group: "apps", Kind: "Deployment"}:         "deployments",
	{Group: "apps", Kind: "ReplicaSet"}:         "replicasets",
	{Group: "apps", Kind: "StatefulSet"}:        "statefulsets",

	{Group: "autoscaling", Kind: "HorizontalPodAutoscaler"}: "horizontalpodautoscalers",
	{Group: "batch", Kind: "CronJob"}:                       "cronjobs",
	{Group: "batch", Kind: "Job"}:                           "jobs",
	{Group: "coordination.k8s.io", Kind: "Lease"}:           "leases",
	{Group: "discovery.k8s.io", Kind: "EndpointSlice"}:      "endpointslices",
	{Group: "events.k8s.io", Kind: "Event"}:                 "events",
	{Group: "networking.k8s.io", Kind: "Ingress"}:           "ingresses",
	{Group: "networking.k8s.io", Kind: "NetworkPolicy"}:     "networkpolicies",
	{Group: "policy", Kind: "PodDisruptionBudget"}:          "poddisruptionbudgets",

	{Group: "rbac.authorization.k8s.io", Kind: "Role"}:               "roles",
	{Group: "rbac.authorization.k8s.io", Kind: "RoleBinding"}:        "rolebindings",
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}:        "clusterroles",
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding"}: "clusterrolebindings",

	{Group: "certificates.k8s.io", Kind: "CertificateSigningRequest"}: "certificatesigningrequests",

	{Group: "resource.k8s.io", Kind: "ResourceClaim"}:         "resourceclaims",
	{Group: "resource.k8s.io", Kind: "ResourceClaimTemplate"}: "resourceclaimtemplates",
	{Group: "storage.k8s.io", Kind: "CSIStorageCapacity"}:     "csistoragecapacities",
	{Group: "storage.k8s.io", Kind: "CSIDriver"}:              "csidrivers",
	{Group: "storage.k8s.io", Kind: "CSINode"}:                "csinodes",
	{Group: "storage.k8s.io", Kind: "StorageClass"}:           "storageclasses",
	{Group: "storage.k8s.io", Kind: "VolumeAttachment"}:       "volumeattachments",
}

// scopeFunc reports whether a kind is namespaced, and whether it knows the kind.
type scopeFunc func(schema.GroupVersionKind) (namespaced, known bool)

// scopeAtLoad knows the built-in kinds and the kinds the CRDs define.
func scopeAtLoad(crds []map[string]any) scopeFunc {
	return func(gvk schema.GroupVersionKind) (bool, bool) {
		if namespaced, known := builtinScopes[gvk.GroupKind()]; known {
			return namespaced, true
		}
		return scopeByCRD(crds, gvk)
	}
}
