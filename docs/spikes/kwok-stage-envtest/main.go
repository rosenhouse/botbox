package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"sigs.k8s.io/kwok/pkg/apis/internalversion"
	"sigs.k8s.io/kwok/pkg/config"
	"sigs.k8s.io/kwok/pkg/config/resources"
	"sigs.k8s.io/kwok/pkg/kwok/controllers"
	"sigs.k8s.io/kwok/pkg/utils/informer"
	"sigs.k8s.io/kwok/pkg/utils/lifecycle"
)

// The stages model Bar's controller. A Bar whose Ready condition lags its
// generation becomes Ready 200 ms later, or Ready False while the mode
// annotation says degraded.
const stagesYAML = `
apiVersion: kwok.x-k8s.io/v1alpha1
kind: Stage
metadata:
  name: bar-ready
spec:
  resourceRef: {apiGroup: example.com/v1, kind: Bar}
  selector:
    matchExpressions:
    - cel: {expression: '!has(self.metadata.deletionTimestamp)'}
    - cel: {expression: '!has(self.metadata.annotations) || !("spike.example.com/mode" in self.metadata.annotations)'}
    - cel: {expression: '!has(self.status) || !has(self.status.conditions) || !self.status.conditions.exists(c, c.type == "Ready" && c.status == "True" && c.observedGeneration == self.metadata.generation)'}
  delay: {durationMilliseconds: 200}
  steps:
  - patch:
      subresource: status
      root: status
      type: merge
      template: |
        observedGeneration: {{ .metadata.generation }}
        conditions:
        - {type: Ready, status: "True", observedGeneration: {{ .metadata.generation }}, reason: Modelled}
---
apiVersion: kwok.x-k8s.io/v1alpha1
kind: Stage
metadata:
  name: bar-degraded
spec:
  resourceRef: {apiGroup: example.com/v1, kind: Bar}
  selector:
    matchExpressions:
    - cel: {expression: '!has(self.metadata.deletionTimestamp)'}
    - cel: {expression: 'has(self.metadata.annotations) && self.metadata.annotations["spike.example.com/mode"] == "degraded"'}
    - cel: {expression: '!has(self.status) || !has(self.status.conditions) || !self.status.conditions.exists(c, c.type == "Ready" && c.status == "False" && c.observedGeneration == self.metadata.generation)'}
  delay: {durationMilliseconds: 200}
  steps:
  - patch:
      subresource: status
      root: status
      type: merge
      template: |
        observedGeneration: {{ .metadata.generation }}
        conditions:
        - {type: Ready, status: "False", observedGeneration: {{ .metadata.generation }}, reason: Degraded}
`

const modeAnnotation = "spike.example.com/mode"

