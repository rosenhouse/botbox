package generate

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"

	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
)

func newGenerator(t *testing.T, loaded *target.Target, opts Options) *Generator {
	t.Helper()
	g, err := New(loaded, opts)
	if err != nil {
		t.Fatalf("New(%s) failed: %v.", loaded.Name, err)
	}
	return g
}

// mutablePaths are the paths each target lets the generator move, and the
// kinds it declares it manages (DESIGN.md §8.1).
var targets = []struct {
	path         string
	mutablePaths []string
	managed      []string
}{
	{toyTarget, []string{"spec.count"}, []string{"v1/ConfigMap"}},
	{certManagerTarget,
		[]string{"spec.dnsNames", "spec.duration", "spec.privateKey.algorithm", "spec.privateKey.rotationPolicy"},
		[]string{"v1/Secret", "cert-manager.io/v1/CertificateRequest"}},
	{externalSecretsTarget, []string{"spec.refreshInterval", "spec.target.name"}, []string{"v1/Secret"}},
}

func TestGeneratedCRsMatchTheirCRD(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			// The API server enforces the CRD; the overlay only tightens what
			// the generator draws, and the sample need not satisfy it.
			g := newGenerator(t, loadTarget(t, testCase.path), Options{})
			crd := crdSchemaOf(t, testCase.path)
			rapid.Check(t, func(rt *rapid.T) {
				var cr map[string]any
				for _, op := range g.sequence(rt).Ops {
					switch op.Type {
					case run.OpCreate, run.OpRecreate:
						cr = op.Obj.DeepCopy().Object
					case run.OpUpdate:
						cr = merge(cr, op.Patch)
					default:
						continue
					}
					if err := validate(crd, cr, ""); err != nil {
						rt.Fatalf("Op %d left the CR outside its schema: %v.", op.Index, err)
					}
				}
			})
		})
	}
}

// The gadget's CRD states rules its schema cannot. The test reads them here,
// apart from the API server's code that generation runs.
func gadgetRuleBroken(cr, old map[string]any) string {
	spec, _ := cr["spec"].(map[string]any)
	value := func(spec map[string]any, name string, absent int64) int64 {
		if set, isInteger := spec[name].(int64); isInteger {
			return set
		}
		return absent
	}
	_, left := spec["left"]
	_, right := spec["right"]
	switch count := value(spec, "count", 0); {
	case value(spec, "maxUnavailable", 0) > count:
		return "maxUnavailable exceeds count"
	case count < value(spec, "minCount", 1):
		return "count is below minCount's default"
	case left == right:
		return "left and right are both set or both unset"
	}
	previous, _ := old["spec"].(map[string]any)
	switch {
	case old == nil:
	case previous["mode"] != spec["mode"]:
		return "mode changed"
	case value(spec, "minCount", 1) > value(previous, "minCount", 1):
		return "minCount rose"
	}
	return ""
}

func TestGeneratedGadgetsKeepTheirCRDsRules(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	g := newGenerator(t, loaded, Options{})
	crd := crdSchemaOf(t, rulesTarget)
	var changed, switchedMode, updated int
	rapid.Check(t, func(rt *rapid.T) {
		var current map[string]any
		for _, op := range g.sequence(rt).Ops {
			var next, old map[string]any
			switch op.Type {
			case run.OpCreate, run.OpRecreate:
				next = op.Obj.DeepCopy().Object
				if !equalJSON(next, loaded.Sample.Object) {
					changed++
				}
				if mode, _, _ := unstructured.NestedString(next, "spec", "mode"); mode != "fast" {
					switchedMode++
				}
			case run.OpUpdate:
				next, old = merge(current, op.Patch), current
				updated++
			case run.OpDelete:
				current = nil
				continue
			default:
				continue
			}
			if err := validate(crd, next, ""); err != nil {
				rt.Fatalf("Op %d (%s) left the gadget outside its schema: %v.", op.Index, op.Type, err)
			}
			if broken := gadgetRuleBroken(next, old); broken != "" {
				rt.Fatalf("Op %d (%s) left a gadget whose %s: %v.", op.Index, op.Type, broken, next["spec"])
			}
			current = next
		}
	})
	if changed == 0 || updated == 0 {
		t.Errorf("%d creates changed the sample and %d updates were drawn; a generator that changes nothing keeps every rule.",
			changed, updated)
	}
	if switchedMode == 0 {
		t.Error("No create switched the sample's mode, which only an update may not change.")
	}
}

