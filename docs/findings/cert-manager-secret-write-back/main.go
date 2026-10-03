// Command cert-manager-secret-write-back counts how often cert-manager gives a
// recreated Certificate the deleted Certificate's private key. It needs a
// cluster where cert-manager runs with --enable-certificate-owner-ref=true.
//
// Each run creates a Certificate with an RSA key under the rotationPolicy
// -policy names, and waits for it to be Ready. It then deletes the Certificate,
// waits until it is gone, and at once creates one of the same name whose key
// algorithm -algorithm names.
package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	certificates = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	requests     = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificaterequests"}
	secrets      = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	issuers      = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "issuers"}
)

func main() {
	kubeconfig := flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "the cluster's kubeconfig")
	runs := flag.Int("runs", 10, "how many runs to make")
	collect := flag.Bool("collect", false, "once the old Certificate is deleted, delete what it owns as a garbage collector would, on a cluster that runs none")
	timeout := flag.Duration("wait", 30*time.Second, "how long the new Certificate has to become Ready")
	algorithm := flag.String("algorithm", "ECDSA", "the new Certificate's key algorithm")
	policy := flag.String("policy", "Never", "both Certificates' rotationPolicy")
	flag.Parse()
	r, err := newRepro(*kubeconfig)
	check(err)
	r.collect, r.timeout, r.algorithm, r.policy = *collect, *timeout, *algorithm, *policy
	counts := map[string]int{}
	for i := 1; i <= *runs; i++ {
		found, err := r.run(context.Background())
		check(err)
		fmt.Printf("run %d: %s\n", i, found)
		counts[found.mechanism]++
	}
	fmt.Println(counts)
}

type repro struct {
	core      kubernetes.Interface
	dyn       dynamic.Interface
	collect   bool
	timeout   time.Duration
	algorithm string
	policy    string
	ns        string
}

// newRepro's clients are unthrottled. client-go's default of 5 requests a second
// would put about 200 ms between the delete and the create, and that closes
// the window.
func newRepro(kubeconfig string) (repro, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return repro{}, err
	}
	config.QPS = -1
	core, err := kubernetes.NewForConfig(config)
	if err != nil {
		return repro{}, err
	}
	dyn, err := dynamic.NewForConfig(config)
	return repro{core: core, dyn: dyn}, err
}

type finding struct {
	mechanism, oldCertificate, newCertificate, ready string
	old, now                                         secret
}

func (f finding) String() string {
	return fmt.Sprintf("%s. Certificate %.8s, then %.8s, Ready %s. Secret %.8s, then %.8s owned by %.8s. tls.key sha256 %.12s, then %.12s.",
		f.mechanism, f.oldCertificate, f.newCertificate, f.ready, f.old.UID, f.now.UID, f.now.Owner, f.old.Key, f.now.Key)
}

// secret is what a run records of the Certificate's Secret. Its UID is empty
// where there is none.
type secret struct{ UID, Owner, Key string }

func mechanism(old, now secret, newCertificate string) string {
	switch {
	case now.UID == "":
		return "no Secret"
	case now.Key != old.Key:
		return "new key"
	case now.UID != old.UID:
		return "write-back"
	case now.Owner == newCertificate:
		return "re-point"
	}
	return "not collected"
}

