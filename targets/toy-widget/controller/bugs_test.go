package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	toyv1 "github.com/rosenhouse/botbox/targets/toy-widget/api/v1"
)

// fixture wires a Reconciler running one seeded bug to a fake API server.
func fixture(t *testing.T, bug Bug, funcs interceptor.Funcs, objects ...client.Object) *Reconciler {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&toyv1.Widget{}).
		WithObjects(objects...).
		WithInterceptorFuncs(funcs).
		Build()
	return &Reconciler{Client: c, APIReader: c, Scheme: scheme, Bug: bug}
}

func reconcile(t *testing.T, r *Reconciler, widget *toyv1.Widget) error {
	t.Helper()
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(widget)})
	return err
}

func mustReconcile(t *testing.T, r *Reconciler, widget *toyv1.Widget) {
	t.Helper()
	if err := reconcile(t, r, widget); err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
}

// childNames returns the sorted names of the ConfigMaps in the Widget's namespace.
func childNames(t *testing.T, reader client.Reader, widget *toyv1.Widget) []string {
	t.Helper()
	configMaps := &corev1.ConfigMapList{}
	if err := reader.List(t.Context(), configMaps, client.InNamespace(widget.Namespace)); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(configMaps.Items))
	for _, configMap := range configMaps.Items {
		names = append(names, configMap.Name)
	}
	slices.Sort(names)
	return names
}

func child(t *testing.T, reader client.Reader, widget *toyv1.Widget, name string) *corev1.ConfigMap {
	t.Helper()
	configMap := &corev1.ConfigMap{}
	if err := reader.Get(t.Context(), client.ObjectKey{Namespace: widget.Namespace, Name: name}, configMap); err != nil {
		t.Fatal(err)
	}
	return configMap
}

func readWidget(t *testing.T, reader client.Reader, widget *toyv1.Widget) *toyv1.Widget {
	t.Helper()
	observed := &toyv1.Widget{}
	if err := reader.Get(t.Context(), client.ObjectKeyFromObject(widget), observed); err != nil {
		t.Fatal(err)
	}
	return observed
}

func setCount(t *testing.T, r *Reconciler, widget *toyv1.Widget, count int32) {
	t.Helper()
	observed := readWidget(t, r, widget)
	observed.Spec.Count = count
	if err := r.Update(t.Context(), observed); err != nil {
		t.Fatal(err)
	}
}

func TestB1ReportsTheChildrenReadyBeforeItCreatesThem(t *testing.T) {
	widget := newWidget(3)
	readyAtFirstChild := int32(-1)
	watchCreates := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isChild := obj.(*corev1.ConfigMap); isChild && readyAtFirstChild < 0 {
				readyAtFirstChild = readWidget(t, c, widget).Status.Ready
			}
			return c.Create(ctx, obj, opts...)
		},
	}
	r := fixture(t, B1, watchCreates, widget)
	r.B1Hold = 50 * time.Millisecond

	start := time.Now()
	mustReconcile(t, r, widget)

	if readyAtFirstChild != widget.Spec.Count {
		t.Errorf("status.ready was %d when the first child was created, want the whole count %d.",
			readyAtFirstChild, widget.Spec.Count)
	}
	if held := time.Since(start); held < r.B1Hold {
		t.Errorf("Reconcile took %v, want it to hold the premature status for %v.", held, r.B1Hold)
	}
}

func TestB2CreatesAnotherSetOfChildrenOnEveryReconcile(t *testing.T) {
	widget := newWidget(2)
	r := fixture(t, B2, interceptor.Funcs{}, widget)

	mustReconcile(t, r, widget)
	mustReconcile(t, r, widget)

	if names := childNames(t, r, widget); len(names) != 4 {
		t.Errorf("Two reconciles left the ConfigMaps %v, want 4 of them for a count of 2.", names)
	}
}

func TestB3OrphansTheFirstChildAndStillReportsItReady(t *testing.T) {
	widget := newWidget(2)
	r := fixture(t, B3, interceptor.Funcs{}, widget)

	mustReconcile(t, r, widget)

	if owner := metav1.GetControllerOf(child(t, r, widget, "w-0")); owner != nil {
		t.Errorf("w-0 is controlled by %v, want no ownerReference at all.", owner)
	}
	if owner := metav1.GetControllerOf(child(t, r, widget, "w-1")); owner == nil {
		t.Error("w-1 carries no ownerReference, want the Widget as its controller.")
	}
	if ready := readWidget(t, r, widget).Status.Ready; ready != widget.Spec.Count {
		t.Errorf("status.ready is %d, want the count %d: the orphan counts by name.", ready, widget.Spec.Count)
	}
}

func TestB3LeavesTheOrphanBehindWhenTheWidgetGoes(t *testing.T) {
	widget := newWidget(2)
	r := fixture(t, B3, interceptor.Funcs{}, widget)
	mustReconcile(t, r, widget)

	if err := r.Delete(t.Context(), readWidget(t, r, widget)); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, r, widget) // Deletes the children it controls.
	mustReconcile(t, r, widget) // Releases the Widget once none remain.

	if err := r.Get(t.Context(), client.ObjectKeyFromObject(widget), &toyv1.Widget{}); !apierrors.IsNotFound(err) {
		t.Errorf("Getting the Widget returned %v, want it gone.", err)
	}
	if names, want := childNames(t, r, widget), []string{"w-0"}; !slices.Equal(names, want) {
		t.Errorf("Deleting the Widget left the ConfigMaps %v, want the orphan %v.", names, want)
	}
}

