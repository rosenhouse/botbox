//go:build envtest

package run_test

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rosenhouse/botbox/pkg/generate"
	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
)

// A correct target passes what generation draws on its fixtures, so the
// fixture ops find bugs rather than harness artefacts.
func TestTheCorrectToyPassesGeneratedFixtureOps(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	toy := loadTarget(t, buildToy(t))
	toy.Generate.Fixtures = []target.MutableFixture{{
		GVK: configMapKind, Name: "widget-config", Mutate: []target.Path{target.MustParsePath("data.label")},
	}}
	testCluster := startCluster(t, toy.CRDs)
	generator, err := generate.New(toy, generate.Options{})
	if err != nil {
		t.Fatalf("Reading the toy's schema failed: %v", err)
	}

	drawn := map[run.OpType]int{}
	for seed := int64(1); drawn[run.OpUpdateFixture] < 2 || drawn[run.OpDeleteFixture] < 2; seed++ {
		sequence, err := generator.Draw(seed)
		if err != nil {
			t.Fatalf("Drawing seed %d failed: %v", seed, err)
		}
		acts := false
		for _, op := range sequence.Ops {
			if op.Type == run.OpUpdateFixture || op.Type == run.OpDeleteFixture {
				drawn[op.Type]++
				acts = true
			}
		}
		if !acts {
			continue
		}
		result, err := run.Run(ctx, toy, sequence, run.Options{
			Dir: filepath.Join(t.TempDir(), strconv.FormatInt(seed, 10)), Config: testCluster.Config(), Check: run.Engine{},
		})
		if err != nil {
			t.Fatalf("Seed %d failed to run: %v", seed, err)
		}
		if result.Violation != nil {
			t.Errorf("Seed %d reported %s, want none: the toy runs without a bug.", seed, result.Violation)
		}
	}
}