var gvr = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "bars"}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "spike failed:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	env := &envtest.Environment{CRDDirectoryPaths: []string{"crds"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		return fmt.Errorf("starting envtest: %w", err)
	}
	defer env.Stop()

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("building the dynamic client: %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("building the discovery client: %w", err)
	}
	groups, err := restmapper.GetAPIGroupResources(disco)
	if err != nil {
		return fmt.Errorf("reading the API groups: %w", err)
	}
	if err := startStages(ctx, dyn, restmapper.NewDiscoveryRESTMapper(groups)); err != nil {
		return err
	}

	namespaces := dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"})
	ns := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "spike"}}}
	if _, err := namespaces.Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating the namespace: %w", err)
	}
	bars := dyn.Resource(gvr).Namespace("spike")
	bar := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1", "kind": "Bar",
		"metadata": map[string]any{"name": "bar-1"}, "spec": map[string]any{"size": int64(1)}}}
	if _, err := bars.Create(ctx, bar, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating the Bar: %w", err)
	}
	if err := report(ctx, bars, "create", 1, "True"); err != nil {
		return err
	}

	if _, err := bars.Patch(ctx, "bar-1", types.MergePatchType, []byte(`{"spec":{"size":2}}`), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("changing the spec: %w", err)
	}
	if err := report(ctx, bars, "spec change", 2, "True"); err != nil {
		return err
	}

	degraded := fmt.Sprintf(`{"metadata":{"annotations":{%q:"degraded"}}}`, modeAnnotation)
	if _, err := bars.Patch(ctx, "bar-1", types.MergePatchType, []byte(degraded), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("annotating the Bar: %w", err)
	}
	if err := report(ctx, bars, "degraded annotation", 2, "False"); err != nil {
		return err
	}

	restored := fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, modeAnnotation)
	if _, err := bars.Patch(ctx, "bar-1", types.MergePatchType, []byte(restored), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("removing the annotation: %w", err)
	}
	if err := report(ctx, bars, "annotation removed", 2, "True"); err != nil {
		return err
	}

	before, err := bars.Get(ctx, "bar-1", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the Bar: %w", err)
	}
	time.Sleep(10 * time.Second)
	after, err := bars.Get(ctx, "bar-1", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the Bar again: %w", err)
	}
	fmt.Printf("quiet: resourceVersion %s, then %s 10s later\n", before.GetResourceVersion(), after.GetResourceVersion())

	if err := bars.Delete(ctx, "bar-1", metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("deleting the Bar: %w", err)
	}
	deleted := time.Now()
	for time.Since(deleted) < 5*time.Second {
		_, err := bars.Get(ctx, "bar-1", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			fmt.Printf("delete: gone after %v\n", time.Since(deleted).Round(time.Millisecond))
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the deleted Bar: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("bar-1 was still there 5s after its delete")
}

// startStages runs one kwok stage controller over bars, with the stages
// above, as kwok's own Controller.initStageController does.
func startStages(ctx context.Context, dyn dynamic.Interface, mapper meta.RESTMapper) error {
	var stages []*internalversion.Stage
	for _, doc := range strings.Split(stagesYAML, "\n---\n") {
		stage, err := config.UnmarshalWithType[*internalversion.Stage](doc)
		if err != nil {
			return fmt.Errorf("reading a stage: %w", err)
		}
		stages = append(stages, stage)
	}
	lc, err := lifecycle.NewLifecycle(stages)
	if err != nil {
		return fmt.Errorf("building the lifecycle: %w", err)
	}
	barInformer := informer.NewInformer[*unstructured.Unstructured, *unstructured.UnstructuredList](dyn.Resource(gvr).Namespace("spike"))
	events := make(chan informer.Event[*unstructured.Unstructured], 1)
	if err := barInformer.Watch(ctx, informer.Option{}, events); err != nil {
		return fmt.Errorf("watching bars: %w", err)
	}
	// A merge patch needs no schema. A strategic one reads the CRD's OpenAPI
	// v3 document through kwok's patch.NewPatchMetaFromOpenAPI3.
	model, err := controllers.NewStageController(controllers.StageControllerConfig{
		DynamicClient:        dyn,
		RESTMapper:           mapper,
		GVR:                  gvr,
		Lifecycle:            resources.NewStaticGetter(lc),
		PlayStageParallelism: 1,
	})
	if err != nil {
		return fmt.Errorf("building the stage controller: %w", err)
	}
	if err := model.Start(ctx, events); err != nil {
		return fmt.Errorf("starting the stage controller: %w", err)
	}
	return nil
}

// report waits up to 10 s for the Bar's Ready condition to carry status for
// generation, and prints how long that took since the step.
func report(ctx context.Context, bars dynamic.ResourceInterface, step string, generation int64, status string) error {
	start := time.Now()
	for time.Since(start) < 10*time.Second {
		bar, err := bars.Get(ctx, "bar-1", metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading the Bar: %w", err)
		}
		conditions, _, _ := unstructured.NestedSlice(bar.Object, "status", "conditions")
		for _, c := range conditions {
			cond, _ := c.(map[string]any)
			observed, _ := cond["observedGeneration"].(int64)
			if cond["type"] == "Ready" && cond["status"] == status && observed == generation && bar.GetGeneration() == generation {
				fmt.Printf("%s: Ready %s for generation %d after %v\n", step, status, generation, time.Since(start).Round(time.Millisecond))
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("after the %s, bar-1 never carried Ready %s for generation %d", step, status, generation)
}
