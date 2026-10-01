package generate

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
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

type goldenTarget struct {
	name    string
	declare declaration
	seeds   []int64
}

// goldenSeeds are the seeds the Makefile and the envtest tier run by number.
func goldenSeeds(t *testing.T) []goldenTarget {
	example := makefileSeeds(t, "EXAMPLE")
	return []goldenTarget{
		{"toy-widget", file(toyTarget), append(seedRange(1, 10), makefileSeeds(t, "KIND")...)},
		{"cert-manager", file(certManagerTarget), append(seedRange(1, 10), example...)},
		{"external-secrets", file(externalSecretsTarget), example},
		{"toy-widget with a label fixture", toyWithALabelFixture, seedRange(1, 60)},
	}
}

// makefileSeeds are the seeds the Makefile's <tier>_SEED and <tier>_RUNS draw.
func makefileSeeds(t *testing.T, tier string) []int64 {
	t.Helper()
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	pin := func(name string) int64 {
		m := regexp.MustCompile(`(?m)^` + name + ` \?= (\d+)$`).FindSubmatch(makefile)
		if m == nil {
			t.Fatalf("The Makefile sets no %s.", name)
		}
		value, _ := strconv.ParseInt(string(m[1]), 10, 64)
		return value
	}
	first := pin(tier + "_SEED")
	return seedRange(first, first+pin(tier+"_RUNS")-1)
}

type declaration func(*testing.T) *target.Target

func file(path string) declaration {
	return func(t *testing.T) *target.Target { return loadTarget(t, path) }
}

// toyWithALabelFixture is the toy as pkg/run's TestGeneratedFixtureOps
// declares it. That test fails unless it draws what the golden file records.
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
	for _, declared := range goldenSeeds(t) {
		g := newGenerator(t, declared.declare(t), Options{})
		drawn[declared.name] = map[string]json.RawMessage{}
		for _, seed := range declared.seeds {
			drawn[declared.name][strconv.FormatInt(seed, 10)] = draw(t, g, seed)
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
	var differ []string
	for _, name := range keys(drawn, golden) {
		for _, seed := range keys(drawn[name], golden[name]) {
			if !equalJSON(golden[name][seed], drawn[name][seed]) {
				differ = append(differ, name+" seed "+seed)
			}
		}
	}
	if len(differ) == 0 {
		t.Fatalf("%s records every draw, but not as -update writes it. Rerun with -update.", goldenDraws)
	}
	t.Errorf("%s differs for %s. If the change is deliberate, rerun with -update, read its diff and say why in the commit.",
		goldenDraws, strings.Join(differ, ", "))
}

// keys are the keys of either map, sorted.
func keys[V any](a, b map[string]V) []string {
	union := slices.AppendSeq(slices.Collect(maps.Keys(a)), maps.Keys(b))
	slices.Sort(union)
	return slices.Compact(union)
}

func TestCertManagersFirstExampleSeedDrawsOneCreate(t *testing.T) {
	// The Makefile's negative control runs it alone, since one op costs no
	// replay to minimize.
	seed := makefileSeeds(t, "EXAMPLE")[0]
	ops := drawOps(t, file(certManagerTarget), seed)
	if len(ops) != 1 || ops[0].Type != run.OpCreate {
		t.Errorf("Seed %d draws %v, want a single create.", seed, opTypes(ops))
	}
}

func TestCertManagersExampleSeedsDrawWhatTheMakefileSays(t *testing.T) {
	seeds := makefileSeeds(t, "EXAMPLE")
	drawn := map[string]bool{}
	for _, seed := range seeds {
		for _, op := range drawOps(t, file(certManagerTarget), seed) {
			drawn[string(op.Type)] = true
			if op.Type == run.OpCreate && op.Obj.GetName() != "example" {
				drawn["a second Certificate"] = true
			}
		}
	}
	for _, want := range []string{"a second Certificate", string(run.OpRecreate), string(run.OpRestart)} {
		if !drawn[want] {
			t.Errorf("Seeds %v draw no %s, and the Makefile says they do.", seeds, want)
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