func TestARefusedUpdateIsDrawnAgain(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	loaded.Generate.Mutate = []string{"spec.left", "spec.maxUnavailable"}
	// Every maxUnavailable exceeds the sample's count, so the CRD refuses it.
	loaded.Generate.Overlay = map[string]map[string]any{"spec.maxUnavailable": {"minimum": 4}}
	g := newGenerator(t, loaded, Options{})
	updates := 0
	for seed := int64(1); seed <= 200; seed++ {
		sequence, err := g.Draw(seed)
		if err != nil {
			t.Fatalf("Draw(%d) failed: %v.", seed, err)
		}
		for _, op := range sequence.Ops {
			if op.Type == run.OpUpdate {
				updates++
			}
		}
	}
	// A left that changes passes a quarter of the draws. Drawn again, most
	// update ops find one.
	if updates < 50 {
		t.Errorf("200 seeds drew %d updates; a refused update is not drawn again.", updates)
	}
}

func TestAnUpdateTheCRDAlwaysRefusesBecomesASettle(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	loaded.Generate.Mutate = []string{"spec.count"}
	// The only count the overlay allows is below minCount's default.
	loaded.Generate.Overlay = map[string]map[string]any{"spec.count": {"maximum": 0}}
	g := newGenerator(t, loaded, Options{})
	rapid.Check(t, func(rt *rapid.T) {
		sequence := g.sequence(rt)
		if err := sequence.Validate(); err != nil {
			rt.Fatalf("The Runner rejects the sequence: %v.", err)
		}
		for _, op := range sequence.Ops {
			if op.Type == run.OpUpdate {
				rt.Fatalf("Op %d updates the gadget with %v, which its CRD refuses.", op.Index, op.Patch)
			}
		}
	})
}

func TestTheStateFollowsTheCRBotboxLastWrote(t *testing.T) {
	spec := func(count int64) map[string]any {
		return map[string]any{"spec": map[string]any{"count": count, "mode": "fast"}}
	}
	at := state{cr: spec(3)}

	at.advance(run.Op{Type: run.OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": int64(5)}}})
	if !equalJSON(at.cr, spec(5)) {
		t.Errorf("After an update the state holds %v, want %v.", at.cr, spec(5))
	}
	at.advance(run.Op{Type: run.OpRecreate, Obj: &unstructured.Unstructured{Object: spec(7)}})
	if !equalJSON(at.cr, spec(7)) {
		t.Errorf("After a recreate the state holds %v, want %v.", at.cr, spec(7))
	}
	at.advance(run.Op{Type: run.OpDelete})
	if at.cr != nil {
		t.Errorf("After a delete the state holds %v, want no CR.", at.cr)
	}
}

func TestSequencesAreLegalToReplay(t *testing.T) {
	const maxOps = 5
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			g := newGenerator(t, loaded, Options{MaxOps: maxOps})
			rapid.Check(t, func(rt *rapid.T) {
				sequence := g.sequence(rt)
				if err := sequence.Validate(); err != nil {
					rt.Fatalf("The Runner rejects the sequence: %v.", err)
				}
				if sequence.Target != loaded.Name {
					rt.Fatalf("The sequence names the target %q, want %q.", sequence.Target, loaded.Name)
				}
				// checkpointed adds at most two settles per drawn op.
				if len(sequence.Ops) > 3*maxOps {
					rt.Fatalf("The sequence holds %d ops, over the %d drawn and the settles each may need.",
						len(sequence.Ops), maxOps)
				}
				if sequence.Ops[0].Type != run.OpCreate {
					rt.Fatalf("The sequence opens with a %s, want the create of the CR.", sequence.Ops[0].Type)
				}
				crExists := true
				for i, op := range sequence.Ops[1:] {
					needsCR := []run.OpType{run.OpUpdate, run.OpDelete, run.OpDeleteManaged}
					switch {
					case op.Type == run.OpCreate:
						rt.Fatalf("Op %d creates the CR a second time.", op.Index)
					case !crExists && slices.Contains(needsCR, op.Type):
						rt.Fatalf("Op %d is a %s while the CR is deleted.", op.Index, op.Type)
					}
					if op.Type == run.OpDeleteManaged {
						if !slices.Contains(testCase.managed, op.Kind) {
							rt.Fatalf("Op %d deletes a %s, which the target does not declare it manages.",
								op.Index, op.Kind)
						}
						if *op.Nth != 0 {
							rt.Fatalf("Op %d deletes the managed object %d, which generation cannot know exists.",
								op.Index, *op.Nth)
						}
						if previous := sequence.Ops[i]; previous.NoSettle {
							rt.Fatalf("Op %d deletes a managed object after op %d skipped its settle wait.",
								op.Index, previous.Index)
						}
					}
					switch op.Type {
					case run.OpDelete:
						crExists = false
					case run.OpRecreate:
						crExists = true
					}
				}
			})
		})
	}
}

