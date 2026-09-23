package controller

import (
	"reflect"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	toyv1 "github.com/rosenhouse/botbox/targets/toy-widget/api/v1"
)

func newWidget(count int32) *toyv1.Widget {
	return &toyv1.Widget{
		ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "ns", Generation: 1, UID: "widget-uid"},
		Spec:       toyv1.WidgetSpec{Count: count},
	}
}

func deletingWidget(count int32) *toyv1.Widget {
	widget := newWidget(count)
	widget.Finalizers = []string{Finalizer}
	deletedAt := metav1.Now()
	widget.DeletionTimestamp = &deletedAt
	return widget
}

func TestDesiredChildrenNamesAndIndexesOnePerCount(t *testing.T) {
	children := desiredChildren(newWidget(3), 3)

	want := []corev1.ConfigMap{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "w-0"}, Data: map[string]string{"index": "0"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "w-1"}, Data: map[string]string{"index": "1"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "w-2"}, Data: map[string]string{"index": "2"}},
	}
	if !reflect.DeepEqual(children, want) {
		t.Errorf("desiredChildren returned %v, want %v", children, want)
	}
}

func TestDesiredChildrenFollowsTheCountItIsGiven(t *testing.T) {
	if children := desiredChildren(newWidget(3), 0); len(children) != 0 {
		t.Errorf("desiredChildren returned %v for count 0.", children)
	}
	if children := desiredChildren(newWidget(0), 2); len(children) != 2 {
		t.Errorf("desiredChildren returned %v for count 2.", children)
	}
}

func TestStatusForCarriesTheReadyCountAndTheGeneration(t *testing.T) {
	next, changed := statusFor(newWidget(3), 2)

	if want := (toyv1.WidgetStatus{Ready: 2, ObservedGeneration: 1}); next != want {
		t.Errorf("statusFor returned %+v, want %+v", next, want)
	}
	if !changed {
		t.Error("statusFor reported no change although the status was empty.")
	}
}

func TestStatusForReportsAChangeOnlyWhenOneIsNeeded(t *testing.T) {
	widget := newWidget(3)
	widget.Status = toyv1.WidgetStatus{Ready: 3, ObservedGeneration: 1}

	if _, changed := statusFor(widget, 3); changed {
		t.Error("statusFor asked for a write although the status already matched.")
	}
	if _, changed := statusFor(widget, 2); !changed {
		t.Error("statusFor kept a stale child count.")
	}

	widget.Generation = 2
	if _, changed := statusFor(widget, 3); !changed {
		t.Error("statusFor kept a stale observedGeneration.")
	}
}

// TestCleanUpFindsChildrenTheCacheHasMissed gives the Reconciler a cache that
// has not seen the child yet, so only a read through APIReader holds the Widget
// back.
func TestCleanUpFindsChildrenTheCacheHasMissed(t *testing.T) {
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	widget := deletingWidget(1)
	child := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: widget.Namespace, Name: "w-0"}}
	if err := controllerutil.SetControllerReference(widget, child, scheme); err != nil {
		t.Fatal(err)
	}
	cache := fake.NewClientBuilder().WithScheme(scheme).WithObjects(widget.DeepCopy()).Build()
	apiServer := fake.NewClientBuilder().WithScheme(scheme).WithObjects(widget.DeepCopy(), child).Build()
	reconciler := &Reconciler{Client: cache, APIReader: apiServer, Scheme: scheme}
	deleting := &toyv1.Widget{}
	if err := cache.Get(t.Context(), client.ObjectKeyFromObject(widget), deleting); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.cleanUp(t.Context(), deleting); err != nil {
		t.Fatalf("cleanUp returned an error: %v", err)
	}

	if !slices.Contains(deleting.Finalizers, Finalizer) {
		t.Errorf("cleanUp left the finalizers %v, want it to hold %s until the child is gone.",
			deleting.Finalizers, Finalizer)
	}
}

func TestCleanUpWaitsOutTheCleanupDelay(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		deletedAgo  time.Duration
		wantRequeue time.Duration
		wantConfigs []string
	}{
		{"within the delay", time.Minute, 59 * time.Minute, []string{"w-0"}},
		{"past the delay", 2 * time.Hour, 0, []string{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheme, err := NewScheme()
			if err != nil {
				t.Fatal(err)
			}
			widget := deletingWidget(1)
			widget.DeletionTimestamp = &metav1.Time{Time: time.Now().Add(-testCase.deletedAgo)}
			configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: widget.Namespace, Name: "w-0"}}
			if err := controllerutil.SetControllerReference(widget, configMap, scheme); err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(widget, configMap).Build()
			r := &Reconciler{Client: c, APIReader: c, Scheme: scheme, CleanupDelay: time.Hour}

			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(widget)})

			if err != nil {
				t.Fatalf("Reconcile returned an error: %v", err)
			}
			if requeue := result.RequeueAfter; requeue > testCase.wantRequeue || requeue < testCase.wantRequeue-time.Minute {
				t.Errorf("Reconcile asked to requeue after %v, want about %v.", requeue, testCase.wantRequeue)
			}
			configMaps := &corev1.ConfigMapList{}
			if err := c.List(t.Context(), configMaps); err != nil {
				t.Fatal(err)
			}
			names := []string{}
			for _, item := range configMaps.Items {
				names = append(names, item.Name)
			}
			if !slices.Equal(names, testCase.wantConfigs) {
				t.Errorf("Reconcile left the ConfigMaps %v, want %v.", names, testCase.wantConfigs)
			}
		})
	}
}

// TestCleanUpIgnoresAWidgetThatIsAlreadyGone covers the reconcile that follows
// the last child's deletion, holding a Widget the API server has removed.
func TestCleanUpIgnoresAWidgetThatIsAlreadyGone(t *testing.T) {
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	widget := deletingWidget(0)
	collected := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &Reconciler{Client: collected, APIReader: collected, Scheme: scheme}

	if err := reconciler.cleanUp(t.Context(), widget); err != nil {
		t.Errorf("cleanUp returned an error for a Widget the collector had taken: %v", err)
	}
}
