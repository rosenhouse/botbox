package main

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestMechanismTellsHowTheOldKeySurvived(t *testing.T) {
	old := secret{UID: "old-secret", Owner: "old-certificate", Key: "rsa"}
	for _, c := range []struct {
		after secret
		want  string
	}{
		{secret{UID: "new-secret", Owner: "new-certificate", Key: "ecdsa"}, "new key"},
		{secret{UID: "new-secret", Owner: "new-certificate", Key: "rsa"}, "write-back"},
		{secret{UID: "old-secret", Owner: "new-certificate", Key: "rsa"}, "re-point"},
		{secret{UID: "old-secret", Owner: "old-certificate", Key: "rsa"}, "not collected"},
		{secret{}, "no Secret"},
	} {
		if got := mechanism(old, c.after, "new-certificate"); got != c.want {
			t.Errorf("mechanism(%+v, %+v) = %q, want %q", old, c.after, got, c.want)
		}
	}
}

func TestCollectGarbageDeletesWhatTheOldCertificateControls(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{secrets: "SecretList", requests: "CertificateRequestList"},
		controlled("Secret", "example-tls", "old"),
		controlled("CertificateRequest", "example-1", "old"),
		controlled("CertificateRequest", "example-2", "new"))
	var deleted []string
	client.PrependReactor("delete", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		del := action.(k8stesting.DeleteAction)
		name := del.GetName()
		if p := del.GetDeleteOptions().Preconditions; p == nil || p.UID == nil || *p.UID != types.UID(name+"-uid") || p.ResourceVersion == nil || *p.ResourceVersion != "7" {
			t.Errorf("deleted %s without the UID and resourceVersion it was read with: %+v", name, p)
		}
		deleted = append(deleted, name)
		if name == "example-tls" {
			return true, nil, apierrors.NewConflict(secrets.GroupResource(), name, nil)
		}
		return true, nil, apierrors.NewNotFound(requests.GroupResource(), name)
	})

	if err := (repro{dyn: client, ns: "ns"}).collectGarbage(context.Background(), "old"); err != nil || len(deleted) > 0 {
		t.Fatalf("without -collect, collectGarbage deleted %v and returned %v", deleted, err)
	}
	if err := (repro{dyn: client, ns: "ns", collect: true}).collectGarbage(context.Background(), "old"); err != nil {
		t.Fatalf("collectGarbage returned %v; a garbage collector leaves an object that changed or went since it read it", err)
	}
	if want := []string{"example-tls", "example-1"}; !slices.Equal(deleted, want) {
		t.Errorf("collectGarbage deleted %v, want %v", deleted, want)
	}
}

func controlled(kind, name string, owner types.UID) *unstructured.Unstructured {
	o := object(kind, name, nil)
	if kind == "Secret" {
		o.SetAPIVersion("v1")
	}
	o.SetNamespace("ns")
	o.SetUID(types.UID(name + "-uid"))
	o.SetResourceVersion("7")
	controller := true
	o.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "cert-manager.io/v1", Kind: "Certificate", Name: "example", UID: owner, Controller: &controller}})
	return o
}

func TestSecretRecordsUIDControllerAndKeyHash(t *testing.T) {
	controller := true
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "example-tls", Namespace: "ns", UID: "secret-uid", OwnerReferences: []metav1.OwnerReference{
			{UID: "other"},
			{UID: "certificate-uid", Controller: &controller},
		}},
		Data: map[string][]byte{"tls.key": []byte("key"), "tls.crt": []byte("crt")},
	}
	r := repro{core: kubefake.NewClientset(s), ns: "ns"}
	got, err := r.secret(context.Background())
	want := secret{UID: "secret-uid", Owner: "certificate-uid", Key: "2c70e12b7a0646f92279f427c7b38e7334d8e5389cff167a1dc30e73f826b683"}
	if err != nil || got != want {
		t.Errorf("secret() = %+v, %v; want %+v", got, err, want)
	}
	if got, err := (repro{core: kubefake.NewClientset(), ns: "ns"}).secret(context.Background()); err != nil || got != (secret{}) {
		t.Errorf("secret() without a Secret = %+v, %v; want none", got, err)
	}
}

func TestReadyWaitsForReadyAndReportsTheLastCondition(t *testing.T) {
	for _, c := range []struct{ status, reason string }{{"True", "Ready"}, {"False", "SecretMismatch"}} {
		crt := certificate("Never", "")
		crt.SetNamespace("ns")
		crt.Object["status"] = map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": c.status, "reason": c.reason},
			map[string]any{"type": "Issuing", "status": "True", "reason": "Other"},
		}}
		r := repro{dyn: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), crt), ns: "ns"}
		start := time.Now()
		got := r.ready(context.Background(), time.Second)
		if want := c.status + " " + c.reason; got != want {
			t.Errorf("ready() = %q, want %q", got, want)
		}
		if waited := time.Since(start); (c.status == "True") != (waited < 500*time.Millisecond) {
			t.Errorf("ready() returned %q after %v; it waits until Ready or the timeout", got, waited)
		}
	}
}

func TestCertificateSetsTheKeysPolicyAndAlgorithm(t *testing.T) {
	for _, c := range []struct {
		policy, algorithm string
		want              map[string]any
	}{
		{"Never", "", map[string]any{"rotationPolicy": "Never"}},
		{"Always", "ECDSA", map[string]any{"rotationPolicy": "Always", "algorithm": "ECDSA"}},
	} {
		got, _, _ := unstructured.NestedMap(certificate(c.policy, c.algorithm).Object, "spec", "privateKey")
		if !maps.Equal(got, c.want) {
			t.Errorf("certificate(%q, %q) has privateKey %v, want %v", c.policy, c.algorithm, got, c.want)
		}
	}
}