// An invariant is evaluated where a settle wait ends (DESIGN.md §6), so an op
// that nothing waits on is a draw spent on a run nothing judges.
func TestEveryDrawnOpIsFollowedByTheSettleThatJudgesIt(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			g := newGenerator(t, loadTarget(t, testCase.path), Options{MaxOps: 5})
			rapid.Check(t, func(rt *rapid.T) {
				ops := g.sequence(rt).Ops
				if last := ops[len(ops)-1]; !last.Settles() {
					rt.Fatalf("The sequence ends with a %s, so only the teardown judges it (§6).", last.Type)
				}
				for i, op := range ops {
					if op.Type != run.OpRestart {
						continue
					}
					// G5 judges a restart only if no other op changed the run
					// between the converged states either side of it.
					if before := ops[i-1]; !before.Settles() {
						rt.Fatalf("Op %d restarts the target after a %s that waits for nothing, so G5 has nothing to judge.",
							i, before.Type)
					}
					if after := ops[i+1]; after.Type != run.OpSettle {
						rt.Fatalf("Op %d restarts the target and op %d is a %s, so G5 has nothing to judge.",
							i, i+1, after.Type)
					}
				}
			})
		})
	}
}

func TestSequencesRoundTripThroughTheirFile(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			g := newGenerator(t, loadTarget(t, testCase.path), Options{})
			file := filepath.Join(t.TempDir(), "sequence.json")
			rapid.Check(t, func(rt *rapid.T) {
				written := write(rt, file, g.sequence(rt))
				read, err := run.ReadSequence(file)
				if err != nil {
					rt.Fatalf("ReadSequence failed on %s: %v.", written, err)
				}
				if rewritten := write(rt, file, read); !bytes.Equal(written, rewritten) {
					rt.Fatalf("The sequence changed as it went through its file:\n%s\n%s", written, rewritten)
				}
			})
		})
	}
}

func write(rt *rapid.T, file string, sequence run.Sequence) []byte {
	if err := run.WriteSequence(file, sequence); err != nil {
		rt.Fatalf("WriteSequence failed: %v.", err)
	}
	written, err := os.ReadFile(file)
	if err != nil {
		rt.Fatalf("Reading back the sequence file failed: %v.", err)
	}
	return written
}

func TestOneSeedDrawsOneSequence(t *testing.T) {
	const seeds = 20
	g := newGenerator(t, loadTarget(t, certManagerTarget), Options{})
	distinct := map[string]bool{}
	for seed := int64(1); seed <= seeds; seed++ {
		first, second := draw(t, g, seed), draw(t, g, seed)
		if !bytes.Equal(first, second) {
			t.Fatalf("Seed %d drew two different sequences:\n%s\n%s", seed, first, second)
		}
		distinct[string(first)] = true
	}
	if len(distinct) < seeds/2 {
		t.Errorf("%d seeds drew %d distinct sequences; the seed chooses the sequence.", seeds, len(distinct))
	}
}

func draw(t *testing.T, g *Generator, seed int64) []byte {
	t.Helper()
	sequence, err := g.Draw(seed)
	if err != nil {
		t.Fatalf("Draw(%d) failed: %v.", seed, err)
	}
	if sequence.Seed != seed {
		t.Fatalf("The sequence drawn at seed %d carries the seed %d.", seed, sequence.Seed)
	}
	if err := sequence.Validate(); err != nil {
		t.Fatalf("The Runner rejects the sequence drawn at seed %d: %v.", seed, err)
	}
	marshalled, err := sequence.Marshal()
	if err != nil {
		t.Fatalf("Marshalling the sequence drawn at seed %d failed: %v.", seed, err)
	}
	return marshalled
}

func TestGeneratedCountsStayInsideTheCRDsBounds(t *testing.T) {
	loaded := loadTarget(t, toyTarget)
	g := newGenerator(t, loaded, Options{})
	rapid.Check(t, func(rt *rapid.T) {
		for _, count := range generatedAt(g.sequence(rt), loaded.Sample, "spec", "count") {
			whole, isInteger := count.(int64)
			if !isInteger {
				rt.Fatalf("spec.count is %#v, want the int64 the API server's decoder writes.", count)
			}
			if whole < 0 || whole > 10 {
				rt.Fatalf("spec.count is %d, outside the CRD's 0 to 10.", whole)
			}
		}
	})
}

