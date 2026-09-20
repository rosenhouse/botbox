package invariant_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rosenhouse/botbox/pkg/invariant"
	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/proxy"
	"github.com/rosenhouse/botbox/pkg/target"
)

// The fixtures are the toy target of DESIGN.md §9: a Widget owning one
// ConfigMap per index, with the toy's short windows.
const (
	namespace     = "botbox-run-1"
	widgetName    = "w"
	widgetUID     = "uid-w"
	settleTimeout = 5 * time.Second
	stableWindow  = 2 * time.Second
	deleteTimeout = 10 * time.Second
	errLoop       = 5
)

var (
	widgetGVK    = schema.GroupVersionKind{Group: "toy.botbox", Version: "v1", Kind: "Widget"}
	configMapGVK = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	epoch        = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

// at returns the instant d into the run.
func at(d time.Duration) time.Time { return epoch.Add(d) }

func toyTarget() *target.Target {
	return &target.Target{
		Name:    "toy-widget",
		Primary: widgetGVK,
		Manages: []schema.GroupVersionKind{configMapGVK},
		Ready:   widgetReady,
		Properties: []target.Property{{
			ID:          "P1",
			Description: "status.ready never exceeds the number of ConfigMaps present.",
			Eval:        readyCountsChildren,
			When:        target.Checkpoint,
		}},
		Timeouts:   target.Timeouts{Settle: settleTimeout, Stable: stableWindow, Delete: deleteTimeout},
		Thresholds: target.Thresholds{ErrLoop: errLoop},
	}
}

// widgetReady is the toy's ready predicate (DESIGN.md §9).
func widgetReady(cr *unstructured.Unstructured) (bool, error) {
	observed, found, err := unstructured.NestedInt64(cr.Object, "status", "observedGeneration")
	if !found || err != nil {
		return false, err
	}
	ready, found, err := unstructured.NestedInt64(cr.Object, "status", "ready")
	if !found || err != nil {
		return false, err
	}
	count, _, err := unstructured.NestedInt64(cr.Object, "spec", "count")
	return observed == cr.GetGeneration() && ready == count, err
}

// readyCountsChildren is the toy's P1 (DESIGN.md §9).
func readyCountsChildren(cr *unstructured.Unstructured, managed []*unstructured.Unstructured) (bool, error) {
	if cr == nil {
		return true, nil
	}
	ready, found, err := unstructured.NestedInt64(cr.Object, "status", "ready")
	return !found || ready <= int64(len(managed)), err
}

// run assembles one run's Input.
type run struct {
	in    invariant.Input
	store *observe.Store
}

// newRun returns a run of the toy target whose primary CR botbox created, so
// that the CR is never a managed object (DESIGN.md §6).
func newRun() *run {
	t := toyTarget()
	store := observe.NewStore(observe.Options{Namespace: namespace, Primary: t.Primary, Manages: t.Manages})
	store.MarkBotboxCreated(widgetGVK, widgetName)
	return &run{in: invariant.Input{Target: t, History: store}, store: store}
}

// fixture marks an object as botbox's, so that it is never managed (§6).
func (r *run) fixture(obj *unstructured.Unstructured) *run {
	r.store.MarkBotboxCreated(obj.GroupVersionKind(), obj.GetName())
	return r
}

func (r *run) op(opType invariant.OpType, when time.Duration) *run {
	r.in.Ops = append(r.in.Ops, invariant.Op{Index: len(r.in.Ops), Type: opType, Time: at(when)})
	return r
}

func (r *run) checkpoint(when time.Duration, result invariant.SettleResult) *run {
	r.in.Checkpoints = append(r.in.Checkpoints, invariant.Checkpoint{
		Op: len(r.in.Ops) - 1, Time: at(when), Settle: result,
	})
	return r
}

// settled ends the last op's settle wait at when, which is where §6's quiet
// window opens. The teardown follows one T_stable later, because §5.5 step 4
// waits that long before it deletes.
func (r *run) settled(when time.Duration, result invariant.SettleResult) *run {
	return r.checkpoint(when, result).teardown(when + stableWindow + 100*time.Millisecond)
}

// teardown is when botbox began emptying the namespace (DESIGN.md §5.5).
func (r *run) teardown(when time.Duration) *run {
	r.in.Teardown = at(when)
	return r
}

func (r *run) fault(from, to time.Duration) *run {
	r.in.Faults = append(r.in.Faults, invariant.FaultWindow{Start: at(from), End: at(to)})
	return r
}

func (r *run) request(when time.Duration, req proxy.Request) *run {
	req.Start = at(when)
	r.in.Requests = append(r.in.Requests, req)
	return r
}

// requests repeats one request at a fixed interval.
func (r *run) requests(first, every time.Duration, count int, req proxy.Request) *run {
	for i := range count {
		r.request(first+time.Duration(i)*every, req)
	}
	return r
}

func (r *run) record(when time.Duration, objects ...*unstructured.Unstructured) *run {
	for _, obj := range objects {
		r.store.Record(obj.GroupVersionKind(), obj, at(when))
	}
	return r
}

func (r *run) remove(when time.Duration, objects ...*unstructured.Unstructured) *run {
	for _, obj := range objects {
		r.store.RecordDeletion(obj.GroupVersionKind(), obj, at(when))
	}
	return r
}

// through ends the observation at when, which is where the engine evaluates.
func (r *run) through(when time.Duration) invariant.Input {
	r.in.End = at(when)
	return r.in
}

type option func(*unstructured.Unstructured)

func widget(resourceVersion string, opts ...option) *unstructured.Unstructured {
	defaults := []option{uid(widgetUID), generation(1)}
	return object(widgetGVK, widgetName, resourceVersion, append(defaults, opts...)...)
}

// child is a ConfigMap of the Widget, owned by it unless an option says
// otherwise.
func child(name, resourceVersion string, opts ...option) *unstructured.Unstructured {
	return object(configMapGVK, name, resourceVersion, append([]option{ownedByWidget}, opts...)...)
}

func object(gvk schema.GroupVersionKind, name, resourceVersion string, opts ...option) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetResourceVersion(resourceVersion)
	u.SetUID(types.UID("uid-" + name))
	for _, opt := range opts {
		opt(u)
	}
	return u
}

