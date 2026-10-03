package generate

import (
	"bytes"
	"cmp"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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

const (
	certManagerSequences     = "../../examples/cert-manager/sequences/*.json"
	certManagerHuntSequences = "../../examples/cert-manager/sequences/hunt/*.json"
)

func TestCertManagersPinnedSequencesReissueUnderRotationPolicyNever(t *testing.T) {
	// Draws never set Never, so the example tier runs it only from the
	// sequences it pins.
	for _, change := range certificateChanges(t, certManagerSequences) {
		respecs := change.op.Type == run.OpUpdate && !equalJSON(change.before["spec"], change.after["spec"])
		if respecs && privateKeyOf(change.before).rotationPolicy == "Never" && privateKeyOf(change.after).rotationPolicy == "Never" {
			return
		}
	}
	t.Errorf("No sequence in %s updates the spec of a Certificate that stays under rotationPolicy Never.", certManagerSequences)
}

func TestCertManagersSequencesKeepTheAlgorithmUnderRotationPolicyNever(t *testing.T) {
	// Under Never, cert-manager keeps a stored key that another algorithm
	// does not match, and waits for a user.
	for _, change := range certificateChanges(t, certManagerSequences, certManagerHuntSequences) {
		before, after := privateKeyOf(change.before), privateKeyOf(change.after)
		if after.rotationPolicy == "Never" && after.algorithm != before.algorithm {
			t.Errorf("%s op %d moves spec.privateKey.algorithm from %s to %s under rotationPolicy Never.",
				change.file, change.op.Index, before.algorithm, after.algorithm)
		}
	}
}

// privateKey is what a Certificate asks of its key, with cert-manager's
// defaults filled in.
type privateKey struct{ rotationPolicy, algorithm string }

func privateKeyOf(certificate map[string]any) privateKey {
	policy, _, _ := unstructured.NestedString(certificate, "spec", "privateKey", "rotationPolicy")
	algorithm, _, _ := unstructured.NestedString(certificate, "spec", "privateKey", "algorithm")
	return privateKey{cmp.Or(policy, "Always"), cmp.Or(algorithm, "RSA")}
}

// certificateChange is an update or recreate of a Certificate, and the
// Certificate before and after it.
type certificateChange struct {
	file          string
	op            run.Op
	before, after map[string]any
}

// certificateChanges replays the Certificates of the sequences that patterns
// match.
func certificateChanges(t *testing.T, patterns ...string) []certificateChange {
	t.Helper()
	sample := loadTarget(t, certManagerTarget).Sample.GetName()
	var changes []certificateChange
	for _, pattern := range patterns {
		files, err := filepath.Glob(pattern)
		if err != nil || len(files) == 0 {
			t.Fatalf("%s matches no sequence: %v", pattern, err)
		}
		for _, file := range files {
			sequence, err := run.ReadSequence(file)
			if err != nil {
				t.Fatal(err)
			}
			certificates := map[string]map[string]any{}
			for _, op := range sequence.Ops {
				name := cmp.Or(op.CR, sample)
				if op.Type == run.OpCreate {
					name = op.Obj.GetName()
				}
				before, existed := certificates[name]
				switch op.Type {
				case run.OpCreate, run.OpRecreate:
					certificates[name] = op.Obj.Object
				case run.OpUpdate:
					certificates[name] = run.MergePatch(runtime.DeepCopyJSON(before), op.Patch)
				case run.OpDelete:
					delete(certificates, name)
				}
				if existed && (op.Type == run.OpRecreate || op.Type == run.OpUpdate) {
					changes = append(changes, certificateChange{file, op, before, certificates[name]})
				}
			}
		}
	}
	return changes
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
		t.Errorf("Seed 19 draws %v, want an updateFixture under a settled Widget, then only settles, then a restart.", opTypes(ops))
	}
}

func TestOnlyARestartNextAfterTheLabelChangesUnderASettledWidgetRevealsB14(t *testing.T) {
	create, unsettledCreate := run.Op{Type: run.OpCreate}, run.Op{Type: run.OpCreate, NoSettle: true}
	label, settle, restart := run.Op{Type: run.OpUpdateFixture}, run.Op{Type: run.OpSettle}, run.Op{Type: run.OpRestart}
	for _, test := range []struct {
		name string
		ops  []run.Op
		want bool
	}{
		{"a settled create", []run.Op{create, label, settle, restart}, true},
		{"a create a settle follows", []run.Op{unsettledCreate, settle, label, restart}, true},
		{"a recreate", []run.Op{create, {Type: run.OpRecreate}, label, restart}, true},
		{"an update after the label", []run.Op{create, label, {Type: run.OpUpdate}, restart}, false},
		{"a deleteFixture after the label", []run.Op{create, label, {Type: run.OpDeleteFixture}, restart}, false},
		{"a delete", []run.Op{create, {Type: run.OpDelete}, label, restart}, false},
		{"no Widget", []run.Op{label, settle, restart}, false},
		{"a create that has not settled", []run.Op{unsettledCreate, label, settle, restart}, false},
		{"an update that has not settled", []run.Op{create, {Type: run.OpUpdate, NoSettle: true}, label, settle, restart}, false},
	} {
		if got := revealsB14(test.ops); got != test.want {
			t.Errorf("With %s, revealsB14(%v) = %t, want %t.", test.name, opTypes(test.ops), got, test.want)
		}
	}
}

// revealsB14 says whether the label changes once the Widget's last CR op has
// settled, and a restart is the next op to reconcile it.
func revealsB14(ops []run.Op) bool {
	var exists, unsettled bool
	for i, op := range ops {
		switch {
		case op.Type.OnCR():
			exists, unsettled = op.Type != run.OpDelete, op.NoSettle
		case op.Type == run.OpSettle:
			unsettled = false
		}
		next := slices.IndexFunc(ops[i+1:], func(op run.Op) bool { return op.Type != run.OpSettle })
		if op.Type == run.OpUpdateFixture && exists && !unsettled && next >= 0 && ops[i+1+next].Type == run.OpRestart {
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
