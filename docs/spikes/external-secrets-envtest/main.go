// smoke: run the external-secrets controller as a black-box binary against
// envtest, drive a fake-provider SecretStore + ExternalSecret through
// create/update/restart/delete, and report what a harness would observe. A
// counting reverse proxy in front of the API server records the traffic the
// controller makes, which is what G1 and G2 judge.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	storeGVR = schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "secretstores"}
	esGVR    = schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"}
)

const (
	namespace  = "run-1"
	storeName  = "fake"
	esName     = "example"
	secretName = "example-secret"
)

var (
	// The defaults are the CRD's own for spec.target, and the refresh policy
	// this spike recommends. Each flag names one thing to vary from there.
	creationPolicy  = flag.String("creation-policy", "Owner", "spec.target.creationPolicy of the ExternalSecret")
	deletionPolicy  = flag.String("deletion-policy", "Retain", "spec.target.deletionPolicy of the ExternalSecret")
	refreshInterval = flag.String("refresh-interval", "1h0m0s", "spec.refreshInterval of the ExternalSecret")
	refreshPolicy   = flag.String("refresh-policy", "OnChange", "spec.refreshPolicy of the ExternalSecret; empty leaves it unset")
	quiet           = flag.Duration("quiet", 30*time.Second, "how long to watch a converged ExternalSecret for writes")
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		os.Exit(1)
	}
}

// counter is the request log of DESIGN.md §5.2, cut down to what this spike
// reports: the verb and resource of every request the controller makes.
type counter struct {
	mu   sync.Mutex
	log  []string
	url  string
	from int
}

func startProxy(cfg *rest.Config) *counter {
	transport, err := rest.TransportFor(cfg)
	must(err)
	upstream, _, err := rest.DefaultServerUrlFor(cfg)
	must(err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)

	c := &counter{url: "http://" + listener.Addr().String()}
	reverse := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.Header.Del("Authorization")
			r.SetURL(upstream)
		},
		Transport:     transport,
		FlushInterval: -1,
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.record(r)
		reverse.ServeHTTP(w, r)
	})
	go http.Serve(listener, handler)
	return c
}

func (c *counter) record(r *http.Request) {
	verb := strings.ToLower(r.Method)
	if r.URL.Query().Get("watch") == "true" {
		verb = "watch"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, verb+" "+r.URL.Path)
}

// mark starts a new window. since reports what arrived after the last mark.
func (c *counter) mark() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.from = len(c.log)
}

func (c *counter) since() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.log[c.from:]...)
}

// writes are the requests G2 counts: everything that is not a read.
func writes(requests []string) []string {
	var out []string
	for _, r := range requests {
		if strings.HasPrefix(r, "get ") || strings.HasPrefix(r, "watch ") {
			continue
		}
		out = append(out, r)
	}
	return out
}

func tally(requests []string) string {
	counts := map[string]int{}
	for _, r := range requests {
		counts[r]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "\n    %3d  %s", counts[k], k)
	}
	if b.Len() == 0 {
		return " none"
	}
	return b.String()
}