func spec(count int64) option {
	return func(u *unstructured.Unstructured) {
		u.Object["spec"] = map[string]any{"count": count}
	}
}

func status(ready, observedGeneration int64) option {
	return func(u *unstructured.Unstructured) {
		u.Object["status"] = map[string]any{"ready": ready, "observedGeneration": observedGeneration}
	}
}

func generation(g int64) option {
	return func(u *unstructured.Unstructured) { u.SetGeneration(g) }
}

func uid(id string) option {
	return func(u *unstructured.Unstructured) { u.SetUID(types.UID(id)) }
}

func finalizers(names ...string) option {
	return func(u *unstructured.Unstructured) { u.SetFinalizers(names) }
}

func deleting(when time.Duration) option {
	return func(u *unstructured.Unstructured) {
		stamp := metav1.NewTime(at(when))
		u.SetDeletionTimestamp(&stamp)
	}
}

func ownedByWidget(u *unstructured.Unstructured) {
	u.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: widgetGVK.GroupVersion().String(),
		Kind:       widgetGVK.Kind,
		Name:       widgetName,
		UID:        widgetUID,
	}})
}

func orphaned(u *unstructured.Unstructured) { u.SetOwnerReferences(nil) }

// ownedByGhost names an owner no snapshot holds, which §6 ignores.
func ownedByGhost(u *unstructured.Unstructured) {
	u.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: widgetGVK.GroupVersion().String(),
		Kind:       widgetGVK.Kind,
		Name:       "gone",
		UID:        "uid-gone",
	}})
}

func nothing(*unstructured.Unstructured) {}

func created(when time.Duration) option {
	return func(u *unstructured.Unstructured) {
		u.SetCreationTimestamp(metav1.NewTime(at(when)))
	}
}