func TestB4TakesTheCountFromTheStatus(t *testing.T) {
	t.Run("a fresh Widget never gets a child", func(t *testing.T) {
		widget := newWidget(3)
		r := fixture(t, B4, interceptor.Funcs{}, widget)

		mustReconcile(t, r, widget)

		if names := childNames(t, r, widget); len(names) != 0 {
			t.Errorf("Reconcile left the ConfigMaps %v, want none while status.ready is 0.", names)
		}
	})

	t.Run("a status.ready of 1 gets one child", func(t *testing.T) {
		widget := newWidget(3)
		widget.Status.Ready = 1
		r := fixture(t, B4, interceptor.Funcs{}, widget)

		mustReconcile(t, r, widget)

		if names, want := childNames(t, r, widget), []string{"w-0"}; !slices.Equal(names, want) {
			t.Errorf("Reconcile left the ConfigMaps %v, want %v from status.ready.", names, want)
		}
	})
}

func TestB5TreatsAMissingChildAsAnErrorForever(t *testing.T) {
	widget := newWidget(1)
	r := fixture(t, B5, interceptor.Funcs{}, widget)

	for attempt := 1; attempt <= 2; attempt++ {
		if err := reconcile(t, r, widget); !apierrors.IsNotFound(err) {
			t.Errorf("Reconcile %d returned %v, want the NotFound of the missing child.", attempt, err)
		}
	}

	if names := childNames(t, r, widget); len(names) != 0 {
		t.Errorf("Reconcile left the ConfigMaps %v, want none: the Get never gives way to a create.", names)
	}
}

func TestB6WritesTheStatusOnEveryReconcile(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		bug        Bug
		wantWrites int
		wantStamp  bool
	}{
		{"the correct controller writes the status once", 0, 1, false},
		{"B6 writes it again for a fresh lastSyncTime", B6, 2, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			writes := 0
			countStatusWrites := interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object,
					patch client.Patch, opts ...client.SubResourcePatchOption) error {
					writes++
					return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
				},
			}
			widget := newWidget(1)
			r := fixture(t, testCase.bug, countStatusWrites, widget)

			mustReconcile(t, r, widget)
			mustReconcile(t, r, widget)

			if writes != testCase.wantWrites {
				t.Errorf("Two reconciles wrote the status %d times, want %d.", writes, testCase.wantWrites)
			}
			if stamped := readWidget(t, r, widget).Status.LastSyncTime != nil; stamped != testCase.wantStamp {
				t.Errorf("status.lastSyncTime is set: %t, want %t.", stamped, testCase.wantStamp)
			}
		})
	}
}

func TestB7KeepsTheSurplusChildrenOnScaleDown(t *testing.T) {
	widget := newWidget(2)
	r := fixture(t, B7, interceptor.Funcs{}, widget)
	mustReconcile(t, r, widget)

	setCount(t, r, widget, 1)
	mustReconcile(t, r, widget)

	if names, want := childNames(t, r, widget), []string{"w-0", "w-1"}; !slices.Equal(names, want) {
		t.Errorf("Scaling down to 1 left the ConfigMaps %v, want %v.", names, want)
	}
	if ready := readWidget(t, r, widget).Status.Ready; ready != 2 {
		t.Errorf("status.ready is %d, want the 2 ConfigMaps that are still there.", ready)
	}
}

func TestB9ReleasesTheWidgetBeforeDeletingItsChildren(t *testing.T) {
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	widget := deletingWidget(1)
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: widget.Namespace, Name: "w-0"}}
	if err := controllerutil.SetControllerReference(widget, configMap, scheme); err != nil {
		t.Fatal(err)
	}
	r := fixture(t, B9, interceptor.Funcs{}, widget, configMap)

	mustReconcile(t, r, widget)

	if err := r.Get(t.Context(), client.ObjectKeyFromObject(widget), &toyv1.Widget{}); !apierrors.IsNotFound(err) {
		t.Errorf("Getting the Widget returned %v, want it released on the first deletion reconcile.", err)
	}
	if names, want := childNames(t, r, widget), []string{"w-0"}; !slices.Equal(names, want) {
		t.Errorf("The deletion reconcile left the ConfigMaps %v, want the abandoned %v.", names, want)
	}
}

func TestB9OrphansEveryChild(t *testing.T) {
	widget := newWidget(2)
	r := fixture(t, B9, interceptor.Funcs{}, widget)

	mustReconcile(t, r, widget)

	for _, name := range []string{"w-0", "w-1"} {
		if owner := metav1.GetControllerOf(child(t, r, widget, name)); owner != nil {
			t.Errorf("%s is controlled by %v, want no ownerReference at all.", name, owner)
		}
	}
}

func TestB10LosesTheStatusOfAWidgetItDidNotCreateChildrenFor(t *testing.T) {
	widget := newWidget(3)
	r := fixture(t, B10, interceptor.Funcs{}, widget)
	mustReconcile(t, r, widget)
	if ready := readWidget(t, r, widget).Status.Ready; ready != 3 {
		t.Fatalf("status.ready is %d after the reconcile that created the children, want 3.", ready)
	}

	restarted := &Reconciler{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, Bug: B10}
	setCount(t, r, widget, 1)
	mustReconcile(t, restarted, widget)

	if names, want := childNames(t, r, widget), []string{"w-0"}; !slices.Equal(names, want) {
		t.Errorf("The restarted reconciler left the ConfigMaps %v, want the converged %v.", names, want)
	}
	if ready := readWidget(t, r, widget).Status.Ready; ready != 3 {
		t.Errorf("status.ready is %d, want it left stale at 3.", ready)
	}
}