func writeKubeconfig(path, server string) {
	const name = "smoke"
	config := clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{name: {Server: server}},
		Contexts:       map[string]*clientcmdapi.Context{name: {Cluster: name}},
		CurrentContext: name,
	}
	must(clientcmd.WriteToFile(config, path))
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
	flag.Parse()
	if flag.NArg() < 2 {
		fmt.Println("usage: smoke [flags] <crds.yaml> <controller-binary>")
		flag.PrintDefaults()
		os.Exit(2)
	}
	crds, bin := flag.Arg(0), flag.Arg(1)

	env := &envtest.Environment{CRDDirectoryPaths: []string{crds}, ErrorIfCRDPathMissing: true}
	t0 := time.Now()
	cfg, err := env.Start()
	must(err)
	defer func() { _ = env.Stop() }()
	fmt.Printf("envtest up in %s (host %s)\n", time.Since(t0).Round(time.Millisecond), cfg.Host)

	proxy := startProxy(cfg)
	dir, err := os.MkdirTemp("", "eso-smoke")
	must(err)
	kcPath := filepath.Join(dir, "kubeconfig")
	writeKubeconfig(kcPath, proxy.url)
	fmt.Println("proxy:", proxy.url)

	logPath := filepath.Join(dir, "controller.log")
	logf, err := os.Create(logPath)
	must(err)
	fmt.Println("controller log:", logPath)

	// There is no --kubeconfig flag: the controller calls
	// controller-runtime's GetConfigOrDie, which reads $KUBECONFIG.
	args := []string{
		"--enable-leader-election=false",
		"--metrics-addr=127.0.0.1:0",
		"--live-addr=127.0.0.1:0",
		"--loglevel=debug",
	}
	start := func() *exec.Cmd {
		c := exec.Command(bin, args...)
		c.Env = append(os.Environ(), "KUBECONFIG="+kcPath)
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

	_, err = kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
	must(err)

	store := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "external-secrets.io/v1", "kind": "SecretStore",
		"metadata": map[string]any{"name": storeName, "namespace": namespace},
		"spec": map[string]any{"provider": map[string]any{"fake": map[string]any{
			"data": []any{
				map[string]any{"key": "/token", "value": "s3cr3t"},
				map[string]any{"key": "/other", "value": "other-value"},
			},
		}}},
	}}
	_, err = dyn.Resource(storeGVR).Namespace(namespace).Create(ctx, store, metav1.CreateOptions{})
	must(err)

	es := func(key, target string) *unstructured.Unstructured {
		spec := map[string]any{
			"refreshInterval": *refreshInterval,
			"secretStoreRef":  map[string]any{"name": storeName, "kind": "SecretStore"},
			"target": map[string]any{
				"name":           target,
				"creationPolicy": *creationPolicy,
				"deletionPolicy": *deletionPolicy,
			},
			"data": []any{map[string]any{
				"secretKey": "token",
				"remoteRef": map[string]any{"key": key},
			}},
		}
		if *refreshPolicy != "" {
			spec["refreshPolicy"] = *refreshPolicy
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret",
			"metadata": map[string]any{"name": esName, "namespace": namespace},
			"spec":     spec,
		}}
	}
	target := secretName
	_, err = dyn.Resource(esGVR).Namespace(namespace).Create(ctx, es("/token", target), metav1.CreateOptions{})
	must(err)

	// Readiness must also prove the controller saw this generation. An
	// ExternalSecret carries no observedGeneration; status.syncedResourceVersion
	// is "<generation>-<hash of labels and annotations>" (DESIGN.md §8.4).
	ready := func(u *unstructured.Unstructured) bool {
		synced, _, _ := unstructured.NestedString(u.Object, "status", "syncedResourceVersion")
		if !strings.HasPrefix(synced, fmt.Sprintf("%d-", u.GetGeneration())) {
			return false
		}
		conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		for _, c := range conds {
			m, _ := c.(map[string]any)
			if m["type"] == "Ready" && m["status"] == "True" {
				return true
			}
		}
		return false
	}
	waitReady := func(label string, timeout time.Duration) *unstructured.Unstructured {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			u, err := dyn.Resource(esGVR).Namespace(namespace).Get(ctx, esName, metav1.GetOptions{})
			must(err)
			if ready(u) {
				fmt.Printf("%s: Ready after %s (generation %d)\n", label, time.Since(t1).Round(time.Millisecond), u.GetGeneration())
				return u
			}
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Printf("%s: NOT ready within %s\n", label, timeout)
		if cur, err := dyn.Resource(esGVR).Namespace(namespace).Get(ctx, esName, metav1.GetOptions{}); err == nil {
			status, _, _ := unstructured.NestedMap(cur.Object, "status")
			b, _ := json.Marshal(status)
			fmt.Printf("  generation %d, status %s\n", cur.GetGeneration(), b)
		}
		dumpLog(logPath)
		os.Exit(1)
		return nil
	}

	showStatus := func(u *unstructured.Unstructured) {
		status, _, _ := unstructured.NestedMap(u.Object, "status")
		b, _ := json.Marshal(status)
		fmt.Printf("  externalsecret status: %s\n", b)
		fmt.Printf("  externalsecret finalizers=%v\n", u.GetFinalizers())
	}

	listSecrets := func(tag string) []corev1.Secret {
		secrets, err := kube.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
		must(err)
		for _, s := range secrets.Items {
			keys := make([]string, 0, len(s.Data))
			for k, v := range s.Data {
				keys = append(keys, k+"="+string(v))
			}
			sort.Strings(keys)
			owners := make([]string, 0, len(s.OwnerReferences))
			for _, o := range s.OwnerReferences {
				owners = append(owners, fmt.Sprintf("%s/%s", o.Kind, o.Name))
			}
			fmt.Printf("  [%s] secret %s keys=%v labels=%v annotations=%v ownerRefs=%v\n",
				tag, s.Name, keys, s.Labels, s.Annotations, owners)
		}
		if len(secrets.Items) == 0 {
			fmt.Printf("  [%s] no secrets\n", tag)
		}
		return secrets.Items
	}

	u := waitReady("initial sync", 120*time.Second)
	showStatus(u)
	listSecrets("after sync")
	fmt.Printf("listening sockets:%s\n", listeningSockets(cmd.Process.Pid))
	fmt.Printf("everything in the namespace:%s\n", inventory(ctx, cfg, dyn))

	// Quiescence: with nothing changing, does the controller keep writing?
	// The fixture SecretStore is watched too, because G1 counts every request
	// the target makes, including one against a fixture.
	rv := func() map[string]string {
		out := map[string]string{}
		if o, err := dyn.Resource(esGVR).Namespace(namespace).Get(ctx, esName, metav1.GetOptions{}); err == nil {
			out["externalsecret"] = o.GetResourceVersion()
		}
		if o, err := dyn.Resource(storeGVR).Namespace(namespace).Get(ctx, storeName, metav1.GetOptions{}); err == nil {
			out["secretstore"] = o.GetResourceVersion()
		}
		if o, err := kube.CoreV1().Secrets(namespace).Get(ctx, target, metav1.GetOptions{}); err == nil {
			out["secret"] = o.ResourceVersion
		}
		return out
	}
	proxy.mark()
	seen := rv()
	churn := 0
	deadline := time.Now().Add(*quiet)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		for k, v := range rv() {
			if old, ok := seen[k]; ok && old != v {
				churn++
				fmt.Printf("  churn: %s %s -> %s\n", k, old, v)
			}
			seen[k] = v
		}
	}
	quietRequests := proxy.since()
	fmt.Printf("over %s of a stable spec: %d resourceVersion changes, %d requests, %d of them writes\n",
		*quiet, churn, len(quietRequests), len(writes(quietRequests)))
	fmt.Printf("  writes in the quiet window:%s\n", tally(writes(quietRequests)))

	update := func(label, key, newTarget string) {
		proxy.mark()
		t1 = time.Now()
		target = newTarget
		next := es(key, target)
		cur, err := dyn.Resource(esGVR).Namespace(namespace).Get(ctx, esName, metav1.GetOptions{})
		must(err)
		next.SetResourceVersion(cur.GetResourceVersion())
		next.SetFinalizers(cur.GetFinalizers())
		_, err = dyn.Resource(esGVR).Namespace(namespace).Update(ctx, next, metav1.UpdateOptions{})
		must(err)
		u = waitReady(label, 120*time.Second)
		showStatus(u)
		listSecrets(label)
	}

	// Update 1: a different remoteRef key into the same target Secret. The
	// Secret keeps its name, so only the generation tells the controller to act.
	update("after the remoteRef change", "/other", target)
	// Update 2: a renamed target Secret.
	update("after the target rename", "/other", secretName+"-renamed")

	// Restart: SIGKILL and re-exec, give the controller 15 s to resync and
	// reconcile, then compare the converged state (G5).
	before := rv()
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	proxy.mark()
	t1 = time.Now()
	cmd = start()
	time.Sleep(15 * time.Second)
	waitReady("15s after the restart", 60*time.Second)
	after := rv()
	for _, k := range slices.Sorted(maps.Keys(after)) {
		fmt.Printf("restart: %s resourceVersion %s -> %s\n", k, before[k], after[k])
	}
	fmt.Printf("  writes over the restart:%s\n", tally(writes(proxy.since())))

	// Delete: this spike runs no garbage collector, so whatever is left is
	// either the controller's own cleanup or something botbox's collector
	// would resolve from its ownerReferences (DESIGN.md §5.8).
	proxy.mark()
	t1 = time.Now()
	must(dyn.Resource(esGVR).Namespace(namespace).Delete(ctx, esName, metav1.DeleteOptions{}))
	gone := time.Duration(-1)
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := dyn.Resource(esGVR).Namespace(namespace).Get(ctx, esName, metav1.GetOptions{}); err != nil {
			gone = time.Since(t1)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("externalsecret gone after %s (its finalizer had to clear first)\n", gone.Round(time.Millisecond))
	time.Sleep(10 * time.Second)
	remaining := listSecrets("10s after the delete")
	fmt.Printf("  writes over the delete:%s\n", tally(writes(proxy.since())))
	fmt.Printf("secrets remaining after the delete: %d\n", len(remaining))
	fmt.Println("OK")
}