func managedFields(manager string) option {
	return func(u *unstructured.Unstructured) {
		u.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate}})
	}
}

func condition(status string, lastTransition time.Duration) option {
	return func(u *unstructured.Unstructured) {
		u.Object["status"] = map[string]any{"conditions": []any{map[string]any{
			"type":               "Ready",
			"status":             status,
			"lastTransitionTime": at(lastTransition).Format(time.RFC3339),
		}}}
	}
}

func labelled(value string) option {
	return func(u *unstructured.Unstructured) { u.SetLabels(map[string]string{"app": value}) }
}

func annotated(value string) option {
	return func(u *unstructured.Unstructured) { u.SetAnnotations(map[string]string{"note": value}) }
}

func data(value string) option {
	return func(u *unstructured.Unstructured) {
		u.Object["data"] = map[string]any{"index": value}
	}
}

// get is the read a quiet target does not make.
func get(name string) proxy.Request {
	return proxy.Request{Verb: "get", Version: "v1", Resource: "configmaps", Namespace: namespace, Name: name, Status: 200}
}

func createChild(name string) proxy.Request {
	created := get(name)
	created.Verb, created.Name, created.Status = "create", name, 201
	return created
}

func failedGet(name string, status int) proxy.Request {
	failed := get(name)
	failed.Status = status
	return failed
}

func failedDelete(name string, status int) proxy.Request {
	failed := get(name)
	failed.Verb, failed.Status = "delete", status
	return failed
}

// failedUpdate is a write the API server turned away.
func failedUpdate(name string, status int) proxy.Request {
	failed := get(name)
	failed.Verb, failed.Status = "update", status
	return failed
}

func failedWidgetGet(status int) proxy.Request {
	return proxy.Request{
		Verb: "get", Group: widgetGVK.Group, Version: widgetGVK.Version, Resource: "widgets",
		Namespace: namespace, Name: widgetName, Status: status,
	}
}

func watch() proxy.Request {
	return proxy.Request{Verb: "watch", Version: "v1", Resource: "configmaps", Namespace: namespace, Watch: true, Status: 200}
}

// failedWatch is the watch envtest's flow control turned away.
func failedWatch(status int) proxy.Request {
	failed := watch()
	failed.Status = status
	return failed
}

func leaseUpdate() proxy.Request {
	return proxy.Request{
		Verb: "update", Group: "coordination.k8s.io", Version: "v1", Resource: "leases",
		Namespace: namespace, Name: "toy-widget", Status: 200,
	}
}

func statusPatch() proxy.Request {
	return proxy.Request{
		Verb: "patch", Group: widgetGVK.Group, Version: widgetGVK.Version, Resource: "widgets",
		Namespace: namespace, Name: widgetName, Subresource: "status", Status: 200,
	}
}

// fired returns the single violation the check reported.
func fired(t *testing.T, check invariant.Check, in invariant.Input) invariant.Violation {
	t.Helper()
	result := evaluate(t, check, in)
	if len(result.Violations) != 1 {
		t.Fatalf("%s reported %d violations, want exactly one: %v", result.ID, len(result.Violations), statements(result))
	}
	return result.Violations[0]
}

// silent fails the test unless the check reported nothing.
func silent(t *testing.T, check invariant.Check, in invariant.Input) invariant.Result {
	t.Helper()
	result := evaluate(t, check, in)
	if len(result.Violations) > 0 {
		t.Fatalf("%s reported %v, want no violation.", result.ID, statements(result))
	}
	return result
}

func evaluate(t *testing.T, check invariant.Check, in invariant.Input) invariant.Result {
	t.Helper()
	result, err := check(in)
	if err != nil {
		t.Fatalf("The check returned an error: %v", err)
	}
	return result
}

func statements(result invariant.Result) []string {
	out := make([]string, len(result.Violations))
	for i, v := range result.Violations {
		out[i] = v.Statement
	}
	return out
}
