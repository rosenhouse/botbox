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
	"pgregory.net/rapid"

	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
)

// defaultMaxOps bounds a sequence that Options does not bound.
const defaultMaxOps = 6

// Options tune generation.
type Options struct {
	// MaxOps bounds a sequence's length, counting the create it opens with.
	// Zero takes the default.
	MaxOps int
}

// Generator draws sequences for one target. New reads the target's CRDs; a
// draw does no I/O.
type Generator struct {
	target    *target.Target
	fields    []field
	managed   []string
	maxOps    int
	sequences *rapid.Generator[run.Sequence]
}

// New reads the target's primary CRD and the constraints on generating from
// it. Every error it returns is a configuration error (DESIGN.md §11).
func New(t *target.Target, opts Options) (*Generator, error) {
	primary, err := primarySchema(t)
	if err != nil {
		return nil, fmt.Errorf("generating for the target %s: %w", t.Name, err)
	}
	fields, err := mutableFields(t, primary)
	if err != nil {
		return nil, fmt.Errorf("generating for the target %s: %w", t.Name, err)
	}
	g := &Generator{target: t, fields: fields, maxOps: opts.MaxOps}
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
	g.sequences = rapid.Custom(g.Sequence)
	return g, nil
}

// Sequence draws one sequence, which starts by creating the primary CR
// (DESIGN.md §5.5). Its Seed is zero: Draw records the seed it was asked for,
// and a caller that drives rapid itself records the seed rapid runs under.
func (g *Generator) Sequence(t *rapid.T) run.Sequence {
	create := run.Op{Index: 0, Type: run.OpCreate, Obj: g.cr(t), NoSettle: rapid.Bool().Draw(t, "noSettle")}
	ops := []run.Op{create}
	at := state{crExists: true, settled: settles(create)}
	for range rapid.IntRange(0, g.maxOps-1).Draw(t, "ops") {
		ops = append(ops, g.op(t, len(ops), &at))
	}
	return run.Sequence{Target: g.target.Name, Ops: ops}
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

// cr is the target's sample with a subset of the mutable fields changed.
func (g *Generator) cr(t *rapid.T) *unstructured.Unstructured {
	cr := g.target.Sample.DeepCopy()
	for _, mutable := range g.fields {
		if !rapid.Bool().Draw(t, "mutate "+mutable.dotted) {
			continue
		}
		value, present := mutable.draw(t)
		if !present {
			unstructured.RemoveNestedField(cr.Object, mutable.path...)
			continue
		}
		if err := unstructured.SetNestedField(cr.Object, value, mutable.path...); err != nil {
			t.Fatalf("The sample does not take a %s: %v.", mutable.dotted, err)
		}
	}
	return cr
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
	crExists bool
	// settled says the Runner has waited for the target's reaction since the
	// last change, so the objects the target manages are there to be deleted.
	settled bool
}

// op draws the next op.
func (g *Generator) op(t *rapid.T, index int, at *state) run.Op {
	op := run.Op{Index: index, Type: rapid.SampledFrom(g.legal(at)).Draw(t, "op")}
	switch op.Type {
	case run.OpUpdate:
		mutable := rapid.SampledFrom(g.fields).Draw(t, "field")
		value, _ := mutable.draw(t)
		// A merge patch removes the field where the draw left it absent
		// (RFC 7386).
		op.Patch = nest(mutable.path, value)
	case run.OpRecreate:
		op.Obj = g.cr(t)
	case run.OpDeleteManaged:
		op.Kind = rapid.SampledFrom(g.managed).Draw(t, "kind")
		// The first managed object of its kind: generation cannot know how
		// many the run will hold, and an index that resolves to nothing is a
		// harness error (DESIGN.md §7).
		op.Nth = new(int)
	}
	if slices.Contains(crOps, op.Type) {
		op.NoSettle = rapid.Bool().Draw(t, "noSettle")
	}
	at.advance(op)
	return op
}

// legal are the ops the state allows, simplest first, so that shrinking
// prefers the simplest.
func (g *Generator) legal(at *state) []run.OpType {
	legal := []run.OpType{run.OpSettle, run.OpRestart}
	if at.crExists && len(g.fields) > 0 {
		legal = append(legal, run.OpUpdate)
	}
	if at.crExists && at.settled && len(g.managed) > 0 {
		legal = append(legal, run.OpDeleteManaged)
	}
	legal = append(legal, run.OpRecreate)
	if at.crExists {
		legal = append(legal, run.OpDelete)
	}
	return legal
}

func (at *state) advance(op run.Op) {
	switch op.Type {
	case run.OpDelete:
		at.crExists = false
	case run.OpRecreate:
		at.crExists = true
	}
	if settles(op) {
		at.settled = true
	} else if slices.Contains(mutatingOps, op.Type) {
		at.settled = false
	}
}

// crOps act on the primary CR and are the only ops that carry noSettle
// (DESIGN.md §4).
var crOps = []run.OpType{run.OpCreate, run.OpUpdate, run.OpDelete, run.OpRecreate}

// mutatingOps change the CR or a managed object, so the Runner settles after
// them (DESIGN.md §5.5).
var mutatingOps = append(slices.Clone(crOps), run.OpDeleteManaged)

// settles reports whether the Runner waits for the target's reaction after the
// op, as the Runner itself reads it (DESIGN.md §5.5).
func settles(op run.Op) bool {
	return op.Type == run.OpSettle || (!op.NoSettle && slices.Contains(mutatingOps, op.Type))
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