// inventory lists every namespaced object in the run namespace, so `manages`
// is written from what is there rather than from what the docs say.
func inventory(ctx context.Context, cfg *rest.Config, dyn *dynamic.DynamicClient) string {
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	must(err)
	_, resources, err := disco.ServerGroupsAndResources()
	must(err)
	var lines []string
	for _, list := range resources {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		must(err)
		for _, r := range list.APIResources {
			if !r.Namespaced || strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") {
				continue
			}
			objects, err := dyn.Resource(gv.WithResource(r.Name)).Namespace(namespace).List(ctx, metav1.ListOptions{})
			if err != nil || len(objects.Items) == 0 {
				continue
			}
			for _, o := range objects.Items {
				lines = append(lines, fmt.Sprintf("    %s %s/%s", o.GetAPIVersion(), o.GetKind(), o.GetName()))
			}
		}
	}
	sort.Strings(lines)
	return "\n" + strings.Join(lines, "\n")
}

// listeningSockets reports the ports the controller bound, so the answer is
// the kernel's rather than the flags'.
func listeningSockets(pid int) string {
	out, err := exec.Command("lsof", "-nP", "-a", "-p", fmt.Sprint(pid), "-iTCP", "-sTCP:LISTEN").Output()
	if err != nil {
		return " none"
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n")[1:] {
		fields := strings.Fields(line)
		b.WriteString("\n    " + fields[len(fields)-2])
	}
	if b.Len() == 0 {
		return " none"
	}
	return b.String()
}
