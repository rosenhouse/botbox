package run

import (
	"math"
	"time"

	"github.com/rosenhouse/botbox/pkg/launch"
	"github.com/rosenhouse/botbox/pkg/target"
)

// stopBudget is what stopping a run takes: a grace period after SIGTERM,
// another after SIGKILL, and deleting the namespace.
const stopBudget = 2*launch.DefaultGracePeriod + teardownMargin

// Bound is the longest the Runner's waits can make the runs of the sequences
// take, one after another. A target that exits more than once per fault op
// while faults are active can outlast it.
func Bound(t *target.Target, sequences ...Sequence) time.Duration {
	var total time.Duration
	for _, s := range sequences {
		total = sum(total, bound(t.Timeouts, s))
	}
	return total
}

func bound(timeouts target.Timeouts, s Sequence) time.Duration {
	bound := defaultNamespaceDefaultsWithin
	faults, stops, stopTogether := 0, 0, 0
	for _, op := range s.Ops {
		switch op.Type {
		case OpDelete, OpRecreate:
			bound = sum(bound, timeouts.Delete)
		case OpRestart:
			bound = sum(bound, launch.DefaultGracePeriod)
		case OpFault:
			faults++
			if op.Fault.Until == (Trigger{}) {
				stopTogether = 1
			} else {
				stops++
			}
		}
		if op.Settles() {
			bound = sum(bound, timeouts.Settle)
		}
	}
	// A target that exits while a fault excuses it is owed T_settle past a
	// restart that can take MaxBackoff. Faults that stopped are owed as long
	// as they lasted and T_settle. Faults with no trigger stop together.
	exit := sum(launch.MaxBackoff, timeouts.Settle)
	for range faults {
		bound = sum(bound, exit)
	}
	for range stops + stopTogether {
		bound = sum(bound, bound, timeouts.Settle, exit)
	}
	return sum(bound, teardownBudget(timeouts), stopBudget)
}

// teardownBudget bounds the teardown's steps after any recovery wait.
func teardownBudget(timeouts target.Timeouts) time.Duration {
	return sum(timeouts.Stable, timeouts.Delete, teardownMargin)
}

// sum adds durations that are not negative, and saturates rather than
// overflow.
func sum(durations ...time.Duration) time.Duration {
	var total time.Duration
	for _, d := range durations {
		if d > math.MaxInt64-total {
			return math.MaxInt64
		}
		total += d
	}
	return total
}
