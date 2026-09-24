// Package generate draws the sequences a run executes (DESIGN.md §5.4). Values
// come from the primary CRD's OpenAPI v3 schema, tightened by the target's
// generate.mutate and generate.overlay. Generation starts from the target's
// sample, so a generated CR carries the fields a webhook demands and the schema
// does not describe (§8.3).
package generate

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"pgregory.net/rapid"

	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
)

// defaultMaxOps bounds a sequence that Options does not bound.
const defaultMaxOps = 6

// defaultMaxCRs bounds the CRs a sequence creates for a target that does not.
const defaultMaxCRs = 3

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
	maxCRs    int
	// distinct are the paths at which no two CRs hold one value.
	distinct  [][]string
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
	g := &Generator{target: t, rules: rules, fields: fields, leftAlone: leftAlone, maxOps: opts.MaxOps, maxCRs: t.Generate.MaxCRs}
	if g.maxOps < 1 {
		g.maxOps = defaultMaxOps
	}
	if g.maxCRs < 1 {
		g.maxCRs = defaultMaxCRs
	}
	for _, dotted := range t.Generate.Distinct {
		path := strings.Split(dotted, ".")
		if _, found, _ := unstructured.NestedString(t.Sample.Object, path...); !found {
			return nil, fmt.Errorf("generate.distinct %s: the sample holds no string there, and each CR after the first appends to it", dotted)
		}
		g.distinct = append(g.distinct, path)
	}
	for n := 1; n < g.maxCRs; n++ {
		if err := rules.refusal(g.base(n).Object, nil); err != nil {
			return nil, fmt.Errorf("the CRD refuses the CR %s: %w", g.name(n), err)
		}
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
	var at state
	create := run.Op{Index: 0, Type: run.OpCreate, Obj: g.cr(t, 0, &at), NoSettle: rapid.Bool().Draw(t, "noSettle")}
	ops := []run.Op{create}
	at.advance(create, 0)
	for range rapid.IntRange(0, g.maxOps-1).Draw(t, "ops") {
		ops = append(ops, g.op(t, len(ops), &at))
	}
	return run.Sequence{Target: g.target.Name, Ops: checkpointed(ops)}
}

// checkpointed inserts the settle waits that leave the drawn ops judged
// (DESIGN.md §6). A restart is wrapped in them: G5 compares the converged
// state either side of a restart, less what another op in between may have
// changed. The last op takes one because nothing else judges the state the run
// ends in. A noSettle elsewhere is left alone.
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

// cr is the n-th CR's base with a subset of the mutable fields changed. A
// change its CRD refuses, or that gives it another CR's distinct value, is
// undone, and the next is judged without it.
func (g *Generator) cr(t *rapid.T, n int, at *state) *unstructured.Unstructured {
	cr := g.base(n)
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
		if g.rules.refusal(changed.Object, nil) == nil && !g.collides(at, n, changed.Object) {
			cr = changed
		}
	}
	return cr
}

// base is the sample as the n-th CR, counting from 0. Each CR after the first
// appends -<n+1> to the sample's name and to its value at each distinct path.
func (g *Generator) base(n int) *unstructured.Unstructured {
	cr := g.target.Sample.DeepCopy()
	if n == 0 {
		return cr
	}
	suffix := fmt.Sprintf("-%d", n+1)
	cr.SetName(g.name(n))
	for _, path := range g.distinct {
		value, _, _ := unstructured.NestedString(cr.Object, path...)
		_ = unstructured.SetNestedField(cr.Object, value+suffix, path...)
	}
	return cr
}

// name is the n-th CR's name.
func (g *Generator) name(n int) string {
	if n == 0 {
		return g.target.Sample.GetName()
	}
	return fmt.Sprintf("%s-%d", g.target.Sample.GetName(), n+1)
}

// updateDraws bounds how often an update is drawn again where the CRD refuses
// it.
const updateDraws = 8

