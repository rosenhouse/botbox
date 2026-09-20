//go:build envtest

package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/rosenhouse/botbox/pkg/cluster"
	toyv1 "github.com/rosenhouse/botbox/targets/toy-widget/api/v1"
	"github.com/rosenhouse/botbox/targets/toy-widget/controller"
)

const (
	pollInterval = 25 * time.Millisecond
	pollDeadline = 20 * time.Second
)

// TestWidgetLifecycle drives one Widget through the operations the harness
// performs, sharing a control plane because each start costs seconds.
func TestWidgetLifecycle(t *testing.T) {
	ctx := t.Context()
	c := startController(t, ctx)
	widget := createWidget(t, ctx, c, 3)

	t.Run("creates one owned ConfigMap per count", func(t *testing.T) {
		requireChildren(t, ctx, c, widget, 3)
		requireStatus(t, ctx, c, widget, 3)
	})

	t.Run("creates the missing children on scale up", func(t *testing.T) {
		setCount(t, ctx, c, widget, 5)

		requireChildren(t, ctx, c, widget, 5)
		requireStatus(t, ctx, c, widget, 5)
	})

	t.Run("deletes the surplus children on scale down", func(t *testing.T) {
		setCount(t, ctx, c, widget, 1)

		requireChildren(t, ctx, c, widget, 1)
		requireStatus(t, ctx, c, widget, 1)
	})

	t.Run("recreates a child deleted behind its back", func(t *testing.T) {
		deleted := &corev1.ConfigMap{}
		key := client.ObjectKey{Namespace: widget.Namespace, Name: widget.Name + "-0"}
		if err := c.Get(ctx, key, deleted); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(ctx, deleted); err != nil {
			t.Fatal(err)
		}

		eventually(t, func() error {
			recreated := &corev1.ConfigMap{}
			if err := c.Get(ctx, key, recreated); err != nil {
				return err
			}
			if recreated.UID == deleted.UID {
				return fmt.Errorf("ConfigMap %s is still the deleted one", key.Name)
			}
			return nil
		})
		requireChildren(t, ctx, c, widget, 1)
	})

	t.Run("holds no children at count zero", func(t *testing.T) {
		setCount(t, ctx, c, widget, 0)

		requireChildren(t, ctx, c, widget, 0)
		requireStatus(t, ctx, c, widget, 0)
		requireStatusFieldsExist(t, ctx, c, widget)
	})

	t.Run("deletes its children and itself on delete", func(t *testing.T) {
		if err := c.Delete(ctx, widget); err != nil {
			t.Fatal(err)
		}

		eventually(t, func() error {
			err := c.Get(ctx, client.ObjectKeyFromObject(widget), &toyv1.Widget{})
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("the Widget is still there: %v", err)
			}
			return childrenMatch(ctx, c, widget, 0)
		})
	})
}

// startController brings up a control plane holding the Widget CRD, runs the
// reconciler against it, and returns a client that bypasses the controller's
// cache.
func startController(t *testing.T, ctx context.Context) client.Client {
	t.Helper()
	ctrl.SetLogger(zap.New(zap.WriteTo(io.Discard)))

	testCluster, err := cluster.Start(cluster.Options{CRDPaths: []string{"crds"}})
	if err != nil {
		t.Fatalf("Starting the test cluster failed: %v", err)
	}
	t.Cleanup(func() {
		if err := testCluster.Stop(); err != nil {
			t.Errorf("Stopping the test cluster failed: %v", err)
		}
	})

	scheme, err := controller.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := ctrl.NewManager(testCluster.Config(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("Creating the manager failed: %v", err)
	}
	reconciler := &controller.Reconciler{Client: manager.GetClient(), Scheme: manager.GetScheme()}
	if err := reconciler.SetupWithManager(manager); err != nil {
		t.Fatalf("Setting up the controller failed: %v", err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- manager.Start(ctx) }()
	t.Cleanup(func() {
		if err := <-stopped; err != nil {
			t.Errorf("The manager returned an error: %v", err)
		}
	})

	uncached, err := client.New(testCluster.Config(), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Building a client failed: %v", err)
	}
	return uncached
}

func createWidget(t *testing.T, ctx context.Context, c client.Client, count int) *toyv1.Widget {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "widget-"}}
	if err := c.Create(ctx, namespace); err != nil {
		t.Fatalf("Creating a namespace failed: %v", err)
	}
	widget := &toyv1.Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace.Name, Name: "w"},
		Spec:       toyv1.WidgetSpec{Count: count},
	}
	if err := c.Create(ctx, widget); err != nil {
		t.Fatalf("Creating a Widget failed: %v", err)
	}
	return widget
}

