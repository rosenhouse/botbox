package main

import (
	"cmp"
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
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

func TestParseReadsEveryFlag(t *testing.T) {
	kubeconfig := writeKubeconfig(t, "https://127.0.0.1:1")
	for _, c := range []struct {
		args []string
		want repro
		runs int
	}{
		{nil, repro{poll: 10 * time.Millisecond, settle: 2 * time.Second, timeout: 30 * time.Second, algorithm: "ECDSA", policy: "Never", propagation: metav1.DeletePropagationBackground, dnsName: "example.test"}, 10},
		{
			[]string{"-runs", "3", "-collect", "-wait", "5s", "-algorithm", "RSA", "-policy", "Always", "-propagation", "Foreground", "-pause", "200ms", "-dns", "other.test"},
			repro{poll: 10 * time.Millisecond, settle: 2 * time.Second, collect: true, timeout: 5 * time.Second, algorithm: "RSA", policy: "Always", propagation: metav1.DeletePropagationForeground, pause: 200 * time.Millisecond, dnsName: "other.test"},
			3,
		},
	} {
		r, runs, err := parse(append([]string{"-kubeconfig", kubeconfig}, c.args...))
		if err != nil || r.core == nil || r.dyn == nil {
			t.Fatalf("parse(%q) = %+v, %v; want clients for the kubeconfig", c.args, r, err)
		}
		r.core, r.dyn = nil, nil
		if r != c.want || runs != c.runs {
			t.Errorf("parse(%q) = %+v and %d runs, want %+v and %d", c.args, r, runs, c.want, c.runs)
		}
	}
}

// client-go's default limit allows 10 requests at once and then 5 a second,
// so a throttled client cannot make 50 requests in 5 s.
func TestNewReproLeavesBothClientsUnthrottled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
	}))
	defer server.Close()
	r, err := newRepro(writeKubeconfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := range 50 {
		if _, err := r.dyn.Resource(certificates).Namespace("ns").Get(ctx, "example", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("the dynamic client's get %d returned %v; a throttled client misses the window the bug needs", i, err)
		}
		if _, err := r.core.CoreV1().Secrets("ns").Get(ctx, "example-tls", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("the core client's get %d returned %v; a throttled client misses the window the bug needs", i, err)
		}
	}
}

func writeKubeconfig(t *testing.T, server string) string {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "`+server+`"}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return kubeconfig
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

