//go:build envtest

package cluster_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse/botbox/pkg/cluster"
)

var configMapKind = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

// collectionBudget is the bound DESIGN.md §5.8 puts on the collector: it
// deletes within a second of the owner's deletion event.
const collectionBudget = time.Second

func TestCollector(t *testing.T) {
	c, err := cluster.Start(cluster.Options{})
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Stop(); err != nil {
			t.Errorf("Stop returned an error: %v", err)
		}
	})
	client, err := kubernetes.NewForConfig(c.Config())
	if err != nil {
		t.Fatalf("Building a client failed: %v", err)
	}

	t.Run("collects a child once its owner is gone", func(t *testing.T) {
		namespace := newNamespace(t, client)
		parent := createConfigMap(t, client, namespace, "parent")
		child := createConfigMap(t, client, namespace, "child", ownerOf(parent))
		startCollector(t, c.Config(), namespace, configMapKind)

		deleteConfigMap(t, client, namespace, parent.Name)

		waitGone(t, client, namespace, child.Name)
	})

	t.Run("keeps what it must not collect", func(t *testing.T) {
		namespace := newNamespace(t, client)
		parent := createConfigMap(t, client, namespace, "parent")
		canary := createConfigMap(t, client, namespace, "canary", ownerOf(parent))
		ownerless := createConfigMap(t, client, namespace, "ownerless")
		unwatchedOwner := createConfigMap(t, client, namespace, "unwatched-owner", secretOwner("absent"))
		startCollector(t, c.Config(), namespace, configMapKind)

		deleteConfigMap(t, client, namespace, parent.Name)

		waitGone(t, client, namespace, canary.Name)
		requirePresent(t, client, namespace, ownerless.Name)
		requirePresent(t, client, namespace, unwatchedOwner.Name)
	})

	t.Run("collects a child once its last owner is gone", func(t *testing.T) {
		namespace := newNamespace(t, client)
		first := createConfigMap(t, client, namespace, "first-owner")
		second := createConfigMap(t, client, namespace, "second-owner")
		child := createConfigMap(t, client, namespace, "child", ownerOf(first), ownerOf(second))
		canary := createConfigMap(t, client, namespace, "canary", ownerOf(first))
		startCollector(t, c.Config(), namespace, configMapKind)

		deleteConfigMap(t, client, namespace, first.Name)

		waitGone(t, client, namespace, canary.Name)
		requirePresent(t, client, namespace, child.Name)

		deleteConfigMap(t, client, namespace, second.Name)

		waitGone(t, client, namespace, child.Name)
	})

	// The owner is recreated before the collector starts, so the child can
	// only be collected by the UID comparison, never by the owner's absence.
	t.Run("collects a child whose owner's name was reused", func(t *testing.T) {
		namespace := newNamespace(t, client)
		parent := createConfigMap(t, client, namespace, "parent")
		deleteConfigMap(t, client, namespace, parent.Name)
		recreated := createConfigMap(t, client, namespace, "parent")
		child := createConfigMap(t, client, namespace, "child", ownerOf(parent))

		startCollector(t, c.Config(), namespace, configMapKind)

		waitGone(t, client, namespace, child.Name)
		requirePresent(t, client, namespace, recreated.Name)
	})

	// A kind given without a version resolves to the version the API server
	// serves, which is the version an ownerReference names.
	t.Run("collects a child when the kind carries no version", func(t *testing.T) {
		namespace := newNamespace(t, client)
		parent := createConfigMap(t, client, namespace, "parent")
		child := createConfigMap(t, client, namespace, "child", ownerOf(parent))
		startCollector(t, c.Config(), namespace, schema.GroupVersionKind{Kind: "ConfigMap"})

		deleteConfigMap(t, client, namespace, parent.Name)

		waitGone(t, client, namespace, child.Name)
	})

	t.Run("refuses a cluster-scoped kind", func(t *testing.T) {
		clusterScoped := schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}

		collector, err := cluster.StartCollector(c.Config(), cluster.CollectorOptions{
			Namespace: newNamespace(t, client),
			Kinds:     []schema.GroupVersionKind{clusterScoped},
		})

		if err == nil {
			collector.Stop()
			t.Fatal("StartCollector accepted a cluster-scoped kind.")
		}
		if !strings.Contains(err.Error(), "cluster-scoped") || !strings.Contains(err.Error(), clusterScoped.Kind) {
			t.Errorf("StartCollector returned %q, which does not say that %s is cluster-scoped.", err, clusterScoped)
		}
	})
}

func newNamespace(t *testing.T, client kubernetes.Interface) string {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "collector-"}}
	created, err := client.CoreV1().Namespaces().Create(context.Background(), namespace, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Creating a namespace failed: %v", err)
	}
	return created.Name
}

func startCollector(t *testing.T, config *rest.Config, namespace string, kind schema.GroupVersionKind) {
	t.Helper()
	collector, err := cluster.StartCollector(config, cluster.CollectorOptions{
		Namespace: namespace,
		Kinds:     []schema.GroupVersionKind{kind},
	})
	if err != nil {
		t.Fatalf("StartCollector returned an error: %v", err)
	}
	t.Cleanup(collector.Stop)
}

func createConfigMap(t *testing.T, client kubernetes.Interface, namespace, name string, owners ...metav1.OwnerReference) *corev1.ConfigMap {
	t.Helper()
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: owners}}
	created, err := client.CoreV1().ConfigMaps(namespace).Create(context.Background(), configMap, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Creating the ConfigMap %s failed: %v", name, err)
	}
	return created
}

func deleteConfigMap(t *testing.T, client kubernetes.Interface, namespace, name string) {
	t.Helper()
	if err := client.CoreV1().ConfigMaps(namespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Deleting the ConfigMap %s failed: %v", name, err)
	}
}

func ownerOf(configMap *corev1.ConfigMap) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: configMap.Name, UID: configMap.UID}
}

// secretOwner names an owner of a kind the collector does not watch.
func secretOwner(name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "Secret",
		Name:       name,
		UID:        "8a1d0f2c-5b3e-4d7a-9c6f-1e2b3c4d5e6f",
	}
}

func waitGone(t *testing.T, client kubernetes.Interface, namespace, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), collectionBudget)
	defer cancel()
	gone := func(ctx context.Context) (bool, error) {
		_, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	}
	if err := wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, gone); err != nil {
		t.Fatalf("The collector left the ConfigMap %s in place: %v", name, err)
	}
}

func requirePresent(t *testing.T, client kubernetes.Interface, namespace, name string) {
	t.Helper()
	if _, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Fatalf("Reading the ConfigMap %s failed: %v", name, err)
	}
}
