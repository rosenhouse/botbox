//go:build envtest

package run_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
)

// draw stands in for pkg/generate until it lands: it draws one sequence for
// the target from a seed (DESIGN.md §5.4). What M5 accepts is that the harness
// finds and shrinks the failure without a hand-written sequence, so all this
// has to be is deterministic.
func draw(t *target.Target, seed int64) run.Sequence {
	widget := t.Sample.DeepCopy()
	if err := unstructured.SetNestedField(widget.Object, seed%2+1, "spec", "count"); err != nil {
		panic(err)
	}
	ops := []run.Op{
		{Type: run.OpCreate, Obj: widget},
		{Type: run.OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": seed%4 + 1}}},
		{Type: run.OpSettle},
		{Type: run.OpDelete},
	}
	for i := range ops {
		ops[i].Index = i
	}
	return run.Sequence{Seed: seed, Target: t.Name, Ops: ops}
}

// The acceptance of DESIGN.md §10 M5: with --bug=2 the harness finds a failure
// in a sequence it drew and shrinks it to three ops or fewer.
func TestShrinkingAGeneratedFailure(t *testing.T) {
	ctx := t.Context()
	toy := loadTarget(t, buildToy(t))
	toy.Launch.Args = append(toy.Launch.Args, "--bug=2")
	// The toy converges in milliseconds, and a shrink pass replays many times,
	// so the waits are cut to what keeps the tier inside its budget (§11).
	toy.Timeouts = target.Timeouts{Settle: time.Second, Stable: 500 * time.Millisecond, Delete: 3 * time.Second}
	testCluster := startCluster(t, toy.CRDs)
	dir := t.TempDir()
	replay := func(ctx context.Context, candidate run.Sequence) (run.Result, error) {
		return run.Run(ctx, toy, candidate, run.Options{
			Dir: filepath.Join(dir, "replay"), Config: testCluster.Config(), Check: run.Engine{},
		})
	}

	drawn := draw(toy, 8675309)
	found, err := run.Run(ctx, toy, drawn, run.Options{
		Dir: filepath.Join(dir, "run-1"), Config: testCluster.Config(), Check: run.Engine{},
	})
	if err != nil {
		t.Fatalf("The generated run failed: %v", err)
	}
	if found.Violation == nil {
		t.Fatalf("The generated run found nothing, and the toy runs with bug 2.")
	}

	shrunk := run.Shrink(ctx, drawn, *found.Violation, replay)

	if len(shrunk.Ops) > 3 {
		t.Errorf("Shrink returned %d ops, want the three or fewer M5 accepts: %v", len(shrunk.Ops), shrunk.Ops)
	}
	if err := shrunk.Validate(); err != nil {
		t.Errorf("Shrink returned a sequence that does not validate: %v", err)
	}
	again, err := replay(ctx, shrunk)
	if err != nil {
		t.Fatalf("Replaying the minimized sequence failed: %v", err)
	}
	if again.Violation == nil || again.Violation.ID != found.Violation.ID {
		t.Errorf("The minimized sequence reported %+v, want the %s the drawn sequence failed.",
			again.Violation, found.Violation.ID)
	}
}
