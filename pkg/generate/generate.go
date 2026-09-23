// Package generate draws the sequences a run executes (DESIGN.md §5.4). Values
// come from the primary CRD's OpenAPI v3 schema, tightened by the target's
// generate.mutate and generate.overlay. Generation starts from the target's
// sample, so a generated CR carries the fields a webhook demands and the schema
// does not describe (§8.3).
package generate

import (
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"pgregory.net/rapid"

	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
)

// defaultMaxOps bounds a sequence that Options does not bound.
const defaultMaxOps = 6

// Options tune generation.
type Options struct {
	// MaxOps bounds the ops a draw makes, counting the create it opens with.
	// Generation adds the settle waits that leave those ops judged (DESIGN.md
	// §6), at most two per drawn op, so a sequence holds at most 3*MaxOps.
	// Zero takes the default.
	MaxOps int
}

// Generator draws sequences for one target. New reads the target's CRDs; a
// draw does no I/O.
type Generator struct {
	target    *target.Target
	rules     *crdRules
	fields    []field
	leftAlone []string
	managed   []string
	maxOps    int
	sequences *rapid.Generator[run.Sequence]
}

// New reads the target's primary CRD and the constraints on generating from
// it. Every error it returns is a configuration error (DESIGN.md §11).
func New(t *target.Target, opts Options) (*Generator, error) {
	g, err := build(t, opts)
	if err != nil {
		return nil, fmt.Errorf("generating for the target %s: %w", t.Name, err)
	}
	return g, nil
}

func build(t *target.Target, opts Options) (*Generator, error) {
	crd, err := openAPISchema(t)
	if err != nil {
		return nil, err
	}
	// The API server judges a CR against the CRD as declared, so the rules are
	// read before the overlay changes it.
	rules, err := newCRDRules(crd)
	if err != nil {
		return nil, err
	}
	if err := rules.refusal(t.Sample.Object, nil); err != nil {
		return nil, fmt.Errorf("the CRD refuses the sample: %w", err)
	}
	primary, err := overlaid(crd, t.Generate.Overlay)
	if err != nil {
		return nil, err
	}
	fields, leftAlone, err := mutableFields(t, primary)
	if err != nil {
		return nil, err
	}
	fields, leftAlone, err = drawable(t, rules, fields, leftAlone)
	if err != nil {
		return nil, err
	}
	g := &Generator{target: t, rules: rules, fields: fields, leftAlone: leftAlone, maxOps: opts.MaxOps}
	if g.maxOps < 1 {
		g.maxOps = defaultMaxOps
	}
	for _, gvk := range t.Manages {
		name := gvk.Version + "/" + gvk.Kind
		if gvk.Group != "" {
			name = gvk.Group + "/" + name
		}
		g.managed = append(g.managed, name)
	}
	g.sequences = rapid.Custom(g.sequence)
	return g, nil
}

// drawable keeps the fields the CRD accepts a drawn value of in the sample. A
// refused field that generate.mutate names is a configuration error, and any
// other is left alone.
func drawable(t *target.Target, rules *crdRules, fields []field, leftAlone []string) ([]field, []string, error) {
	var kept []field
	for _, mutable := range fields {
		refused := rules.acceptsADraw(t.Sample, mutable)
		switch {
		case refused == nil:
			kept = append(kept, mutable)
		case len(t.Generate.Mutate) > 0:
			return nil, nil, fmt.Errorf("generate.mutate %s: %w", mutable.dotted, refused)
		default:
			leftAlone = append(leftAlone, leftAloneNote(mutable.dotted, refused))
		}
	}
	slices.Sort(leftAlone)
	return kept, leftAlone, nil
}

// LeftAlone says which spec paths generation never changes, and why. Only a
// target without generate.mutate has any.
func (g *Generator) LeftAlone() []string { return g.leftAlone }

// sequence draws one sequence, which starts by creating the primary CR
// (DESIGN.md §5.5). It leaves Seed zero; Draw, the only way out of this
// package, records the seed it was asked for.
func (g *Generator) sequence(t *rapid.T) run.Sequence {
	create := run.Op{Index: 0, Type: run.OpCreate, Obj: g.cr(t), NoSettle: rapid.Bool().Draw(t, "noSettle")}
	ops := []run.Op{create}
	at := state{cr: create.Obj.Object, settled: create.Settles()}
	for range rapid.IntRange(0, g.maxOps-1).Draw(t, "ops") {
		ops = append(ops, g.op(t, len(ops), &at))
	}
	return run.Sequence{Target: g.target.Name, Ops: checkpointed(ops)}
}

// checkpointed inserts the settle waits that leave the drawn ops judged
// (DESIGN.md §6). A restart is wrapped in them: G5 compares the converged
// state either side of a restart, and judges nothing if another op changed the
// run in between. The last op takes one because nothing else judges the state
// the run ends in. A noSettle elsewhere is left alone.
func checkpointed(ops []run.Op) []run.Op {
	judged := make([]run.Op, 0, 3*len(ops))
	settled := false
	emit := func(op run.Op) {
		op.Index = len(judged)
		judged = append(judged, op)
		settled = op.Settles()
	}
	for i, op := range ops {
		if op.Type == run.OpRestart && !settled {
			emit(run.Op{Type: run.OpSettle})
		}
		emit(op)
		var next run.Op
		last := i == len(ops)-1
		if !last {
			next = ops[i+1]
		}
		switch {
		case op.Type == run.OpRestart && next.Type != run.OpSettle:
			emit(run.Op{Type: run.OpSettle})
		case last && !settled:
			emit(run.Op{Type: run.OpSettle})
		}
	}
	return judged
}