func (r repro) run(ctx context.Context) (finding, error) {
	ns, err := r.core.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "write-back-"}}, metav1.CreateOptions{})
	if err != nil {
		return finding{}, err
	}
	defer r.core.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{})
	r.ns = ns.Name
	if _, err := r.dyn.Resource(issuers).Namespace(r.ns).Create(ctx, object("Issuer", "selfsigned", map[string]any{"selfSigned": map[string]any{}}), metav1.CreateOptions{}); err != nil {
		return finding{}, err
	}
	first, err := r.dyn.Resource(certificates).Namespace(r.ns).Create(ctx, certificate(r.policy, ""), metav1.CreateOptions{})
	if err != nil {
		return finding{}, err
	}
	if ready := r.ready(ctx, time.Minute); ready != "True Ready" {
		return finding{}, fmt.Errorf("the first Certificate is not Ready: %s", ready)
	}
	old, err := r.secret(ctx)
	if err != nil {
		return finding{}, err
	}
	if err := r.dyn.Resource(certificates).Namespace(r.ns).Delete(ctx, "example", metav1.DeleteOptions{}); err != nil {
		return finding{}, err
	}
	collected := make(chan error, 1)
	go func() { collected <- r.collectGarbage(ctx, first.GetUID()) }()
	if err := wait.PollUntilContextTimeout(ctx, 10*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := r.dyn.Resource(certificates).Namespace(r.ns).Get(ctx, "example", metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	}); err != nil {
		return finding{}, err
	}
	second, err := r.dyn.Resource(certificates).Namespace(r.ns).Create(ctx, certificate(r.policy, r.algorithm), metav1.CreateOptions{})
	if err != nil {
		return finding{}, err
	}
	if err := <-collected; err != nil {
		return finding{}, err
	}
	ready := r.ready(ctx, r.timeout)
	now, err := r.secretOnceThere(ctx)
	if err != nil {
		return finding{}, err
	}
	return finding{mechanism(old, now, string(second.GetUID())), string(first.GetUID()), string(second.GetUID()), ready, old, now}, nil
}

// collectGarbage, under -collect, deletes what the old Certificate controls as
// a garbage collector would: it deletes an object only while the object still
// names that owner and has not changed since it was read.
func (r repro) collectGarbage(ctx context.Context, owner types.UID) error {
	if !r.collect {
		return nil
	}
	for _, resource := range []schema.GroupVersionResource{secrets, requests} {
		list, err := r.dyn.Resource(resource).Namespace(r.ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		for _, o := range list.Items {
			if ref := metav1.GetControllerOfNoCopy(&o); ref == nil || ref.UID != owner {
				continue
			}
			uid, version := o.GetUID(), o.GetResourceVersion()
			err := r.dyn.Resource(resource).Namespace(r.ns).Delete(ctx, o.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}})
			if err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

// ready waits for the Certificate to be Ready, and returns the status and
// reason of the Ready condition it saw last.
func (r repro) ready(ctx context.Context, timeout time.Duration) string {
	seen := "unset"
	_ = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, timeout, true, func(context.Context) (bool, error) {
		crt, err := r.dyn.Resource(certificates).Namespace(r.ns).Get(ctx, "example", metav1.GetOptions{})
		if err != nil {
			seen = err.Error()
			return false, nil
		}
		conditions, _, _ := unstructured.NestedSlice(crt.Object, "status", "conditions")
		for _, c := range conditions {
			if c, _ := c.(map[string]any); c["type"] == "Ready" {
				seen = fmt.Sprintf("%v %v", c["status"], c["reason"])
			}
		}
		return seen == "True Ready", nil
	})
	return seen
}

// secretOnceThere waits for the Secret to exist, because a Certificate can turn
// Ready while cert-manager still applies its Secret.
func (r repro) secretOnceThere(ctx context.Context) (secret, error) {
	var s secret
	err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, r.timeout, true, func(context.Context) (bool, error) {
		var err error
		s, err = r.secret(ctx)
		return s.UID != "", err
	})
	if wait.Interrupted(err) {
		err = nil
	}
	return s, err
}

func (r repro) secret(ctx context.Context) (secret, error) {
	s, err := r.core.CoreV1().Secrets(r.ns).Get(ctx, "example-tls", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return secret{}, nil
	}
	if err != nil {
		return secret{}, err
	}
	var owner string
	if ref := metav1.GetControllerOf(s); ref != nil {
		owner = string(ref.UID)
	}
	return secret{string(s.UID), owner, fmt.Sprintf("%x", sha256.Sum256(s.Data[corev1.TLSPrivateKeyKey]))}, nil
}

func certificate(policy, algorithm string) *unstructured.Unstructured {
	key := map[string]any{"rotationPolicy": policy}
	if algorithm != "" {
		key["algorithm"] = algorithm
	}
	return object("Certificate", "example", map[string]any{
		"secretName": "example-tls",
		"commonName": "example.test",
		"dnsNames":   []any{"example.test"},
		"issuerRef":  map[string]any{"kind": "Issuer", "name": "selfsigned"},
		"privateKey": key,
	})
}

func object(kind, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}}
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
