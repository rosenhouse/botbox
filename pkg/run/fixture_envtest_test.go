//go:build envtest

package run_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/rosenhouse/botbox/pkg/generate"
	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
)

// lastFixtureSeed is the last seed fixtureSequences draws.
const lastFixtureSeed = 60

// seedThatFindsB14 draws an updateFixture and then a restart.
const seedThatFindsB14 = 19

// A correct toy passes what generation draws on its fixtures, so the fixture
// ops find bugs rather than harness artefacts.
func TestGeneratedFixtureOps(t *testing.T) {
	t.Parallel()
	toy := loadTarget(t, buildToy(t))
	toy.Generate.Fixtures = []target.MutableFixture{{
		GVK: configMapKind, Name: "widget-config", Mutate: []target.Path{target.MustParsePath("data.label")},
	}}
	testCluster := startCluster(t, toy.CRDs)
	generator, err := generate.New(toy, generate.Options{})
	if err != nil {
		t.Fatalf("Reading the toy's schema failed: %v", err)
	}
	execute := func(t *testing.T, against *target.Target, sequence run.Sequence) run.Result {
		t.Helper()
		result, err := run.Run(t.Context(), against, sequence, run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})
		if err != nil {
			t.Fatalf("Seed %d failed to run: %v", sequence.Seed, err)
		}
		return result
	}
	sequences := fixtureSequences(t, generator)

	t.Run("passes the correct toy", func(t *testing.T) {
		for _, sequence := range sequences {
			t.Run(strconv.FormatInt(sequence.Seed, 10), func(t *testing.T) {
				t.Parallel()
				if result := execute(t, toy, sequence); result.Violation != nil {
					t.Errorf("Seed %d reported %s, want none: the toy runs without a bug.", sequence.Seed, result.Violation)
				}
			})
		}
	})

	t.Run("finds B14", func(t *testing.T) {
		buggy := *toy
		buggy.Launch.Args = append(slices.Clone(toy.Launch.Args), "--bug=14")
		i := slices.IndexFunc(sequences, func(sequence run.Sequence) bool { return sequence.Seed == seedThatFindsB14 })
		if i < 0 {
			t.Fatalf("Seed %d draws no fixture op. Re-pin the seed.", seedThatFindsB14)
		}

		result := execute(t, &buggy, sequences[i])

		if result.Violation == nil || result.Violation.ID != "G5" {
			t.Errorf("Seed %d reported %v, want G5: B14 misses the label until a restart.", seedThatFindsB14, result.Violation)
		}
	})
}

// fixtureSequences are the sequences seeds 1 to lastFixtureSeed draw that act
// on a fixture.
func fixtureSequences(t *testing.T, generator *generate.Generator) []run.Sequence {
	t.Helper()
	var sequences []run.Sequence
	drawn := map[run.OpType]int{}
	for seed := int64(1); seed <= lastFixtureSeed; seed++ {
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
		if acts {
			sequences = append(sequences, sequence)
		}
	}
	if drawn[run.OpUpdateFixture] < 5 || drawn[run.OpDeleteFixture] < 5 {
		t.Fatalf("Seeds 1 to %d draw %v, want at least five of each fixture op.", lastFixtureSeed, drawn)
	}
	return sequences
}
