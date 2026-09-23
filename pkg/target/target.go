// Package target loads a target.yaml (DESIGN.md §8.1) into the Go form of
// DESIGN.md §8.2. Every error Load returns is a configuration error, which the
// CLI reports as exit code 2 (§11).
package target

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/observe"
)

// ReadyFunc is the target's readiness predicate (DESIGN.md §6, G4). An error
// means "not ready"; it is an *EvalError, which a report quotes (§8.4).
type ReadyFunc func(cr *unstructured.Unstructured) (bool, error)

// PropertyFunc evaluates a declared property over the primary CR and the
// managed objects. An error is a configuration error (DESIGN.md §8.4).
type PropertyFunc func(cr *unstructured.Unstructured, managed []*unstructured.Unstructured) (bool, error)

// EqualFunc compares snapshots taken around a Restart (DESIGN.md §6, G5).
type EqualFunc func(a, b observe.Snapshot) bool

// PropertyWhen says where a property is evaluated (DESIGN.md §8.1).
type PropertyWhen string

const (
	Always     PropertyWhen = "always"
	Checkpoint PropertyWhen = "checkpoint"
	End        PropertyWhen = "end"
)

// Property is a per-target check, identified as P1..Pn.
type Property struct {
	ID, Description string
	Eval            PropertyFunc
	When            PropertyWhen
}

// GenerateSpec constrains sequence generation (DESIGN.md §8.3).
type GenerateSpec struct {
	// Mutate lists dotted paths the generator may change. Empty means every
	// schema path.
	Mutate []string
	// Overlay tightens the CRD schema at a dotted path.
	Overlay map[string]map[string]any
}

// LaunchSpec says how to run the target (DESIGN.md §5.1).
type LaunchSpec struct {
	// Binary is relative to the working directory, or a name on PATH.
	Binary string `json:"binary"`
	// Args and the values of Env carry $KUBECONFIG and $NAMESPACE wherever the
	// launcher must substitute the kubeconfig path and the run namespace.
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

// Check reports a binary that botbox could not exec.
func (l LaunchSpec) Check() error {
	_, err := exec.LookPath(l.Binary)
	switch {
	case err == nil:
		return nil
	case strings.Contains(l.Binary, "/") && !filepath.IsAbs(l.Binary):
		wd, _ := os.Getwd()
		return fmt.Errorf("launch.binary: %w, in the working directory %s; the path is relative to where botbox runs, not to target.yaml", err, wd)
	default:
		return fmt.Errorf("launch.binary: %w", err)
	}
}

// Timeouts are the run's waits (DESIGN.md §6).
type Timeouts struct{ Settle, Stable, Delete time.Duration }

// Thresholds hold N_errloop for G6 (DESIGN.md §6).
type Thresholds struct{ ErrLoop int }

// Defaults for a target that declares neither block (DESIGN.md §6).
var (
	DefaultTimeouts   = Timeouts{Settle: 30 * time.Second, Stable: 10 * time.Second, Delete: 60 * time.Second}
	DefaultThresholds = Thresholds{ErrLoop: 20}
)

// DefaultReady is the readiness predicate of DESIGN.md §6, used by a target
// that declares none.
const DefaultReady = "has(status.observedGeneration) && status.observedGeneration == metadata.generation"

// Target is a controller under test, its CRDs, and how to exercise it.
type Target struct {
	Name, Version string
	CRDs          []string
	Primary       schema.GroupVersionKind
	Sample        *unstructured.Unstructured
	Fixtures      []*unstructured.Unstructured
	Manages       []schema.GroupVersionKind
	Selector      labels.Selector
	Ready         ReadyFunc
	// Equal is nil unless the target names a hook. The invariant engine then
	// applies the §6 default equality together with EqualIgnore.
	Equal       EqualFunc
	EqualIgnore []Path
	Properties  []Property
	Generate    GenerateSpec
	Launch      LaunchSpec
	Timeouts    Timeouts
	Thresholds  Thresholds
}

// CheckScopes refuses every kind of the target's that the mapper serves at
// cluster scope. It leaves a kind the mapper does not know to the run.
func (t *Target) CheckScopes(mapper meta.RESTMapper) error {
	return t.refuseClusterScoped(func(gvk schema.GroupVersionKind) bool {
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		return err == nil && mapping.Scope.Name() == meta.RESTScopeNameRoot
	})
}

// refuseClusterScoped names every cluster-scoped kind the target declares.
func (t *Target) refuseClusterScoped(clusterScoped func(schema.GroupVersionKind) bool) error {
	var found []string
	if clusterScoped(t.Primary) {
		found = append(found, "the primary "+kindName(t.Primary))
	}
	for _, gvk := range t.Manages {
		if clusterScoped(gvk) {
			found = append(found, "the managed "+kindName(gvk))
		}
	}
	for _, fixture := range t.Fixtures {
		if gvk := fixture.GroupVersionKind(); clusterScoped(gvk) {
			found = append(found, fmt.Sprintf("the fixture %s %s", kindName(gvk), fixture.GetName()))
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("a run owns one namespace, so botbox cannot test these cluster-scoped kinds: %s", strings.Join(found, ", "))
}

// kindName writes a kind as target.yaml declares it.
func kindName(gvk schema.GroupVersionKind) string {
	if gvk.Group == "" {
		return gvk.Version + "/" + gvk.Kind
	}
	return gvk.Group + "/" + gvk.Version + "/" + gvk.Kind
}

// WatchedKinds are the kinds botbox watches: the primary CR and every managed
// kind. The collector resolves an owner only among them (DESIGN.md §5.8), and
// managed objects are commonly owned by the CR.
func (t *Target) WatchedKinds() []schema.GroupVersionKind {
	kinds := []schema.GroupVersionKind{t.Primary}
	for _, gvk := range t.Manages {
		if !slices.Contains(kinds, gvk) {
			kinds = append(kinds, gvk)
		}
	}
	return kinds
}
