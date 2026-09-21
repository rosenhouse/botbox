//go:build envtest

package run_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/generate"
	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
)

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

	generator, err := generate.New(toy, generate.Options{})
	if err != nil {
		t.Fatalf("Reading the toy's schema failed: %v", err)
	}
	drawn, err := generator.Draw(8675309)
	if err != nil {
		t.Fatalf("Drawing a sequence failed: %v", err)
	}
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