func setCount(t *testing.T, ctx context.Context, c client.Client, widget *toyv1.Widget, count int) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(ctx, client.ObjectKeyFromObject(widget), widget); err != nil {
			return err
		}
		widget.Spec.Count = count
		return c.Update(ctx, widget)
	})
	if err != nil {
		t.Fatalf("Setting spec.count to %d failed: %v", count, err)
	}
}

func requireChildren(t *testing.T, ctx context.Context, c client.Client, widget *toyv1.Widget, count int) {
	t.Helper()
	eventually(t, func() error { return childrenMatch(ctx, c, widget, count) })
}

// childrenMatch reports whether the Widget's namespace holds exactly the
// ConfigMaps its count requires.
func childrenMatch(ctx context.Context, c client.Client, widget *toyv1.Widget, count int) error {
	configMaps := &corev1.ConfigMapList{}
	if err := c.List(ctx, configMaps, client.InNamespace(widget.Namespace)); err != nil {
		return err
	}
	byName := map[string]corev1.ConfigMap{}
	for _, configMap := range configMaps.Items {
		byName[configMap.Name] = configMap
	}

	want := make([]string, count)
	for index := range count {
		want[index] = fmt.Sprintf("%s-%d", widget.Name, index)
	}
	if got := slices.Sorted(maps.Keys(byName)); !slices.Equal(got, want) {
		return fmt.Errorf("the namespace holds the ConfigMaps %v, want %v", got, want)
	}
	for index, name := range want {
		if err := childMatches(byName[name], widget, index); err != nil {
			return err
		}
	}
	return nil
}

func childMatches(child corev1.ConfigMap, widget *toyv1.Widget, index int) error {
	if want := map[string]string{"index": strconv.Itoa(index)}; !maps.Equal(child.Data, want) {
		return fmt.Errorf("ConfigMap %s holds the data %v, want %v", child.Name, child.Data, want)
	}
	want := metav1.OwnerReference{
		APIVersion: toyv1.GroupVersion.String(),
		Kind:       "Widget",
		Name:       widget.Name,
		UID:        widget.UID,
	}
	owner := metav1.GetControllerOf(&child)
	if owner == nil || owner.APIVersion != want.APIVersion || owner.Kind != want.Kind ||
		owner.Name != want.Name || owner.UID != want.UID {
		return fmt.Errorf("ConfigMap %s carries the ownerReferences %v, want %v as its controller",
			child.Name, child.OwnerReferences, want)
	}
	return nil
}

func requireStatus(t *testing.T, ctx context.Context, c client.Client, widget *toyv1.Widget, ready int) {
	t.Helper()
	eventually(t, func() error {
		observed := &toyv1.Widget{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(widget), observed); err != nil {
			return err
		}
		if observed.Status.Ready != ready {
			return fmt.Errorf("status.ready is %d, want %d", observed.Status.Ready, ready)
		}
		if observed.Status.ObservedGeneration != observed.Generation {
			return fmt.Errorf("status.observedGeneration is %d, want the generation %d",
				observed.Status.ObservedGeneration, observed.Generation)
		}
		if !slices.Contains(observed.Finalizers, controller.Finalizer) {
			return fmt.Errorf("the Widget carries the finalizers %v, want %s",
				observed.Finalizers, controller.Finalizer)
		}
		return nil
	})
}

// requireStatusFieldsExist guards the ready predicate of DESIGN.md §9, which
// reaches for status.ready and status.observedGeneration behind has(). A zero
// that the API drops is a Widget that never becomes ready.
func requireStatusFieldsExist(t *testing.T, ctx context.Context, c client.Client, widget *toyv1.Widget) {
	t.Helper()
	observed := &unstructured.Unstructured{}
	observed.SetGroupVersionKind(toyv1.GroupVersion.WithKind("Widget"))
	if err := c.Get(ctx, client.ObjectKeyFromObject(widget), observed); err != nil {
		t.Fatal(err)
	}
	status, _, err := unstructured.NestedMap(observed.Object, "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"ready", "observedGeneration"} {
		if _, found := status[field]; !found {
			t.Errorf("The stored status is %v, want it to carry %s.", status, field)
		}
	}
}

// eventually polls check until it passes or the deadline expires.
func eventually(t *testing.T, check func() error) {
	t.Helper()
	deadline := time.Now().Add(pollDeadline)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("The condition never held: %v", err)
		}
		time.Sleep(pollInterval)
	}
}
