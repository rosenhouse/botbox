package run

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fakeReplay answers what a candidate sequence does, without a cluster.
type fakeReplay struct {
	// violates names the check a candidate trips, or nothing if it passes.
	violates func(Sequence) string
	// broken makes a candidate a harness error, as the Runner reports one it
	// cannot execute.
	broken func(Sequence) bool
	// after runs once the candidate has been replayed, which is where a test
	// expires the deadline.
	after func()
	seen  []Sequence
}

func (f *fakeReplay) replay(_ context.Context, candidate Sequence) (Result, error) {
	f.seen = append(f.seen, candidate)
	if f.after != nil {
		f.after()
	}
	if f.broken != nil && f.broken(candidate) {
		return Result{}, errors.New("op 0 (update): no CR has been created yet")
	}
	if id := f.violates(candidate); id != "" {
		return Result{Violation: &Violation{ID: id}}, nil
	}
	return Result{}, nil
}

// failsOn trips id whenever the candidate holds every one of needs.
func failsOn(id string, needs ...OpType) func(Sequence) string {
	return func(candidate Sequence) string {
		for _, needed := range needs {
			if !slices.Contains(opTypesOf(candidate), string(needed)) {
				return ""
			}
		}
		return id
	}
}

// sequenceOfTypes builds a sequence of those op types, each op carrying the
// fields its type needs, because Shrink is given sequences that validate.
func sequenceOfTypes(types ...OpType) Sequence {
	ops := make([]Op, len(types))
	for i, opType := range types {
		ops[i] = opOfType(opType)
	}
	return sequenceOf(ops...)
}

func opOfType(opType OpType) Op {
	op := Op{Type: opType}
	switch opType {
	case OpCreate, OpRecreate:
		op.Obj = &unstructured.Unstructured{Object: map[string]any{"kind": "Widget"}}
	case OpUpdate:
		op.Patch = map[string]any{"spec": map[string]any{"count": int64(1)}}
	case OpDeleteManaged:
		op.Kind, op.Nth = "v1/ConfigMap", nth(0)
	case OpFault:
		op.Fault = &Fault{Action: Action{Drop: true}, Until: Trigger{Count: 1}}
	}
	return op
}

// names writes op types as opTypesOf reads them.
func names(types ...OpType) []string {
	written := make([]string, len(types))
	for i, opType := range types {
		written[i] = string(opType)
	}
	return written
}

func shrink(t *testing.T, ctx context.Context, failing Sequence, id string, replay *fakeReplay) Sequence {
	t.Helper()
	return Shrink(ctx, failing, Violation{ID: id}, replay.replay)
}

func TestShrinkRemovesTheOpsTheFailureDoesNotNeed(t *testing.T) {
	failing := sequenceOfTypes(OpRestart, OpSettle, OpDelete, OpSettle)
	replay := &fakeReplay{violates: failsOn("G3", OpDelete)}

	shrunk := shrink(t, t.Context(), failing, "G3", replay)

	if want := names(OpDelete); !slices.Equal(opTypesOf(shrunk), want) {
		t.Errorf("Shrink returned %v, want %v: every other op is removable.", opTypesOf(shrunk), want)
	}
}

func TestShrinkKeepsAnOpTheFailureNeeds(t *testing.T) {
	failing := sequenceOfTypes(OpRestart, OpSettle, OpDelete)
	replay := &fakeReplay{violates: failsOn("G5", OpRestart, OpDelete)}

	shrunk := shrink(t, t.Context(), failing, "G5", replay)

	if want := names(OpRestart, OpDelete); !slices.Equal(opTypesOf(shrunk), want) {
		t.Errorf("Shrink returned %v, want %v: the failure needs both.", opTypesOf(shrunk), want)
	}
}

// A shorter sequence that fails a different check is not a smaller reproducer
// of the same bug (DESIGN.md §5.5).
func TestShrinkRejectsAShorterSequenceThatFailsDifferently(t *testing.T) {
	failing := sequenceOfTypes(OpRestart, OpSettle, OpDelete)
	replay := &fakeReplay{violates: func(candidate Sequence) string {
		if slices.Contains(opTypesOf(candidate), string(OpRestart)) {
			return "G5"
		}
		return "G3"
	}}

	shrunk := shrink(t, t.Context(), failing, "G5", replay)

	if want := names(OpRestart, OpDelete); !slices.Equal(opTypesOf(shrunk), want) {
		t.Errorf("Shrink returned %v, want %v: the sequence without the restart fails G3, not G5.",
			opTypesOf(shrunk), want)
	}
}

