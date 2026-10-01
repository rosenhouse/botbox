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
	"github.com/rosenhouse/botbox/pkg/target"
	kinds "k8s.io/apimachinery/pkg/runtime/schema"
)

var updateGolden = flag.Bool("update", false, "rewrite "+goldenDraws+" from what the seeds draw now")

const (
	externalSecretsTarget = "../../examples/external-secrets/target.yaml"
	goldenDraws           = "testdata/draws.golden.json"
)

// goldenSeeds are the seeds the repository runs by number: the Makefile's
// EXAMPLE_SEED runs, the README's quickstarts and the envtest tier's B2 and
// fixture runs.
var goldenSeeds = []struct {
	name    string
	declare declaration
	seeds   []int64
}{
	{"toy-widget", file(toyTarget), seedRange(1, 10)},
	{"cert-manager", file(certManagerTarget), append(seedRange(1, 10), seedRange(23, 27)...)},
	{"external-secrets", file(externalSecretsTarget), seedRange(23, 27)},
	{"toy-widget with a label fixture", toyWithALabelFixture, seedRange(1, 60)},
}

type declaration func(*testing.T) *target.Target

func file(path string) declaration {
	return func(t *testing.T) *target.Target { return loadTarget(t, path) }
}

// toyWithALabelFixture is the toy as pkg/run's TestGeneratedFixtureOps
// declares it.
func toyWithALabelFixture(t *testing.T) *target.Target {
	toy := loadTarget(t, toyTarget)
	toy.Generate.Fixtures = []target.MutableFixture{{
		GVK:    kinds.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		Name:   "widget-config",
		Mutate: []target.Path{target.MustParsePath("data.label")},
	}}
	return toy
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
		g := newGenerator(t, golden.declare(t), Options{})
		drawn[golden.name] = map[string]json.RawMessage{}
		for _, seed := range golden.seeds {
			drawn[golden.name][strconv.FormatInt(seed, 10)] = draw(t, g, seed)
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
	ops := drawOps(t, file(certManagerTarget), 23)
	if len(ops) != 1 || ops[0].Type != run.OpCreate {
		t.Errorf("Seed 23 draws %v, want a single create.", opTypes(ops))
	}
}

func TestCertManagersSeeds23To27DrawWhatTheMakefileSays(t *testing.T) {
	drawn := map[string]bool{}
	for seed := int64(23); seed <= 27; seed++ {
		for _, op := range drawOps(t, file(certManagerTarget), seed) {
			drawn[string(op.Type)] = true
			if op.Type == run.OpCreate && op.Obj.GetName() != "example" {
				drawn["a second Certificate"] = true
			}
		}
	}
	for _, want := range []string{"a second Certificate", string(run.OpRecreate), string(run.OpRestart)} {
		if !drawn[want] {
			t.Errorf("Seeds 23 to 27 draw no %s, and the Makefile says they do.", want)
		}
	}
}

func TestTheToysSeed2DrawsMoreThanTheShrunkB2Reproducer(t *testing.T) {
	// The envtest tier shrinks what seed 2 draws to three ops or fewer.
	if ops := drawOps(t, file(toyTarget), 2); len(ops) <= 3 {
		t.Errorf("Seed 2 draws %v, which leaves the shrink pass nothing to do.", opTypes(ops))
	}
}

func TestTheToyWithALabelFixtureDrawsB14sReproducerAtSeed19(t *testing.T) {
	// TestGeneratedFixtureOps finds B14 with it.
	if ops := drawOps(t, toyWithALabelFixture, 19); !revealsB14(ops) {
		t.Errorf("Seed 19 draws %v, want an updateFixture under a live Widget, then only settles, then a restart.", opTypes(ops))
	}
}

func TestOnlyARestartNextAfterTheLabelChangesRevealsB14(t *testing.T) {
	for _, test := range []struct {
		ops  []run.OpType
		want bool
	}{
		{[]run.OpType{run.OpCreate, run.OpUpdateFixture, run.OpSettle, run.OpRestart}, true},
		{[]run.OpType{run.OpCreate, run.OpUpdateFixture, run.OpUpdate, run.OpRestart}, false},
		{[]run.OpType{run.OpCreate, run.OpUpdateFixture, run.OpDeleteFixture, run.OpRestart}, false},
		{[]run.OpType{run.OpCreate, run.OpDelete, run.OpUpdateFixture, run.OpRestart}, false},
		{[]run.OpType{run.OpUpdateFixture, run.OpCreate, run.OpRestart}, false},
	} {
		var ops []run.Op
		for _, opType := range test.ops {
			ops = append(ops, run.Op{Type: opType})
		}
		if got := revealsB14(ops); got != test.want {
			t.Errorf("revealsB14(%v) = %t, want %t.", test.ops, got, test.want)
		}
	}
}

// revealsB14 says whether the label changes under a live Widget and a restart
// is the next op to reconcile it.
func revealsB14(ops []run.Op) bool {
	live := false
	for i, op := range ops {
		if op.Type.OnCR() {
			live = op.Type != run.OpDelete
		}
		next := slices.IndexFunc(ops[i+1:], func(op run.Op) bool { return op.Type != run.OpSettle })
		if op.Type == run.OpUpdateFixture && live && next >= 0 && ops[i+1+next].Type == run.OpRestart {
			return true
		}
	}
	return false
}

func drawOps(t *testing.T, declare declaration, seed int64) []run.Op {
	t.Helper()
	sequence, err := newGenerator(t, declare(t), Options{}).Draw(seed)
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
