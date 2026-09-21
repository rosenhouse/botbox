package run

import (
	"context"
	"slices"
	"time"
)

// Replay executes one candidate sequence from clean state, as Run does. An
// error is a configuration or harness error, never a finding.
type Replay func(ctx context.Context, candidate Sequence) (Result, error)

// Shrink simplifies one op at a time, replays what is left and keeps the
// simpler sequence while it still fails for the same reason, until no op can
// give any more (DESIGN.md §5.5). It stops at the context's deadline and
// returns the smallest failing sequence it found (§11).
func Shrink(ctx context.Context, failing Sequence, violation Violation, replay Replay) Sequence {
	smallest := failing
	for removed := true; removed; {
		smallest, removed = shrinkPass(ctx, smallest, violation, replay)
	}
	return smallest
}

// shrinkPass simplifies every op it can, in order, and reports whether it
// simplified any. Simplifying one op can free another, so Shrink passes again.
func shrinkPass(ctx context.Context, s Sequence, violation Violation, replay Replay) (Sequence, bool) {
	simplified := false
	for i := 0; i < len(s.Ops) && ctx.Err() == nil; {
		// Removal first, because no fault is simpler than a short one
		// (DESIGN.md §5.5). On success the next op has shifted into this
		// position, so try it again.
		if candidate, ok := reproducing(ctx, s, i, []Sequence{s.without(i)}, violation, replay); ok {
			s, simplified = candidate, true
			continue
		}
		// The op stays. Weaken it as far as it goes, and do not ask about
		// removing it again: the sequence without it is the same whatever
		// its fault carries, and each replay is a cluster (§11).
		for ctx.Err() == nil {
			candidate, ok := reproducing(ctx, s, i, milderThan(s, i), violation, replay)
			if !ok {
				break
			}
			s, simplified = candidate, true
		}
		i++
	}
	return s, simplified
}

// milderThan are the sequences with op i's fault weakened.
func milderThan(s Sequence, i int) []Sequence {
	var candidates []Sequence
	for _, milder := range weakened(s.Ops[i]) {
		candidates = append(candidates, s.with(i, milder))
	}
	return candidates
}

// reproducing returns the first candidate that still fails the same way.
func reproducing(ctx context.Context, s Sequence, i int, candidates []Sequence,
	violation Violation, replay Replay) (Sequence, bool) {
	for _, candidate := range candidates {
		// Simplifying an op can leave a sequence the format does not allow,
		// such as one ending on a restart. It is not a reproducer, and
		// replaying it would cost a run to learn so. A candidate that changed
		// nothing is not one either, and accepting it would spin the pass.
		if candidate.Validate() != nil || len(candidate.Ops) == len(s.Ops) && candidate.Ops[i].same(s.Ops[i]) {
			continue
		}
		if reproduces(ctx, candidate, violation, replay) {
			return candidate, true
		}
		if ctx.Err() != nil {
			break
		}
	}
	return s, false
}

// minFaultDuration is where halving a fault's duration stops. Halving runs to
// a nanosecond in thirty-two steps, and each step is a whole cluster (§11),
// for a reproducer nobody can read: a fault delaying a request by a
// nanosecond is indistinguishable from no fault, which removal already tries.
const minFaultDuration = Duration(10 * time.Millisecond)

// weakened are the milder faults to try in the op's place, each halving one
// thing the fault carries (DESIGN.md §5.5). A count halves to 1, because a
// zero Trigger never ends and is a stronger fault, not a milder one. A
// duration halves to minFaultDuration.
func weakened(op Op) []Op {
	if op.Type != OpFault || op.Fault == nil {
		return nil
	}
	var milder []Op
	if half := op.Fault.Until.Count / 2; half > 0 {
		milder = append(milder, op.withFault(func(f *Fault) { f.Until.Count = half }))
	}
	if half, ok := halved(op.Fault.Until.For); ok {
		milder = append(milder, op.withFault(func(f *Fault) { f.Until.For = half }))
	}
	if half, ok := halved(op.Fault.Action.Delay); ok {
		milder = append(milder, op.withFault(func(f *Fault) { f.Action.Delay = half }))
	}
	return milder
}

// halved is the duration to try next, and whether there is one. A duration
// already at the floor has nothing milder to offer.
func halved(d Duration) (Duration, bool) {
	if d <= minFaultDuration {
		return d, false
	}
	if half := d / 2; half > minFaultDuration {
		return half, true
	}
	return minFaultDuration, true
}

// withFault copies the op with its fault changed, so that a candidate never
// shares a fault with the sequence it came from.
func (o Op) withFault(change func(*Fault)) Op {
	fault := *o.Fault
	change(&fault)
	o.Fault = &fault
	return o
}

// same reports whether the ops carry the same fault, which is what weakening
// changes. It is the guard against a candidate that simplifies nothing.
func (o Op) same(other Op) bool {
	if o.Fault == nil || other.Fault == nil {
		return o.Fault == other.Fault
	}
	return *o.Fault == *other.Fault
}

// with returns the sequence with op i replaced.
func (s Sequence) with(i int, op Op) Sequence {
	replaced := s
	replaced.Ops = slices.Clone(s.Ops)
	replaced.Ops[i] = op
	return replaced
}

// reproduces reports whether the candidate fails the same check the failing
// sequence did. A shorter sequence that fails another one is a different bug,
// not a smaller reproducer of this one. A candidate the Runner cannot execute,
// such as an update whose create is gone, reproduces nothing either.
//
// The ID is all that can decide it. A check states itself with the op it
// anchors to, and G1 and G2 with a request count as well, so comparing the
// statement would reject every candidate that removes an op before the failing
// one, which is most of them.
func reproduces(ctx context.Context, candidate Sequence, violation Violation, replay Replay) bool {
	result, err := replay(ctx, candidate)
	return err == nil && result.Violation != nil && result.Violation.ID == violation.ID
}

// without returns the sequence with op i removed, renumbered so that it is
// legal (DESIGN.md §7). A fault still ends where it did: at the op it named,
// or at the first op left after it.
func (s Sequence) without(i int) Sequence {
	shorter := s
	shorter.Ops = slices.Delete(slices.Clone(s.Ops), i, i+1)
	for j := range shorter.Ops {
		shorter.Ops[j].Index = j
		if until := shorter.Ops[j].endsAfter(i); until != nil {
			fault := *shorter.Ops[j].Fault
			fault.Until.Op = until
			shorter.Ops[j].Fault = &fault
		}
	}
	return shorter
}

// endsAfter returns where the op's fault ends once op i is gone, or nil if
// removing i does not move it.
func (o Op) endsAfter(i int) *int {
	if o.Fault == nil || o.Fault.Until.Op == nil || *o.Fault.Until.Op <= i {
		return nil
	}
	moved := *o.Fault.Until.Op - 1
	return &moved
}