// Shrink's candidates are replayed and reported, so each one is a sequence of
// DESIGN.md §7. Removing an op can leave one that is not: a sequence ending on
// a restart waits for nothing.
func TestShrinkSkipsACandidateThatIsNotALegalSequence(t *testing.T) {
	failing := sequenceOfTypes(OpRestart, OpSettle)
	replay := &fakeReplay{violates: failsOn("G5", OpRestart)}

	shrunk := shrink(t, t.Context(), failing, "G5", replay)

	if want := names(OpRestart, OpSettle); !slices.Equal(opTypesOf(shrunk), want) {
		t.Errorf("Shrink returned %v, want %v: dropping the settle ends the sequence on the restart.",
			opTypesOf(shrunk), want)
	}
	for _, candidate := range replay.seen {
		if err := candidate.Validate(); err != nil {
			t.Errorf("Shrink replayed %v, which is not a legal sequence: %v", opTypesOf(candidate), err)
		}
	}
}

func TestShrinkRejectsACandidateTheRunnerCannotExecute(t *testing.T) {
	failing := sequenceOfTypes(OpCreate, OpDelete)
	replay := &fakeReplay{
		violates: failsOn("G3", OpDelete),
		broken:   func(candidate Sequence) bool { return !slices.Contains(opTypesOf(candidate), string(OpCreate)) },
	}

	shrunk := shrink(t, t.Context(), failing, "G3", replay)

	if want := names(OpCreate, OpDelete); !slices.Equal(opTypesOf(shrunk), want) {
		t.Errorf("Shrink returned %v, want %v: a delete with no create left is a harness error.",
			opTypesOf(shrunk), want)
	}
}

// The result is a legal sequence of DESIGN.md §7: its ops are numbered by
// position.
func TestShrinkRenumbersTheOpsItKeeps(t *testing.T) {
	failing := sequenceOfTypes(OpSettle, OpRestart, OpDelete)
	replay := &fakeReplay{violates: failsOn("G3", OpDelete)}

	shrunk := shrink(t, t.Context(), failing, "G3", replay)

	if err := shrunk.Validate(); err != nil {
		t.Errorf("Shrink returned a sequence that does not validate: %v", err)
	}
	if shrunk.Seed != failing.Seed || shrunk.Target != failing.Target {
		t.Errorf("Shrink returned the seed %d and the target %q, want %d and %q.",
			shrunk.Seed, shrunk.Target, failing.Seed, failing.Target)
	}
}

// Removing one op can free another, so the pass runs again over what it left.
func TestShrinkPassesAgainOverWhatItLeft(t *testing.T) {
	failing := sequenceOfTypes(OpRestart, OpSettle, OpDelete)
	// The first pass reaches the restart before the settle is gone, so only a
	// second pass removes it.
	reproducers := []string{"restart settle delete", "restart delete", "delete"}
	replay := &fakeReplay{violates: func(candidate Sequence) string {
		if slices.Contains(reproducers, strings.Join(opTypesOf(candidate), " ")) {
			return "G3"
		}
		return ""
	}}

	shrunk := shrink(t, t.Context(), failing, "G3", replay)

	if want := names(OpDelete); !slices.Equal(opTypesOf(shrunk), want) {
		t.Errorf("Shrink returned %v, want %v.", opTypesOf(shrunk), want)
	}
}

// A fault that cannot go entirely shrinks toward a shorter duration
// (DESIGN.md §5.5).
func TestShrinkWeakensAFaultItCannotRemove(t *testing.T) {
	failing := sequenceOfTypes(OpFault, OpSettle)
	failing.Ops[0].Fault = &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 8}}
	// The failure needs a fault, and one refused request is enough.
	replay := &fakeReplay{violates: func(candidate Sequence) string {
		for _, op := range candidate.Ops {
			if op.Type == OpFault {
				return "G1"
			}
		}
		return ""
	}}

	shrunk := shrink(t, t.Context(), failing, "G1", replay)

	if got := shrunk.Ops[0].Fault.Until.Count; got != 1 {
		t.Errorf("Shrink left the fault refusing %d requests, want the 1 the failure needs.", got)
	}
	if failing.Ops[0].Fault.Until.Count != 8 {
		t.Errorf("Shrink changed the sequence it was given: its fault now refuses %d.",
			failing.Ops[0].Fault.Until.Count)
	}
}

