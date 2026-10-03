package main

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func TestNewReproLeavesTheClientUnthrottled(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := newRepro(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if limiter := r.core.CoreV1().RESTClient().GetRateLimiter(); limiter != nil {
		t.Errorf("the client is rate limited (%T); a throttled create misses the window the bug needs", limiter)
	}
}

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
	orphan := controlled("CertificateRequest", "example-3", "old")
	orphan.SetOwnerReferences(nil)
	client := fakeDynamic(controlled("Secret", "example-tls", "old"),
		controlled("CertificateRequest", "example-1", "old"),
		controlled("CertificateRequest", "example-2", "new"),
		orphan)
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

func TestCollectGarbageReportsADeleteItCannotMake(t *testing.T) {
	client := fakeDynamic(controlled("Secret", "example-tls", "old"))
	client.PrependReactor("delete", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(secrets.GroupResource(), "example-tls", nil)
	})
	if err := (repro{dyn: client, ns: "ns", collect: true}).collectGarbage(context.Background(), "old"); !apierrors.IsForbidden(err) {
		t.Errorf("collectGarbage returned %v, want the Forbidden error", err)
	}
}

func TestRecreateDeletesWithThePolicyAndCreatesAfterThePause(t *testing.T) {
	old := certificate("Always", "")
	old.SetNamespace("ns")
	client := fakeDynamic(old)
	var gone, created time.Time
	client.PrependReactor("get", "certificates", func(k8stesting.Action) (bool, runtime.Object, error) {
		gone = time.Now()
		return false, nil, nil
	})
	client.PrependReactor("create", "certificates", func(k8stesting.Action) (bool, runtime.Object, error) {
		created = time.Now()
		return false, nil, nil
	})
	r := repro{dyn: client, ns: "ns", policy: "Never", algorithm: "ECDSA", propagation: metav1.DeletePropagationForeground, pause: 50 * time.Millisecond}

	crt, err := r.recreate(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	var del k8stesting.DeleteAction
	for _, a := range client.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok {
			del = d
		}
	}
	if del == nil || del.GetDeleteOptions().PropagationPolicy == nil || *del.GetDeleteOptions().PropagationPolicy != metav1.DeletePropagationForeground {
		t.Errorf("recreate deleted with %+v, want propagation Foreground", del)
	}
	if pause := created.Sub(gone); pause < r.pause {
		t.Errorf("recreate created %v after it saw the old Certificate gone, want at least %v", pause, r.pause)
	}
	if got, _, _ := unstructured.NestedString(crt.Object, "spec", "privateKey", "algorithm"); got != "ECDSA" {
		t.Errorf("recreate created a Certificate with algorithm %q, want ECDSA", got)
	}
}

func fakeDynamic(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{secrets: "SecretList", requests: "CertificateRequestList"}, objects...)
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
	gets := 0
	becomesReady := fakeDynamic()
	becomesReady.PrependReactor("get", "certificates", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets < 3 {
			return true, withReady("False", "SecretMismatch"), nil
		}
		return true, withReady("True", "Ready"), nil
	})
	if got := (repro{dyn: becomesReady, ns: "ns"}).ready(context.Background(), time.Minute); got != "True Ready" || gets != 3 {
		t.Errorf("ready() = %q after %d reads, want \"True Ready\" after 3", got, gets)
	}

	stuck := fakeDynamic(withReady("False", "SecretMismatch"))
	if got := (repro{dyn: stuck, ns: "ns"}).ready(context.Background(), 300*time.Millisecond); got != "False SecretMismatch" {
		t.Errorf("ready() = %q, want the last Ready condition, \"False SecretMismatch\"", got)
	}

	gone := fakeDynamic()
	if got := (repro{dyn: gone, ns: "ns"}).ready(context.Background(), 300*time.Millisecond); !strings.Contains(got, "not found") {
		t.Errorf("ready() = %q, want the error that kept it from reading the Certificate", got)
	}
}

func withReady(status, reason string) *unstructured.Unstructured {
	crt := certificate("Never", "")
	crt.SetNamespace("ns")
	crt.Object["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": "Ready", "status": status, "reason": reason},
		map[string]any{"type": "Issuing", "status": "True", "reason": "Other"},
	}}
	return crt
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

func TestSecretOnceThereWaitsForTheSecret(t *testing.T) {
	client := kubefake.NewClientset()
	gets := 0
	client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets < 3 {
			return true, nil, apierrors.NewNotFound(corev1.Resource("secrets"), "example-tls")
		}
		return true, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "example-tls", Namespace: "ns", UID: "secret-uid"}}, nil
	})
	if got, err := (repro{core: client, ns: "ns", timeout: time.Minute}).secretOnceThere(context.Background()); err != nil || got.UID != "secret-uid" {
		t.Errorf("secretOnceThere() = %+v, %v; want the Secret once it exists", got, err)
	}
	if got, err := (repro{core: kubefake.NewClientset(), ns: "ns", timeout: 300 * time.Millisecond}).secretOnceThere(context.Background()); err != nil || got != (secret{}) {
		t.Errorf("secretOnceThere() without a Secret = %+v, %v; want none and no error", got, err)
	}
}
