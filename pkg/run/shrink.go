package run

import (
	"context"
	"slices"
)

// Replay executes one candidate sequence from clean state, as Run does. An
// error is a configuration or harness error, never a finding.
type Replay func(ctx context.Context, candidate Sequence) (Result, error)

// Shrink removes ops one at a time, replays what is left and keeps the
// shorter sequence while it still fails for the same reason, until no single
// op can go (DESIGN.md §5.5). It stops at the context's deadline and returns
// the smallest failing sequence it found (§11).
//
// M6 adds the other half of §5.5, where a fault op shrinks toward no fault and
// shorter durations: those are further candidates for shrinkPass to try
// alongside the shorter sequences of without.
func Shrink(ctx context.Context, failing Sequence, violation Violation, replay Replay) Sequence {
	smallest := failing
	for removed := true; removed; {
		smallest, removed = shrinkPass(ctx, smallest, violation, replay)
	}
	return smallest
}

// shrinkPass removes every op it can, in order, and reports whether it
// removed any. Removing one op can free another, so Shrink passes again.
func shrinkPass(ctx context.Context, s Sequence, violation Violation, replay Replay) (Sequence, bool) {
	removed := false
	for i := 0; i < len(s.Ops) && ctx.Err() == nil; {
		candidate := s.without(i)
		// Removing an op can leave a sequence the format does not allow, such
		// as one ending on a restart. It is not a reproducer, and replaying it
		// would cost a run to learn so.
		if candidate.Validate() != nil || !reproduces(ctx, candidate, violation, replay) {
			i++
			continue
		}
		s, removed = candidate, true
	}
	return s, removed
}

// reproduces reports whether the candidate fails the same check the failing
// sequence did. A shorter sequence that fails another one is a different bug,
// not a smaller reproducer of this one. A candidate the Runner cannot execute,
// such as an update whose create is gone, reproduces nothing either.
//
// The statement decides alongside the ID, because one check states itself more
// than one way: G4 holds after a spec change and again after faults stop
// (DESIGN.md §6). The evidence does not, since it quotes op indexes that
// removing an op moves.
func reproduces(ctx context.Context, candidate Sequence, violation Violation, replay Replay) bool {
	result, err := replay(ctx, candidate)
	if err != nil || result.Violation == nil {
		return false
	}
	return result.Violation.ID == violation.ID && result.Violation.Statement == violation.Statement
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