// Removal is tried before weakening, so a fault the failure does not need
// costs one replay and not one per halving. In a run each replay is a cluster
// (DESIGN.md §5.5).
func TestShrinkTriesRemovingAFaultBeforeWeakeningIt(t *testing.T) {
	failing := sequenceOfTypes(OpFault, OpDelete)
	failing.Ops[0].Fault = &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 8}}
	replay := &fakeReplay{violates: failsOn("G3", OpDelete)}

	shrink(t, t.Context(), failing, "G3", replay)

	if first := opTypesOf(replay.seen[0]); slices.Contains(first, string(OpFault)) {
		t.Errorf("Shrink replayed %v first, want the fault gone: no fault is simpler than a short one.", first)
	}
}

// No fault is simpler than a short one, so removal is tried first.
func TestShrinkRemovesAFaultTheFailureDoesNotNeed(t *testing.T) {
	failing := sequenceOfTypes(OpFault, OpDelete)
	failing.Ops[0].Fault = &Fault{Action: Action{Delay: Duration(time.Second)}, Until: Trigger{For: Duration(8 * time.Second)}}
	replay := &fakeReplay{violates: failsOn("G3", OpDelete)}

	shrunk := shrink(t, t.Context(), failing, "G3", replay)

	if want := names(OpDelete); !slices.Equal(opTypesOf(shrunk), want) {
		t.Errorf("Shrink returned %v, want %v: the failure needs no fault at all.", opTypesOf(shrunk), want)
	}
}

// A fault ends where it did: at the op it named, or at the first op left after
// it (DESIGN.md §7).
func TestWithoutMovesAFaultsEnd(t *testing.T) {
	until := 3
	failing := sequenceOfTypes(OpFault, OpSettle, OpRestart, OpDelete, OpSettle)
	failing.Ops[0].Fault = &Fault{Action: Action{Error: 500}, Until: Trigger{Op: &until}}
	for _, test := range []struct {
		name    string
		removed int
		want    int
	}{
		{name: "an op before the one it names", removed: 1, want: until - 1},
		{name: "the op it names", removed: until, want: until},
		{name: "an op after the one it names", removed: 4, want: until},
	} {
		t.Run(test.name, func(t *testing.T) {
			shorter := failing.without(test.removed)

			if got := *shorter.Ops[0].Fault.Until.Op; got != test.want {
				t.Errorf("The fault ends at op %d, want %d.", got, test.want)
			}
			if got := *failing.Ops[0].Fault.Until.Op; got != until {
				t.Errorf("without changed the sequence it was given: its fault ends at op %d.", got)
			}
		})
	}
}

// The deadline of DESIGN.md §11: the pass stops and reports the smallest
// failing sequence it found.
func TestShrinkStopsAtTheDeadline(t *testing.T) {
	failing := sequenceOfTypes(OpSettle, OpRestart, OpDelete, OpSettle)

	t.Run("part way through", func(t *testing.T) {
		ctx, expire := context.WithCancel(t.Context())
		replay := &fakeReplay{violates: failsOn("G3"), after: expire}

		shrunk := shrink(t, ctx, failing, "G3", replay)

		if len(replay.seen) != 1 {
			t.Errorf("The pass replayed %d candidates, want it to stop at the deadline.", len(replay.seen))
		}
		if len(shrunk.Ops) != len(failing.Ops)-1 {
			t.Errorf("Shrink returned %d ops, want the %d it had shrunk to.", len(shrunk.Ops), len(failing.Ops)-1)
		}
	})

	t.Run("before the first replay", func(t *testing.T) {
		ctx, expire := context.WithCancel(t.Context())
		expire()
		replay := &fakeReplay{violates: failsOn("G3")}

		shrunk := shrink(t, ctx, failing, "G3", replay)

		if len(replay.seen) != 0 {
			t.Errorf("The pass replayed %d candidates, want none: the deadline had passed.", len(replay.seen))
		}
		if !slices.Equal(opTypesOf(shrunk), opTypesOf(failing)) {
			t.Errorf("Shrink returned %v, want the failing sequence.", opTypesOf(shrunk))
		}
	})
}
