package main

import (
	"cmp"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
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
		{nil, repro{poll: 10 * time.Millisecond, settle: 2 * time.Second, setupWait: time.Minute, timeout: 30 * time.Second, algorithm: "ECDSA", policy: "Never", propagation: metav1.DeletePropagationBackground, dnsName: "example.test"}, 10},
		{
			[]string{"-runs", "3", "-collect", "-wait", "5s", "-algorithm", "RSA", "-policy", "Always", "-propagation", "Foreground", "-pause", "200ms", "-dns", "other.test"},
			repro{poll: 10 * time.Millisecond, settle: 2 * time.Second, setupWait: time.Minute, collect: true, timeout: 5 * time.Second, algorithm: "RSA", policy: "Always", propagation: metav1.DeletePropagationForeground, pause: 200 * time.Millisecond, dnsName: "other.test"},
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

func TestReportPrintsEachRunAndCountsMechanisms(t *testing.T) {
	found := []finding{{mechanism: "write-back"}, {mechanism: "new key"}, {mechanism: "write-back"}}
	var out strings.Builder
	runs := 0
	err := report(&out, len(found), func() (finding, error) {
		runs++
		return found[runs-1], nil
	})
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if err != nil || len(lines) != 4 || !strings.HasPrefix(lines[1], "run 2: new key.") || lines[3] != "map[new key:1 write-back:2]" {
		t.Errorf("report() printed\n%s\nand returned %v; want a line for each run, then map[new key:1 write-back:2]", out.String(), err)
	}
}

func TestReportStopsAtARunsError(t *testing.T) {
	runs := 0
	err := report(io.Discard, 3, func() (finding, error) {
		runs++
		return finding{}, errors.New("no Issuer")
	})
	if err == nil || err.Error() != "no Issuer" || runs != 1 {
		t.Errorf("report() made %d runs and returned %v, want 1 run and the run's error", runs, err)
	}
}

// main exits, so it runs in a child process.
func TestMainExitsWithWhatStoppedIt(t *testing.T) {
	if args := os.Getenv("REPRO_ARGS"); args != "" {
		os.Args = append([]string{"repro"}, strings.Fields(args)...)
		main()
		return
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","message":"namespaces is forbidden","code":403}`))
	}))
	defer server.Close()
	forbidden := writeKubeconfig(t, server.URL)
	for _, c := range []struct {
		args, want string
		exit       int
	}{
		{"-kubeconfig " + forbidden + " -runs 0", "map[]", 0},
		{"-h", "-collect", 0},
		{"-kubeconfig " + filepath.Join(t.TempDir(), "missing") + " -runs 0", "no such file", 1},
		{"-kubeconfig " + forbidden + " -runs 1", "namespaces is forbidden", 1},
	} {
		child := exec.Command(os.Args[0], "-test.run=^TestMainExitsWithWhatStoppedIt$")
		child.Env = append(os.Environ(), "REPRO_ARGS="+c.args, "KUBECONFIG=", "HOME="+t.TempDir(), "KUBERNETES_SERVICE_HOST=")
		out, err := child.CombinedOutput()
		exit := 0
		if failed := (*exec.ExitError)(nil); errors.As(err, &failed) {
			exit = failed.ExitCode()
		}
		if exit != c.exit || !strings.Contains(string(out), c.want) {
			t.Errorf("main with %s exited %d and wrote\n%s\nwant exit %d and %q", c.args, exit, out, c.exit, c.want)
		}
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

func TestFindingPrintsWhatTheRunSaw(t *testing.T) {
	f := finding{"write-back", "oldcert1-uid", "newcert2-uid", "True Ready",
		secret{"oldsecrt-uid", "oldcert1-uid", "oldkeyhash12-rest", "oldcrthash12-rest"},
		secret{"newsecrt-uid", "owner123-uid", "newkeyhash12-rest", "newcrthash12-rest"}}
	want := "write-back. Certificate oldcert1, then newcert2, Ready True Ready. Secret oldsecrt, then newsecrt owned by owner123. " +
		"tls.key sha256 oldkeyhash12, then newkeyhash12. tls.crt sha256 oldcrthash12, then newcrthash12."
	if got := f.String(); got != want {
		t.Errorf("finding prints\n%s\nwant\n%s", got, want)
	}
}

func TestCollectGarbageDeletesWhatTheOldCertificateControls(t *testing.T) {
	orphan := controlled("CertificateRequest", "example-3", "old")
	orphan.SetOwnerReferences(nil)
	adopted := controlled("CertificateRequest", "example-4", "new")
	adopted.SetOwnerReferences(append([]metav1.OwnerReference{{APIVersion: "cert-manager.io/v1", Kind: "Certificate", Name: "example", UID: "old"}}, adopted.GetOwnerReferences()...))
	client := fakeDynamic(controlled("Secret", "example-tls", "old"),
		controlled("CertificateRequest", "example-1", "old"),
		controlled("CertificateRequest", "example-2", "new"),
		orphan, adopted)
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
	r := repro{dyn: client, ns: "ns", poll: time.Millisecond, setupWait: time.Minute, policy: "Never", algorithm: "ECDSA", dnsName: "other.test", propagation: metav1.DeletePropagationForeground, pause: 50 * time.Millisecond}

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

func TestRecreateReadsAgainAfterAReadFails(t *testing.T) {
	old := certificate("Always", "", "")
	old.SetNamespace("ns")
	client := fakeDynamic(old)
	reads, readsBeforeCreate := 0, 0
	client.PrependReactor("get", "certificates", func(k8stesting.Action) (bool, runtime.Object, error) {
		if reads++; reads == 1 {
			return true, nil, apierrors.NewInternalError(errors.New("etcd timed out"))
		}
		return false, nil, nil
	})
	client.PrependReactor("create", "certificates", func(k8stesting.Action) (bool, runtime.Object, error) {
		readsBeforeCreate = reads
		return false, nil, nil
	})
	r := repro{dyn: client, ns: "ns", poll: time.Millisecond, setupWait: time.Minute}
	if _, err := r.recreate(context.Background(), "old"); err != nil || readsBeforeCreate != 2 {
		t.Errorf("recreate returned %v and created after %d reads, want the new Certificate after the second read found the old one gone", err, readsBeforeCreate)
	}
}

func TestRecreateCreatesNothingWhileTheOldCertificateRemains(t *testing.T) {
	old := certificate("Always", "", "")
	old.SetNamespace("ns")
	client := fakeDynamic(old)
	client.PrependReactor("delete", "certificates", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	_, err := (repro{dyn: client, ns: "ns", poll: time.Millisecond, setupWait: 50 * time.Millisecond}).recreate(ctx, "old")
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" {
			t.Errorf("recreate created a Certificate while the old one remained, and returned %v", err)
		}
	}
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Errorf("recreate returned %v, and its context ended: %v; want it to stop after setupWait", err, ctx.Err())
	}
}

func TestRecreateCollectsWhatTheOldCertificateControls(t *testing.T) {
	old := certificate("Always", "", "")
	old.SetNamespace("ns")
	client := fakeDynamic(old, controlled("Secret", "example-tls", "old"))
	r := repro{dyn: client, ns: "ns", poll: time.Millisecond, collect: true}
	if _, err := r.recreate(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Tracker().Get(secrets, "ns", "example-tls"); !apierrors.IsNotFound(err) {
		t.Errorf("after recreate under -collect, getting the old Certificate's Secret returned %v, want NotFound", err)
	}
}

func TestRunReadsEachSecretOnceItsCertificateSettles(t *testing.T) {
	for _, kept := range []string{"write-back", "re-point"} {
		t.Run(kept, func(t *testing.T) {
			cluster := newFakeCertManager(kept, 50*time.Millisecond)
			r := cluster.repro(5 * time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			got, err := r.run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(cluster.early) > 0 {
				t.Errorf("run read the Secret while %v had not settled", cluster.early)
			}
			wantNow := map[string]string{"write-back": "secret-2", "re-point": "secret-1"}[kept]
			if got.mechanism != kept || got.oldCertificate != "certificate-1" || got.newCertificate != "certificate-2" || got.ready != "True Ready" || got.old.UID != "secret-1" || got.now.UID != wantNow {
				t.Errorf("run() = %v, want %s from certificate-1 to certificate-2, Ready True Ready, Secret secret-1 then %s", got, kept, wantNow)
			}
			for i, want := range []*unstructured.Unstructured{certificate("Always", "", "example.test"), certificate("Always", "ECDSA", "other.test")} {
				if got := cluster.created[i].Object["spec"]; !reflect.DeepEqual(got, want.Object["spec"]) {
					t.Errorf("run created Certificate %d with spec %v, want %v", i+1, got, want.Object["spec"])
				}
			}
			for _, a := range slices.Concat(cluster.core.Actions(), cluster.dyn.Actions()) {
				if a.GetResource().Resource != "namespaces" && a.GetNamespace() != "write-back-1" {
					t.Errorf("run sent %s %s to namespace %q, want write-back-1", a.GetVerb(), a.GetResource().Resource, a.GetNamespace())
				}
			}
			if _, err := cluster.core.Tracker().Get(corev1.SchemeGroupVersion.WithResource("namespaces"), "", "write-back-1"); !apierrors.IsNotFound(err) {
				t.Errorf("after run, getting its namespace returned %v, want NotFound", err)
			}
		})
	}
}

func TestRunCollectsWhatTheFirstCertificateControlsOnceItIsDeleted(t *testing.T) {
	cluster := newFakeCertManager("write-back", 50*time.Millisecond)
	for _, o := range []*unstructured.Unstructured{
		controlled("Secret", "example-tls", "certificate-1"),
		controlled("CertificateRequest", "example-1", "certificate-1"),
		controlled("CertificateRequest", "other-1", "other"),
	} {
		o.SetNamespace("write-back-1")
		if err := cluster.dyn.Tracker().Add(o); err != nil {
			t.Fatal(err)
		}
	}
	r := cluster.repro(5 * time.Second)
	r.collect = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	var collected []string
	certificateDeleted := false
	for _, a := range cluster.dyn.Actions() {
		switch del, ok := a.(k8stesting.DeleteAction); {
		case !ok:
		case del.GetResource() == certificates:
			certificateDeleted = true
		case !certificateDeleted:
			t.Errorf("run deleted %s %s before the Certificate", del.GetResource().Resource, del.GetName())
		default:
			collected = append(collected, del.GetName())
		}
	}
	if want := []string{"example-tls", "example-1"}; !slices.Equal(collected, want) {
		t.Errorf("run collected %v, want %v", collected, want)
	}
}

func TestRunReturnsTheErrorOfACallThatFails(t *testing.T) {
	for _, c := range []struct {
		call, verb, resource string
		// The first such call fails once created Certificates exist.
		created int
		collect bool
	}{
		{"namespace create", "create", "namespaces", 0, false},
		{"Issuer create", "create", "issuers", 0, false},
		{"first Certificate create", "create", "certificates", 0, false},
		{"old Secret read", "get", "secrets", 1, false},
		{"Certificate delete", "delete", "certificates", 1, false},
		{"new Certificate create", "create", "certificates", 1, false},
		{"collector's list", "list", "secrets", 1, true},
		{"new Secret read", "get", "secrets", 2, false},
	} {
		t.Run(c.call, func(t *testing.T) {
			cluster := newFakeCertManager("write-back", 50*time.Millisecond)
			failed := false
			fail := func(k8stesting.Action) (bool, runtime.Object, error) {
				if failed || len(cluster.created) < c.created {
					return false, nil, nil
				}
				failed = true
				return true, nil, apierrors.NewForbidden(corev1.Resource(c.resource), "", errors.New("denied"))
			}
			cluster.core.PrependReactor(c.verb, c.resource, fail)
			cluster.dyn.PrependReactor(c.verb, c.resource, fail)
			r := cluster.repro(5 * time.Second)
			r.collect = c.collect
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if _, err := r.run(ctx); !apierrors.IsForbidden(err) {
				t.Errorf("run() returned %v, want the Forbidden error", err)
			}
		})
	}
}

func TestRunStopsWhenTheFirstCertificateIsNotReady(t *testing.T) {
	cluster := newFakeCertManager("write-back", 50*time.Millisecond)
	cluster.dyn.PrependReactor("create", "issuers", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	r := cluster.repro(5 * time.Second)
	r.setupWait = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	if _, err := r.run(ctx); err == nil || err.Error() != "the first Certificate is not Ready: False IssuerNotFound" || ctx.Err() != nil {
		t.Errorf("run() returned %v, and its context ended: %v; want the first Certificate's Ready condition after setupWait", err, ctx.Err())
	}
}

// -wait bounds the wait for the new Certificate. setupWait bounds the first
// one's.
func TestRunWaitsForTheNewCertificateAsLongAsWaitSays(t *testing.T) {
	cluster := newFakeCertManager("never Ready", 50*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := cluster.repro(150 * time.Millisecond).run(ctx)
	if err != nil || got.ready != "False SecretMismatch, Issuing" || ctx.Err() != nil {
		t.Errorf("run() = %v, %v, and its context ended: %v; want the new Certificate's last Ready condition after -wait", got, err, ctx.Err())
	}
}

// fakeCertManager plays cert-manager and a garbage collector. A Certificate
// whose selfSigned Issuer exists is Ready with an issuance in flight on its
// first two reads, and settles from then on. Under "never Ready", the second
// one waits for a user instead. The Secret is missing until the first
// Certificate settles. It holds a new key until the second settles, is missing
// on the next read, and then holds the old key as kept says.
type fakeCertManager struct {
	core        *kubefake.Clientset
	dyn         *dynamicfake.FakeDynamicClient
	settle      time.Duration
	created     []*unstructured.Unstructured
	reads       map[types.UID]int
	settled     map[types.UID]time.Time
	early       []types.UID
	wentMissing bool
}

func newFakeCertManager(kept string, settle time.Duration) *fakeCertManager {
	f := &fakeCertManager{core: kubefake.NewClientset(), dyn: fakeDynamic(), settle: settle, reads: map[types.UID]int{}, settled: map[types.UID]time.Time{}}
	f.core.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ns := action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace)
		ns.Name = ns.GenerateName + "1"
		return false, nil, nil
	})
	f.dyn.PrependReactor("create", "certificates", func(action k8stesting.Action) (bool, runtime.Object, error) {
		crt := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		crt.SetUID(types.UID("certificate-" + strconv.Itoa(len(f.created)+1)))
		f.created = append(f.created, crt.DeepCopy())
		return false, nil, nil
	})
	f.dyn.PrependReactor("get", "certificates", func(action k8stesting.Action) (bool, runtime.Object, error) {
		o, err := f.dyn.Tracker().Get(certificates, action.GetNamespace(), "example")
		if err != nil {
			return true, nil, err
		}
		crt := o.(*unstructured.Unstructured)
		issuerName, _, _ := unstructured.NestedString(crt.Object, "spec", "issuerRef", "name")
		selfSigned := false
		if issuer, err := f.dyn.Tracker().Get(issuers, action.GetNamespace(), issuerName); err == nil {
			_, selfSigned, _ = unstructured.NestedMap(issuer.(*unstructured.Unstructured).Object, "spec", "selfSigned")
		}
		f.reads[crt.GetUID()]++
		ready, reason, issuing := "True", "Ready", f.reads[crt.GetUID()] <= 2
		switch {
		case !selfSigned:
			ready, reason, issuing = "False", "IssuerNotFound", false
		case kept == "never Ready" && crt.GetUID() == "certificate-2":
			ready, reason, issuing = "False", "SecretMismatch", true
		case !issuing && f.settled[crt.GetUID()].IsZero():
			f.settled[crt.GetUID()] = time.Now()
		}
		crt.Object["status"] = map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": ready, "reason": reason},
			map[string]any{"type": "Issuing", "status": map[bool]string{true: "True", false: "False"}[issuing]},
		}}
		return true, crt, nil
	})
	f.core.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		current := f.created[len(f.created)-1].GetUID()
		settled := !f.settled[current].IsZero() && time.Since(f.settled[current]) >= f.settle
		if !settled {
			f.early = append(f.early, current)
		}
		switch {
		case current == "certificate-1" && settled:
			return true, tlsSecret("secret-1", "certificate-1", "old key"), nil
		case current == "certificate-1":
			return true, nil, apierrors.NewNotFound(corev1.Resource("secrets"), "example-tls")
		case !settled:
			return true, tlsSecret("secret-2", "certificate-2", "new key"), nil
		case !f.wentMissing:
			f.wentMissing = true
			return true, nil, apierrors.NewNotFound(corev1.Resource("secrets"), "example-tls")
		case kept == "write-back":
			return true, tlsSecret("secret-2", "certificate-2", "old key"), nil
		}
		return true, tlsSecret("secret-1", "certificate-2", "old key"), nil
	})
	return f
}

func (f *fakeCertManager) repro(timeout time.Duration) repro {
	return repro{core: f.core, dyn: f.dyn, poll: time.Millisecond, settle: f.settle, setupWait: time.Minute, timeout: timeout,
		policy: "Always", algorithm: "ECDSA", dnsName: "other.test", propagation: metav1.DeletePropagationBackground}
}

func tlsSecret(uid, owner types.UID, key string) *corev1.Secret {
	controller := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "example-tls", Namespace: "write-back-1", UID: uid, OwnerReferences: []metav1.OwnerReference{{UID: owner, Controller: &controller}}},
		Data:       map[string][]byte{"tls.key": []byte(key)},
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
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	if got, err := (repro{core: kubefake.NewClientset(), ns: "ns", timeout: 100 * time.Millisecond}).secretOnceThere(ctx); err != nil || got != (secret{}) || ctx.Err() != nil {
		t.Errorf("secretOnceThere() without a Secret = %+v, %v, and its context ended: %v; want none and no error after -wait", got, err, ctx.Err())
	}
}
