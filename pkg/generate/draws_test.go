package generate

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/rosenhouse/botbox/pkg/run"
)

var updateGolden = flag.Bool("update", false, "rewrite "+goldenDraws+" from what the seeds draw now")

const (
	externalSecretsTarget = "../../examples/external-secrets/target.yaml"
	goldenDraws           = "testdata/draws.golden.json"
)

// goldenSeeds are the seeds the repository runs by number: the Makefile's
// EXAMPLE_SEED runs, the README's quickstarts and the envtest tier's B2
// reproducer.
var goldenSeeds = []struct {
	path  string
	seeds []int64
}{
	{toyTarget, seedRange(1, 10)},
	{certManagerTarget, append(seedRange(1, 10), seedRange(23, 27)...)},
	{externalSecretsTarget, seedRange(23, 27)},
}

func seedRange(first, last int64) []int64 {
	var seeds []int64
	for seed := first; seed <= last; seed++ {
		seeds = append(seeds, seed)
	}
	return seeds
}

// A seed names a sequence only for one build of botbox, so a change to what a
// seed draws has to be deliberate: rerun with -update and say why.
func TestSeedsDrawTheGoldenSequences(t *testing.T) {
	drawn := map[string]map[string]json.RawMessage{}
	for _, golden := range goldenSeeds {
		loaded := loadTarget(t, golden.path)
		g := newGenerator(t, loaded, Options{})
		drawn[loaded.Name] = map[string]json.RawMessage{}
		for _, seed := range golden.seeds {
			drawn[loaded.Name][strconv.FormatInt(seed, 10)] = draw(t, g, seed)
		}
	}
	encoded, err := json.MarshalIndent(drawn, "", "  ")
	if err != nil {
		t.Fatalf("Encoding the draws failed: %v.", err)
	}
	encoded = append(encoded, '\n')
	if *updateGolden {
		if err := os.WriteFile(goldenDraws, encoded, 0o644); err != nil {
			t.Fatalf("Writing %s failed: %v.", goldenDraws, err)
		}
		return
	}

	recorded, err := os.ReadFile(goldenDraws)
	if err != nil {
		t.Fatalf("Reading %s failed: %v.", goldenDraws, err)
	}
	if bytes.Equal(recorded, encoded) {
		return
	}
	var golden map[string]map[string]json.RawMessage
	if err := json.Unmarshal(recorded, &golden); err != nil {
		t.Fatalf("Reading %s failed: %v.", goldenDraws, err)
	}
	for _, name := range slices.Sorted(maps.Keys(drawn)) {
		for _, seed := range slices.Sorted(maps.Keys(drawn[name])) {
			if !equalJSON(golden[name][seed], drawn[name][seed]) {
				t.Errorf("Seed %s draws a different sequence for %s:\n%s", seed, name, drawn[name][seed])
			}
		}
	}
	t.Errorf("The draws differ from %s. If the change is deliberate, rerun with -update and say so in the commit.",
		goldenDraws)
}

func TestCertManagersSeed23DrawsOneCreate(t *testing.T) {
	// The Makefile's negative control runs it alone, since one op costs no
	// replay to minimize.
	ops := drawOps(t, certManagerTarget, 23)
	if len(ops) != 1 || ops[0].Type != run.OpCreate {
		t.Errorf("Seed 23 draws %v, want a single create.", opTypes(ops))
	}
}

func TestCertManagersSeeds23To27DrawEveryOpTheExampleExercises(t *testing.T) {
	drawn := map[run.OpType]bool{}
	for seed := int64(23); seed <= 27; seed++ {
		for _, op := range drawOps(t, certManagerTarget, seed) {
			drawn[op.Type] = true
		}
	}
	for _, want := range []run.OpType{run.OpCreate, run.OpDelete, run.OpRecreate, run.OpRestart, run.OpDeleteManaged} {
		if !drawn[want] {
			t.Errorf("Seeds 23 to 27 draw no %s, and the Makefile says they do.", want)
		}
	}
}

func TestTheToysSeed2DrawsMoreThanTheShrunkB2Reproducer(t *testing.T) {
	// The envtest tier shrinks what seed 2 draws to three ops or fewer.
	if ops := drawOps(t, toyTarget, 2); len(ops) <= 3 {
		t.Errorf("Seed 2 draws %v, which leaves the shrink pass nothing to do.", opTypes(ops))
	}
}

func drawOps(t *testing.T, path string, seed int64) []run.Op {
	t.Helper()
	sequence, err := newGenerator(t, loadTarget(t, path), Options{}).Draw(seed)
	if err != nil {
		t.Fatalf("Draw(%d) failed: %v.", seed, err)
	}
	return sequence.Ops
}

func opTypes(ops []run.Op) []run.OpType {
	var types []run.OpType
	for _, op := range ops {
		types = append(types, op.Type)
	}
	return types
}
