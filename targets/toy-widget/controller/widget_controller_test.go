package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	toyv1 "github.com/rosenhouse/botbox/targets/toy-widget/api/v1"
)

func newWidget(count int) *toyv1.Widget {
	return &toyv1.Widget{
		ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "ns", Generation: 1},
		Spec:       toyv1.WidgetSpec{Count: count},
	}
}

func TestDesiredChildrenNamesAndIndexesOnePerCount(t *testing.T) {
	children := desiredChildren(newWidget(3))

	want := []corev1.ConfigMap{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "w-0"}, Data: map[string]string{"index": "0"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "w-1"}, Data: map[string]string{"index": "1"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "w-2"}, Data: map[string]string{"index": "2"}},
	}
	if !reflect.DeepEqual(children, want) {
		t.Errorf("desiredChildren returned %v, want %v", children, want)
	}
}

func TestDesiredChildrenIsEmptyForCountZero(t *testing.T) {
	if children := desiredChildren(newWidget(0)); len(children) != 0 {
		t.Errorf("desiredChildren returned %v for count 0.", children)
	}
}

func TestStatusForCountsChildrenAndTracksGeneration(t *testing.T) {
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
