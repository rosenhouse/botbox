// Package controller reconciles Widgets (DESIGN.md §9).
package controller

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	toyv1 "github.com/rosenhouse/botbox/targets/toy-widget/api/v1"
)

// Finalizer names the cleanup path a deleted Widget runs. envtest has no
// garbage collector, so it is the only path that deletes children there
// (DESIGN.md §5.8).
const Finalizer = "widget.botbox/cleanup"

const indexKey = "index"

// NewScheme returns a scheme holding the core Kubernetes types and the Widget API.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, toyv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, fmt.Errorf("building the scheme: %w", err)
		}
	}
	return scheme, nil
}

// Reconciler keeps a Widget's ConfigMaps in step with its spec.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Bug    Bug
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&toyv1.Widget{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	widget := &toyv1.Widget{}
	if err := r.Get(ctx, req.NamespacedName, widget); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !widget.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.cleanUp(ctx, widget)
	}

	if controllerutil.AddFinalizer(widget, Finalizer) {
		if err := r.Update(ctx, widget); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding the finalizer: %w", err)
		}
	}

	ready, err := r.syncChildren(ctx, widget)
	if err != nil {
		return ctrl.Result{}, err
	}

	status, changed := statusFor(widget, ready)
	if !changed {
		return ctrl.Result{}, nil
	}
	widget.Status = status
	if err := r.Status().Update(ctx, widget); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating the status: %w", err)
	}
	return ctrl.Result{}, nil
}

// desiredChildren returns the ConfigMaps a Widget requires, one per index
// below spec.count.
func desiredChildren(widget *toyv1.Widget) []corev1.ConfigMap {
	children := make([]corev1.ConfigMap, widget.Spec.Count)
	for index := range children {
		children[index] = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: widget.Namespace,
				Name:      fmt.Sprintf("%s-%d", widget.Name, index),
			},
			Data: map[string]string{indexKey: strconv.Itoa(index)},
		}
	}
	return children
}

// statusFor returns the status a Widget should carry and whether that differs
// from the one it carries now.
func statusFor(widget *toyv1.Widget, ready int) (toyv1.WidgetStatus, bool) {
	status := toyv1.WidgetStatus{Ready: ready, ObservedGeneration: widget.Generation}
	return status, status != widget.Status
}

// syncChildren creates the ConfigMaps the spec requires, deletes the ones it
// no longer requires, and returns how many required ones are present.
func (r *Reconciler) syncChildren(ctx context.Context, widget *toyv1.Widget) (int, error) {
	desired := desiredChildren(widget)
	for index := range desired {
		if err := r.ensureChild(ctx, widget, &desired[index]); err != nil {
			return 0, err
		}
	}

	required := sets.New[string]()
	for index := range desired {
		required.Insert(desired[index].Name)
	}

	owned, err := r.ownedChildren(ctx, widget)
	if err != nil {
		return 0, err
	}
	ready := 0
	for index := range owned {
		if required.Has(owned[index].Name) {
			ready++
			continue
		}
		if err := r.deleteChild(ctx, &owned[index]); err != nil {
			return 0, err
		}
	}
	return ready, nil
}

func (r *Reconciler) ensureChild(ctx context.Context, widget *toyv1.Widget, desired *corev1.ConfigMap) error {
	child := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, child, func() error {
		child.Data = desired.Data
		return controllerutil.SetControllerReference(widget, child, r.Scheme)
	})
	if apierrors.IsAlreadyExists(err) {
		return nil // The cache had not caught up; its create event reconciles again.
	}
	if err != nil {
		return fmt.Errorf("ensuring ConfigMap %s: %w", desired.Name, err)
	}
	return nil
}

// ownedChildren returns the ConfigMaps in the Widget's namespace that the
// Widget controls.
func (r *Reconciler) ownedChildren(ctx context.Context, widget *toyv1.Widget) ([]corev1.ConfigMap, error) {
	configMaps := &corev1.ConfigMapList{}
	if err := r.List(ctx, configMaps, client.InNamespace(widget.Namespace)); err != nil {
		return nil, fmt.Errorf("listing ConfigMaps: %w", err)
	}
	var owned []corev1.ConfigMap
	for _, configMap := range configMaps.Items {
		if owner := metav1.GetControllerOf(&configMap); owner != nil && owner.UID == widget.UID {
			owned = append(owned, configMap)
		}
	}
	return owned, nil
}

// cleanUp deletes the children a deleted Widget still owns and releases the
// Widget once none remain.
func (r *Reconciler) cleanUp(ctx context.Context, widget *toyv1.Widget) error {
	owned, err := r.ownedChildren(ctx, widget)
	if err != nil {
		return err
	}
	for index := range owned {
		if err := r.deleteChild(ctx, &owned[index]); err != nil {
			return err
		}
	}
	if len(owned) > 0 {
		return nil // Each deletion reconciles the Widget again.
	}
	if controllerutil.RemoveFinalizer(widget, Finalizer) {
		if err := r.Update(ctx, widget); err != nil {
			return fmt.Errorf("removing the finalizer: %w", err)
		}
	}
	return nil
}

func (r *Reconciler) deleteChild(ctx context.Context, child *corev1.ConfigMap) error {
	if err := r.Delete(ctx, child); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting ConfigMap %s: %w", child.Name, err)
	}
	return nil
}
