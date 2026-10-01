package target

import (
	"slices"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// builtinNamespaced and builtinClusterScoped are the kinds a Kubernetes 1.37
// API server serves by default.
var builtinNamespaced = []schema.GroupKind{
	{Kind: "Binding"},
	{Kind: "ConfigMap"},
	{Kind: "Endpoints"},
	{Kind: "Event"},
	{Kind: "LimitRange"},
	{Kind: "PersistentVolumeClaim"},
	{Kind: "Pod"},
	{Kind: "PodTemplate"},
	{Kind: "ReplicationController"},
	{Kind: "ResourceQuota"},
	{Kind: "Secret"},
	{Kind: "Service"},
	{Kind: "ServiceAccount"},
	{Group: "apps", Kind: "ControllerRevision"},
	{Group: "apps", Kind: "DaemonSet"},
	{Group: "apps", Kind: "Deployment"},
	{Group: "apps", Kind: "ReplicaSet"},
	{Group: "apps", Kind: "StatefulSet"},
	{Group: "authorization.k8s.io", Kind: "LocalSubjectAccessReview"},
	{Group: "autoscaling", Kind: "HorizontalPodAutoscaler"},
	{Group: "batch", Kind: "CronJob"},
	{Group: "batch", Kind: "Job"},
	{Group: "certificates.k8s.io", Kind: "PodCertificateRequest"},
	{Group: "coordination.k8s.io", Kind: "Lease"},
	{Group: "discovery.k8s.io", Kind: "EndpointSlice"},
	{Group: "events.k8s.io", Kind: "Event"},
	{Group: "networking.k8s.io", Kind: "Ingress"},
	{Group: "networking.k8s.io", Kind: "NetworkPolicy"},
	{Group: "policy", Kind: "PodDisruptionBudget"},
	{Group: "rbac.authorization.k8s.io", Kind: "Role"},
	{Group: "rbac.authorization.k8s.io", Kind: "RoleBinding"},
	{Group: "resource.k8s.io", Kind: "ResourceClaim"},
	{Group: "resource.k8s.io", Kind: "ResourceClaimTemplate"},
	{Group: "storage.k8s.io", Kind: "CSIStorageCapacity"},
}

var builtinClusterScoped = []schema.GroupKind{
	{Kind: "ComponentStatus"},
	{Kind: "Namespace"},
	{Kind: "Node"},
	{Kind: "PersistentVolume"},
	{Group: "admissionregistration.k8s.io", Kind: "MutatingAdmissionPolicy"},
	{Group: "admissionregistration.k8s.io", Kind: "MutatingAdmissionPolicyBinding"},
	{Group: "admissionregistration.k8s.io", Kind: "MutatingWebhookConfiguration"},
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingAdmissionPolicy"},
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingAdmissionPolicyBinding"},
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingWebhookConfiguration"},
	{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"},
	{Group: "apiregistration.k8s.io", Kind: "APIService"},
	{Group: "authentication.k8s.io", Kind: "SelfSubjectReview"},
	{Group: "authentication.k8s.io", Kind: "TokenReview"},
	{Group: "authorization.k8s.io", Kind: "SelfSubjectAccessReview"},
	{Group: "authorization.k8s.io", Kind: "SelfSubjectRulesReview"},
	{Group: "authorization.k8s.io", Kind: "SubjectAccessReview"},
	{Group: "certificates.k8s.io", Kind: "CertificateSigningRequest"},
	{Group: "certificates.k8s.io", Kind: "ClusterTrustBundle"},
	{Group: "flowcontrol.apiserver.k8s.io", Kind: "FlowSchema"},
	{Group: "flowcontrol.apiserver.k8s.io", Kind: "PriorityLevelConfiguration"},
	{Group: "networking.k8s.io", Kind: "IPAddress"},
	{Group: "networking.k8s.io", Kind: "IngressClass"},
	{Group: "networking.k8s.io", Kind: "ServiceCIDR"},
	{Group: "node.k8s.io", Kind: "RuntimeClass"},
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"},
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding"},
	{Group: "resource.k8s.io", Kind: "DeviceClass"},
	{Group: "resource.k8s.io", Kind: "DeviceTaintRule"},
	{Group: "resource.k8s.io", Kind: "ResourceSlice"},
	{Group: "scheduling.k8s.io", Kind: "PriorityClass"},
	{Group: "storage.k8s.io", Kind: "CSIDriver"},
	{Group: "storage.k8s.io", Kind: "CSINode"},
	{Group: "storage.k8s.io", Kind: "StorageClass"},
	{Group: "storage.k8s.io", Kind: "VolumeAttachment"},
	{Group: "storage.k8s.io", Kind: "VolumeAttributesClass"},
	{Group: "storagemigration.k8s.io", Kind: "StorageVersionMigration"},
}

// scope reports whether a kind is namespaced, and whether it knows the kind.
type scope func(schema.GroupVersionKind) (namespaced, known bool)

// scopeAtLoad knows the built-in kinds and the kinds the CRDs define.
func scopeAtLoad(crds []map[string]any) scope {
	return func(gvk schema.GroupVersionKind) (bool, bool) {
		switch {
		case slices.Contains(builtinNamespaced, gvk.GroupKind()):
			return true, true
		case slices.Contains(builtinClusterScoped, gvk.GroupKind()):
			return false, true
		}
		return scopeByCRD(crds, gvk)
	}
}