func TestFindingPrintsBothSecretsHashes(t *testing.T) {
	f := finding{mechanism: "write-back", old: secret{Key: "oldkey", Cert: "oldcert"}, now: secret{Key: "newkey", Cert: "newcert"}}
	for _, hash := range []string{"oldkey", "newkey", "oldcert", "newcert"} {
		if !strings.Contains(f.String(), hash) {
			t.Errorf("%q lacks %s", f, hash)
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
	old := certificate("Always", "", "")
	old.SetNamespace("ns")
	client := fakeDynamic(old)
	gets := 0
	var gone, created time.Time
	client.PrependReactor("get", "certificates", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets < 3 {
			return true, old, nil
		}
		gone = time.Now()
		return false, nil, nil
	})
	client.PrependReactor("create", "certificates", func(k8stesting.Action) (bool, runtime.Object, error) {
		created = time.Now()
		return false, nil, nil
	})
	r := repro{dyn: client, ns: "ns", poll: time.Millisecond, policy: "Never", algorithm: "ECDSA", dnsName: "other.test", propagation: metav1.DeletePropagationForeground, pause: 50 * time.Millisecond}

	crt, err := r.recreate(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	if gets != 3 {
		t.Errorf("recreate read the old Certificate %d times, want until it was gone on the third", gets)
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
	if got, _, _ := unstructured.NestedString(crt.Object, "spec", "commonName"); got != "other.test" {
		t.Errorf("recreate created a Certificate for %q, want other.test", got)
	}
}

// The bug needs the create within about 10 ms of the delete, so recreate looks
// for the old Certificate before its first poll interval.
func TestRecreateLooksForTheOldCertificateAtOnce(t *testing.T) {
	old := certificate("Always", "", "")
	old.SetNamespace("ns")
	r := repro{dyn: fakeDynamic(old), ns: "ns", poll: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.recreate(ctx, "old"); err != nil {
		t.Errorf("recreate returned %v, want the new Certificate before a poll interval passed", err)
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

func TestSecretRecordsUIDControllerAndHashes(t *testing.T) {
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
	want := secret{UID: "secret-uid", Owner: "certificate-uid", Key: "2c70e12b7a0646f92279f427c7b38e7334d8e5389cff167a1dc30e73f826b683", Cert: "793ff64f83b41b5d467a0d017c4cb99bc56e969a028ca7ea65a8795724f16bd1"}
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
			return true, withConditions("False", "SecretMismatch", "True"), nil
		}
		return true, withConditions("True", "Ready", "False"), nil
	})
	if got := (repro{dyn: becomesReady, ns: "ns"}).ready(context.Background(), time.Minute); got != "True Ready" || gets != 3 {
		t.Errorf("ready() = %q after %d reads, want \"True Ready\" after 3", got, gets)
	}

	stuck := fakeDynamic(withConditions("False", "SecretMismatch", "True"))
	if got := (repro{dyn: stuck, ns: "ns"}).ready(context.Background(), 300*time.Millisecond); got != "False SecretMismatch, Issuing" {
		t.Errorf("ready() = %q, want the last Ready condition and the issuance, \"False SecretMismatch, Issuing\"", got)
	}

	gone := fakeDynamic()
	if got := (repro{dyn: gone, ns: "ns"}).ready(context.Background(), 300*time.Millisecond); !strings.Contains(got, "not found") {
		t.Errorf("ready() = %q, want the error that kept it from reading the Certificate", got)
	}

	noStatus := certificate("Never", "", "")
	noStatus.SetNamespace("ns")
	appears := fakeDynamic(noStatus)
	reads := 0
	appears.PrependReactor("get", "certificates", func(k8stesting.Action) (bool, runtime.Object, error) {
		if reads++; reads == 1 {
			return true, nil, apierrors.NewNotFound(certificates.GroupResource(), "example")
		}
		return false, nil, nil
	})
	if got := (repro{dyn: appears, ns: "ns"}).ready(context.Background(), 300*time.Millisecond); got != "unset" {
		t.Errorf("ready() = %q, want \"unset\" for a Certificate without conditions", got)
	}
}

// A Certificate can be Ready while an issuance replaces its Secret.
func TestReadyWaitsOutIssuancesAndTheSettlePeriod(t *testing.T) {
	gets := 0
	var lastIssuanceEnded time.Time
	client := fakeDynamic()
	client.PrependReactor("get", "certificates", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		switch gets {
		case 1, 3:
			return true, withConditions("True", "Ready", "True"), nil
		case 4:
			lastIssuanceEnded = time.Now()
		}
		return true, withConditions("True", "Ready", ""), nil
	})
	r := repro{dyn: client, ns: "ns", settle: 250 * time.Millisecond}

	got := r.ready(context.Background(), time.Minute)
	if after := time.Since(lastIssuanceEnded); got != "True Ready" || gets < 4 || after < r.settle {
		t.Errorf("ready() = %q after %d reads and %v after the last issuance ended, want \"True Ready\" at least %v after the 4th read", got, gets, after, r.settle)
	}
}

// withConditions has no Issuing condition where issuing is empty.
func withConditions(status, reason, issuing string) *unstructured.Unstructured {
	crt := certificate("Never", "", "")
	crt.SetNamespace("ns")
	conditions := []any{map[string]any{"type": "Ready", "status": status, "reason": reason}}
	if issuing != "" {
		conditions = append(conditions, map[string]any{"type": "Issuing", "status": issuing, "reason": "Other"})
	}
	crt.Object["status"] = map[string]any{"conditions": conditions}
	return crt
}

func TestCertificateSetsTheKeysPolicyAndAlgorithmAndTheDNSName(t *testing.T) {
	for _, c := range []struct {
		policy, algorithm, dnsName string
		want                       map[string]any
	}{
		{"Never", "", "", map[string]any{"rotationPolicy": "Never"}},
		{"Always", "ECDSA", "other.test", map[string]any{"rotationPolicy": "Always", "algorithm": "ECDSA"}},
	} {
		crt := certificate(c.policy, c.algorithm, c.dnsName)
		got, _, _ := unstructured.NestedMap(crt.Object, "spec", "privateKey")
		if !maps.Equal(got, c.want) {
			t.Errorf("certificate(%q, %q, %q) has privateKey %v, want %v", c.policy, c.algorithm, c.dnsName, got, c.want)
		}
		wantName := cmp.Or(c.dnsName, "example.test")
		names, _, _ := unstructured.NestedStringSlice(crt.Object, "spec", "dnsNames")
		if common, _, _ := unstructured.NestedString(crt.Object, "spec", "commonName"); common != wantName || !slices.Equal(names, []string{wantName}) {
			t.Errorf("certificate(%q, %q, %q) has commonName %q and dnsNames %v, want %s for both", c.policy, c.algorithm, c.dnsName, common, names, wantName)
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