func TestGeneratedValuesObeyTheOverlay(t *testing.T) {
	// The CRD takes any string as a duration and any list of DNS names; only
	// the overlay says which (DESIGN.md §8.3).
	loaded := loadTarget(t, certManagerTarget)
	g := newGenerator(t, loaded, Options{})
	allowed := []any{"1h", "24h", "2160h"}
	rapid.Check(t, func(rt *rapid.T) {
		sequence := g.sequence(rt)
		for _, duration := range generatedAt(sequence, loaded.Sample, "spec", "duration") {
			if !slices.Contains(allowed, duration) {
				rt.Fatalf("spec.duration is %#v, which the overlay's enum %v does not allow.", duration, allowed)
			}
		}
		for _, names := range generatedAt(sequence, loaded.Sample, "spec", "dnsNames") {
			if length := len(names.([]any)); length < 1 || length > 3 {
				rt.Fatalf("spec.dnsNames holds %d names, outside the overlay's 1 to 3.", length)
			}
		}
	})
}

func TestTheOverlayBoundsTheListLength(t *testing.T) {
	const exactly = 2
	loaded := loadTarget(t, certManagerTarget)
	loaded.Generate.Overlay["spec.dnsNames"] = map[string]any{"minItems": exactly, "maxItems": exactly}
	g := newGenerator(t, loaded, Options{})
	rapid.Check(t, func(rt *rapid.T) {
		for _, names := range generatedAt(g.sequence(rt), loaded.Sample, "spec", "dnsNames") {
			if length := len(names.([]any)); length != exactly {
				rt.Fatalf("spec.dnsNames holds %d names, and the overlay allows %d.", length, exactly)
			}
		}
	})
}

func TestOnlyTheMutablePathsMove(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			g := newGenerator(t, loaded, Options{})
			rapid.Check(t, func(rt *rapid.T) {
				for _, op := range g.sequence(rt).Ops {
					if op.Obj == nil {
						continue
					}
					for _, path := range differences(loaded.Sample.Object, op.Obj.Object, "") {
						if !slices.Contains(testCase.mutablePaths, path) {
							rt.Fatalf("Op %d moved %s, and the target lets the generator move %v.",
								op.Index, path, testCase.mutablePaths)
						}
					}
				}
			})
		})
	}
}

// generatedAt collects what a sequence generated at a path, in the CRs it
// creates and the patches it applies. What a CR kept from the sample is not
// the generator's doing.
func generatedAt(sequence run.Sequence, sample *unstructured.Unstructured, path ...string) []any {
	kept, _, _ := unstructured.NestedFieldNoCopy(sample.Object, path...)
	var values []any
	for _, op := range sequence.Ops {
		var object map[string]any
		switch op.Type {
		case run.OpCreate, run.OpRecreate:
			object = op.Obj.Object
		case run.OpUpdate:
			object = op.Patch
		default:
			continue
		}
		value, found, err := unstructured.NestedFieldNoCopy(object, path...)
		if found && err == nil && value != nil && !equalJSON(value, kept) {
			values = append(values, value)
		}
	}
	return values
}

// differences are the dotted paths where two objects disagree.
func differences(a, b map[string]any, prefix string) []string {
	var paths []string
	for _, name := range union(a, b) {
		left, inA := a[name]
		right, inB := b[name]
		leftObject, aIsObject := left.(map[string]any)
		rightObject, bIsObject := right.(map[string]any)
		switch {
		case (aIsObject || bIsObject) && (aIsObject || !inA) && (bIsObject || !inB):
			paths = append(paths, differences(leftObject, rightObject, join(prefix, name))...)
		case inA != inB || !equalJSON(left, right):
			paths = append(paths, join(prefix, name))
		}
	}
	return paths
}

func union(a, b map[string]any) []string {
	names := slices.Collect(maps.Keys(a))
	for name := range b {
		if _, inA := a[name]; !inA {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// merge applies a JSON merge patch (RFC 7386), as the Runner does.
func merge(object, patch map[string]any) map[string]any {
	merged := map[string]any{}
	maps.Copy(merged, object)
	for name, value := range patch {
		nested, isObject := value.(map[string]any)
		switch {
		case value == nil:
			delete(merged, name)
		case isObject:
			into, _ := merged[name].(map[string]any)
			merged[name] = merge(into, nested)
		default:
			merged[name] = value
		}
	}
	return merged
}

func TestATargetWithNothingToMutateStillDrawsSequences(t *testing.T) {
	// A target may declare no manages, and a CRD may describe no path the
	// generator can write. Neither leaves an op with nothing to choose from.
	g := newGenerator(t, loadTarget(t, toyTarget), Options{})
	g.fields, g.managed = nil, nil
	rapid.Check(t, func(rt *rapid.T) {
		for _, op := range g.sequence(rt).Ops {
			if op.Type == run.OpUpdate || op.Type == run.OpDeleteManaged {
				rt.Fatalf("Op %d is a %s, and the target offers it nothing to name.", op.Index, op.Type)
			}
		}
	})
}
