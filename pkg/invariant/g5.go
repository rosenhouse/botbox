package invariant

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rosenhouse/botbox/pkg/observe"
)

// RestartStable is G5: restarting the target does not change converged state
// (DESIGN.md §6). It compares the last converged snapshot before each Restart
// with the first converged one after it, keyed by kind and name. A Restart
// missing either snapshot, or with a change of botbox's between them, is not
// evaluated, and the result says so.
func RestartStable(in Input) (Result, error) {
	out := Result{ID: "G5"}
	for _, op := range in.Ops {
		if op.Type != OpRestart {
			continue
		}
		before, after, missing := in.convergedAround(op)
		if missing != "" {
			out.note("for %s: it has no converged snapshot %s it", describe(op), missing)
			continue
		}
		if changed, found := in.changedBetween(before.at, after.at); found {
			out.note("for %s: %s ran between the converged states before and after it, so G5 cannot tell what the restart changed; a settle op on each side of a restart lets G5 judge it",
				describe(op), describe(changed))
			continue
		}
		out.compare(in, op, before, after)
	}
	return out, nil
}

// changedBetween returns the first op that changed the CR or a managed object
// strictly between from and to.
func (in Input) changedBetween(from, to time.Time) (Op, bool) {
	for _, op := range in.Ops {
		if op.changesRun() && op.Time.After(from) && op.Time.Before(to) {
			return op, true
		}
	}
	return Op{}, false
}

// convergedAround returns the states the settle waits converged at on either
// side of the op, or which side is missing (DESIGN.md §6, G5 evaluation).
func (in Input) convergedAround(op Op) (before, after state, missing string) {
	var last, first *Checkpoint
	for i, checkpoint := range in.Checkpoints {
		switch {
		case checkpoint.Settle != Converged:
		case checkpoint.Time.Before(op.Time):
			last = &in.Checkpoints[i]
		case first == nil && checkpoint.Time.After(op.Time):
			first = &in.Checkpoints[i]
		}
	}
	switch {
	case last == nil:
		return before, after, "before"
	case first == nil:
		return before, after, "after"
	}
	states := in.statesAt([]time.Time{last.Time, first.Time})
	return states[0], states[1], ""
}

// compare reports every object that the Restart added, dropped or changed.
func (out *Result) compare(in Input, op Op, before, after state) {
	equal := in.equality(before, after)
	indexed := func(s state) map[objectKey]observe.Version {
		objects := map[objectKey]observe.Version{}
		for _, v := range s.live {
			objects[objectKey{kind: kindName(v.GVK), name: v.Name}] = v
		}
		return objects
	}
	was, is := indexed(before), indexed(after)
	for _, key := range union(was, is) {
		old, existed := was[key]
		current, exists := is[key]
		switch {
		case existed && !exists:
			out.differs(in, op, old, "is gone after")
		case !existed && exists:
			out.differs(in, op, current, "appeared only after")
		case !equal(old, current):
			out.differs(in, op, current, "changed across")
		}
	}
}

type objectKey struct{ kind, name string }

func (out *Result) differs(in Input, op Op, v observe.Version, how string) {
	out.violate(Violation{
		Statement: fmt.Sprintf("the %s %s %s the Restart at %s", kindName(v.GVK), v.Name, how, describe(op)),
		At:        op.Time,
	}.quotingVersions(RecentHistory(v.Key, in.History.History(v.Key))))
}

func union(was, is map[objectKey]observe.Version) []objectKey {
	keys := make([]objectKey, 0, len(was)+len(is))
	for key := range was {
		keys = append(keys, key)
	}
	for key := range is {
		if _, both := was[key]; !both {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b objectKey) int {
		return cmp.Or(strings.Compare(a.kind, b.kind), strings.Compare(a.name, b.name))
	})
	return keys
}
