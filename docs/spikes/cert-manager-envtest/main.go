// smoke: run the cert-manager controller as a black-box binary against envtest,
// drive a self-signed Issuer + Certificate through create/update/restart/delete,
// and report what a harness would observe.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	certGVR   = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	issuerGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "issuers"}
	crGVR     = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificaterequests"}
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		os.Exit(1)
	}
}

func dumpLog(path string) {
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	fmt.Println("---- controller log (tail) ----")
	fmt.Println(strings.Join(lines, "\n"))
}

func main() {
	if len(os.Args) < 4 {
		fmt.Println("usage: smoke <crds.yaml> <controller-binary> <owner-ref true|false>")
		os.Exit(2)
	}
	crds, bin, ownerRef := os.Args[1], os.Args[2], os.Args[3]

	env := &envtest.Environment{CRDDirectoryPaths: []string{crds}, ErrorIfCRDPathMissing: true}
	t0 := time.Now()
	cfg, err := env.Start()
	must(err)
	defer func() { _ = env.Stop() }()
	fmt.Printf("envtest up in %s (host %s)\n", time.Since(t0).Round(time.Millisecond), cfg.Host)

	user, err := env.ControlPlane.AddUser(envtest.User{Name: "cert-manager", Groups: []string{"system:masters"}}, nil)
	must(err)
	kc, err := user.KubeConfig()
	must(err)
	dir, err := os.MkdirTemp("", "cm-smoke")
	must(err)
	kcPath := filepath.Join(dir, "kubeconfig")
	must(os.WriteFile(kcPath, kc, 0o600))

	logPath := filepath.Join(dir, "controller.log")
	logf, err := os.Create(logPath)
	must(err)
	fmt.Println("controller log:", logPath)

	args := []string{
		"--kubeconfig=" + kcPath,
		"--leader-elect=false",
		"--enable-certificate-owner-ref=" + ownerRef,
		"--metrics-listen-address=127.0.0.1:19402",
		"--v=2",
	}
	start := func() *exec.Cmd {
		c := exec.Command(bin, args...)
		c.Stdout, c.Stderr = logf, logf
		must(c.Start())
		return c
	}
	cmd := start()
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	t1 := time.Now()

	ctx := context.Background()
	kube, err := kubernetes.NewForConfig(cfg)
	must(err)
	dyn, err := dynamic.NewForConfig(cfg)
	must(err)

	ns := "run-1"
	_, err = kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	must(err)

	issuer := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "Issuer",
		"metadata": map[string]any{"name": "selfsigned", "namespace": ns},
		"spec":     map[string]any{"selfSigned": map[string]any{}},
	}}
	_, err = dyn.Resource(issuerGVR).Namespace(ns).Create(ctx, issuer, metav1.CreateOptions{})
	must(err)

	cert := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
		"metadata": map[string]any{"name": "c1", "namespace": ns},
		"spec": map[string]any{
			"secretName": "c1-tls",
			"commonName": "example.test",
			"dnsNames":   []any{"example.test"},
			"issuerRef":  map[string]any{"name": "selfsigned", "kind": "Issuer"},
		},
	}}
	_, err = dyn.Resource(certGVR).Namespace(ns).Create(ctx, cert, metav1.CreateOptions{})
	must(err)

	ready := func(u *unstructured.Unstructured) bool {
		conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		for _, c := range conds {
			m, _ := c.(map[string]any)
			if m["type"] == "Ready" && m["status"] == "True" {
				og, _, _ := unstructured.NestedInt64(m, "observedGeneration")
				return og == u.GetGeneration()
			}
		}
		return false
	}
	waitReady := func(label string, timeout time.Duration) *unstructured.Unstructured {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			u, err := dyn.Resource(certGVR).Namespace(ns).Get(ctx, "c1", metav1.GetOptions{})
			must(err)
			if ready(u) {
				fmt.Printf("%s: Ready after %s (generation %d)\n", label, time.Since(t1).Round(time.Millisecond), u.GetGeneration())
				return u
			}
			time.Sleep(250 * time.Millisecond)
		}
		fmt.Printf("%s: NOT ready within %s\n", label, timeout)
		dumpLog(logPath)
		os.Exit(1)
		return nil
	}

	listManaged := func(tag string) (secrets, crs int) {
		secs, err := kube.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		must(err)
		for _, s := range secs.Items {
			fmt.Printf("  [%s] secret %s labels=%v ownerRefs=%d\n", tag, s.Name, s.Labels, len(s.OwnerReferences))
		}
		list, err := dyn.Resource(crGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		must(err)
		for _, c := range list.Items {
			fmt.Printf("  [%s] certificaterequest %s ownerRefs=%d\n", tag, c.GetName(), len(c.GetOwnerReferences()))
		}
		return len(secs.Items), len(list.Items)
	}

	u := waitReady("initial issuance", 120*time.Second)
	rev, _, _ := unstructured.NestedInt64(u.Object, "status", "revision")
	fmt.Println("status.revision:", rev)
	listManaged("after issuance")

	// Churn check: do resourceVersions stay put once converged? (G2 proxy)
	rvs := map[string]string{}
	churn := 0
	for i := 0; i < 10; i++ {
		u, err := dyn.Resource(certGVR).Namespace(ns).Get(ctx, "c1", metav1.GetOptions{})
		must(err)
		s, err := kube.CoreV1().Secrets(ns).Get(ctx, "c1-tls", metav1.GetOptions{})
		must(err)
		for k, v := range map[string]string{"certificate/c1": u.GetResourceVersion(), "secret/c1-tls": s.ResourceVersion} {
			if old, ok := rvs[k]; ok && old != v {
				churn++
				fmt.Printf("  churn: %s %s -> %s\n", k, old, v)
			}
			rvs[k] = v
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Printf("resourceVersion changes over 20s of stable spec: %d\n", churn)

	// Spec update: add a DNS name, expect re-issuance and a new revision.
	t1 = time.Now()
	u, err = dyn.Resource(certGVR).Namespace(ns).Get(ctx, "c1", metav1.GetOptions{})
	must(err)
	must(unstructured.SetNestedStringSlice(u.Object, []string{"example.test", "www.example.test"}, "spec", "dnsNames"))
	_, err = dyn.Resource(certGVR).Namespace(ns).Update(ctx, u, metav1.UpdateOptions{})
	must(err)
	u = waitReady("after dnsNames update", 120*time.Second)
	rev, _, _ = unstructured.NestedInt64(u.Object, "status", "revision")
	fmt.Println("status.revision:", rev)
	s1, c1 := listManaged("after update")

	// Restart: SIGKILL + re-exec, then confirm the converged state is unchanged (G5 proxy).
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	t1 = time.Now()
	cmd = start()
	time.Sleep(15 * time.Second)
	u = waitReady("after restart", 60*time.Second)
	rev2, _, _ := unstructured.NestedInt64(u.Object, "status", "revision")
	s2, c2 := listManaged("after restart")
	fmt.Printf("restart-stable: revision %d -> %d, secrets %d -> %d, certificaterequests %d -> %d\n", rev, rev2, s1, s2, c1, c2)

	// Delete: what remains? envtest runs no kube-controller-manager, so ownerRef GC does not happen.
	must(dyn.Resource(certGVR).Namespace(ns).Delete(ctx, "c1", metav1.DeleteOptions{}))
	time.Sleep(10 * time.Second)
	_, err = dyn.Resource(certGVR).Namespace(ns).Get(ctx, "c1", metav1.GetOptions{})
	fmt.Println("certificate gone after delete:", err != nil)
	listManaged("10s after delete (no GC in envtest)")

	// Namespace deletion: also needs the controller-manager; show what happens.
	must(kube.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}))
	time.Sleep(5 * time.Second)
	nsObj, err := kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		fmt.Printf("namespace %s still present 5s after delete, phase=%s\n", ns, nsObj.Status.Phase)
	} else {
		fmt.Println("namespace gone:", err)
	}
	fmt.Println("OK")
}