// Draw returns the sequence the seed produces, recording the seed as the
// sequence's own. One seed always draws the same sequence, so a replay needs
// no rapid (DESIGN.md §5.4).
func (g *Generator) Draw(seed int64) (sequence run.Sequence, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("drawing a sequence for the target %s at seed %d: %v", g.target.Name, seed, recovered)
		}
	}()
	sequence = g.sequences.Example(int(seed))
	sequence.Seed = seed
	return sequence, nil
}

// cr is the target's sample with a subset of the mutable fields changed. A
// change its CRD refuses is undone, and the next is judged without it.
func (g *Generator) cr(t *rapid.T) *unstructured.Unstructured {
	cr := g.target.Sample.DeepCopy()
	for _, mutable := range g.fields {
		if !rapid.Bool().Draw(t, "mutate "+mutable.dotted) {
			continue
		}
		value, present := mutable.draw(t)
		changed := cr.DeepCopy()
		if !present {
			unstructured.RemoveNestedField(changed.Object, mutable.path...)
		} else if err := unstructured.SetNestedField(changed.Object, value, mutable.path...); err != nil {
			t.Fatalf("The sample does not take a %s: %v.", mutable.dotted, err)
		}
		if g.rules.refusal(changed.Object, nil) == nil {
			cr = changed
		}
	}
	return cr
}

// updateDraws bounds how often an update is drawn again where the CRD refuses
// it.
const updateDraws = 8

// patch is a merge patch that changes one field of the CR, or nil if the CRD
// refused every one drawn.
func (g *Generator) patch(t *rapid.T, cr map[string]any) map[string]any {
	for range updateDraws {
		mutable := rapid.SampledFrom(g.fields).Draw(t, "field")
		value, _ := mutable.draw(t)
		// A merge patch removes the field where the draw left it absent
		// (RFC 7386).
		patch := nest(mutable.path, value)
		if g.rules.refusal(run.MergePatch(runtime.DeepCopyJSON(cr), patch), cr) == nil {
			return patch
		}
	}
	return nil
}

// draw is one value for the field, or its removal where the schema allows the
// field to be absent (DESIGN.md §5.4).
func (f field) draw(t *rapid.T) (any, bool) {
	if f.optional && rapid.Bool().Draw(t, "remove "+f.dotted) {
		return nil, false
	}
	return f.values.Draw(t, f.dotted), true
}

// state is what the sequence so far leaves the run in, which says what op may
// come next.
type state struct {
	// cr is the primary CR as botbox last wrote it, or nil once deleted.
	cr map[string]any
	// settled says the Runner has waited for the target's reaction since the
	// last change, so the objects the target manages are there to be deleted.
	settled bool
}

// op draws the next op.
func (g *Generator) op(t *rapid.T, index int, at *state) run.Op {
	op := run.Op{Index: index, Type: rapid.SampledFrom(g.legal(at)).Draw(t, "op")}
	switch op.Type {
	case run.OpUpdate:
		if op.Patch = g.patch(t, at.cr); op.Patch == nil {
			op.Type = run.OpSettle
		}
	case run.OpRecreate:
		op.Obj = g.cr(t)
	case run.OpDeleteManaged:
		op.Kind = rapid.SampledFrom(g.managed).Draw(t, "kind")
		// The first managed object of its kind: generation cannot know how many
		// the run will hold, and a later index would often resolve to nothing
		// and be skipped (DESIGN.md §7).
		op.Nth = new(int)
	}
	if op.Type.OnCR() {
		op.NoSettle = rapid.Bool().Draw(t, "noSettle")
	}
	at.advance(op)
	return op
}

// legal are the ops the state allows, simplest first, so that shrinking
// prefers the simplest.
func (g *Generator) legal(at *state) []run.OpType {
	legal := []run.OpType{run.OpSettle, run.OpRestart}
	if at.cr != nil && len(g.fields) > 0 {
		legal = append(legal, run.OpUpdate)
	}
	if at.cr != nil && at.settled && len(g.managed) > 0 {
		legal = append(legal, run.OpDeleteManaged)
	}
	legal = append(legal, run.OpRecreate)
	if at.cr != nil {
		legal = append(legal, run.OpDelete)
	}
	return legal
}

func (at *state) advance(op run.Op) {
	switch op.Type {
	case run.OpDelete:
		at.cr = nil
	case run.OpRecreate:
		at.cr = op.Obj.Object
	case run.OpUpdate:
		at.cr = run.MergePatch(runtime.DeepCopyJSON(at.cr), op.Patch)
	}
	// The only op that changes the managed objects without waiting for the
	// target's reaction is a CR op that skips its settle.
	if op.Settles() {
		at.settled = true
	} else if op.Type.OnCR() {
		at.settled = false
	}
}

// nest wraps a value in the objects its path names, which is the merge patch
// that changes that one field (DESIGN.md §7).
func nest(path []string, value any) map[string]any {
	nested := value
	for i := len(path) - 1; i >= 0; i-- {
		nested = map[string]any{path[i]: nested}
	}
	return nested.(map[string]any)
}
