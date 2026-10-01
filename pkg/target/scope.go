package target

import (
	"slices"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// builtinClusterScoped are the kinds Kubernetes itself serves at cluster scope.
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

// clusterScopedAtLoad reports the built-in kinds and the kinds the CRDs define
// that are cluster-scoped.
func clusterScopedAtLoad(crds []map[string]any) func(schema.GroupVersionKind) bool {
	byCRD := clusterScopedByCRD(crds)
	return func(gvk schema.GroupVersionKind) bool {
		return slices.Contains(builtinClusterScoped, gvk.GroupKind()) || byCRD(gvk)
	}
}