// patch is a merge patch that changes one field of the n-th CR, or nil if the
// CRD refused every one drawn or each gave the CR another's distinct value.
func (g *Generator) patch(t *rapid.T, n int, at *state) map[string]any {
	cr := at.crs[n].object
	for range updateDraws {
		mutable := rapid.SampledFrom(g.fields).Draw(t, "field")
		value, _ := mutable.draw(t)
		// A merge patch removes the field where the draw left it absent
		// (RFC 7386).
		patch := nest(mutable.path, value)
		patched := run.MergePatch(runtime.DeepCopyJSON(cr), patch)
		if g.rules.refusal(patched, cr) == nil && !g.collides(at, n, patched) {
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
	// crs are the CRs the sequence created, in order.
	crs []drawnCR
	// settled says the Runner has waited for the target's reaction since the
	// last change, so the objects the target manages are there to be deleted.
	settled bool
}

// drawnCR is a CR as botbox last wrote it, which it keeps once deleted.
type drawnCR struct {
	object map[string]any
	live   bool
}

// live are the positions of the CRs not deleted.
func (at *state) live() []int {
	var live []int
	for n, cr := range at.crs {
		if cr.live {
			live = append(live, n)
		}
	}
	return live
}

// collides reports whether the n-th CR, written as cr, would hold at a
// distinct path another CR's value, or the value another CR's base holds,
// which that CR falls back on.
func (g *Generator) collides(at *state, n int, cr map[string]any) bool {
	var others []map[string]any
	for other := range g.maxCRs {
		if other == n {
			continue
		}
		others = append(others, g.base(other).Object)
		if other < len(at.crs) {
			others = append(others, at.crs[other].object)
		}
	}
	for _, path := range g.distinct {
		value, _, _ := unstructured.NestedFieldNoCopy(cr, path...)
		for _, theirs := range others {
			held, _, _ := unstructured.NestedFieldNoCopy(theirs, path...)
			if reflect.DeepEqual(value, held) {
				return true
			}
		}
	}
	return false
}

// op draws the next op, and the CR it acts on.
func (g *Generator) op(t *rapid.T, index int, at *state) run.Op {
	op := run.Op{Index: index, Type: rapid.SampledFrom(g.legal(at)).Draw(t, "op")}
	n := 0
	switch op.Type {
	case run.OpCreate:
		n = len(at.crs)
		op.Obj = g.cr(t, n, at)
	case run.OpUpdate:
		n = rapid.SampledFrom(at.live()).Draw(t, "cr")
		if op.Patch = g.patch(t, n, at); op.Patch == nil {
			op.Type = run.OpSettle
		}
	case run.OpDelete:
		n = rapid.SampledFrom(at.live()).Draw(t, "cr")
	case run.OpRecreate:
		n = rapid.IntRange(0, len(at.crs)-1).Draw(t, "cr")
		op.Obj = g.cr(t, n, at)
	case run.OpDeleteManaged:
		op.Kind = rapid.SampledFrom(g.managed).Draw(t, "kind")
		// The first managed object of its kind: generation cannot know how many
		// the run will hold, and a later index would often resolve to nothing
		// and be skipped (DESIGN.md §7).
		op.Nth = new(int)
	}
	if op.Type.OnCR() {
		if op.Type != run.OpCreate && n > 0 {
			op.CR = g.name(n)
		}
		op.NoSettle = rapid.Bool().Draw(t, "noSettle")
	}
	at.advance(op, n)
	return op
}

// legal are the ops the state allows, simplest first, so that shrinking
// prefers the simplest.
func (g *Generator) legal(at *state) []run.OpType {
	legal := []run.OpType{run.OpSettle, run.OpRestart}
	live := len(at.live()) > 0
	if live && len(g.fields) > 0 {
		legal = append(legal, run.OpUpdate)
	}
	if live && at.settled && len(g.managed) > 0 {
		legal = append(legal, run.OpDeleteManaged)
	}
	legal = append(legal, run.OpRecreate)
	if live {
		legal = append(legal, run.OpDelete)
	}
	if len(at.crs) < g.maxCRs {
		legal = append(legal, run.OpCreate)
	}
	return legal
}

// advance follows the op, which acts on the n-th CR if it is a CR op.
func (at *state) advance(op run.Op, n int) {
	switch op.Type {
	case run.OpCreate:
		at.crs = append(at.crs, drawnCR{object: op.Obj.Object, live: true})
	case run.OpDelete:
		at.crs[n].live = false
	case run.OpRecreate:
		at.crs[n] = drawnCR{object: op.Obj.Object, live: true}
	case run.OpUpdate:
		at.crs[n].object = run.MergePatch(runtime.DeepCopyJSON(at.crs[n].object), op.Patch)
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
