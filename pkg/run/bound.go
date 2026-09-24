package run

import (
	"math"
	"time"

	"github.com/rosenhouse/botbox/pkg/launch"
	"github.com/rosenhouse/botbox/pkg/target"
)

// stopBudget is what stopping a run takes: a grace period after SIGTERM,
// another after SIGKILL, and deleting the namespace.
const stopBudget = 2*launch.DefaultGracePeriod + namespaceDeletionBudget

// Bound is the longest the Runner's waits can make the runs of the sequences
// take, one after another. A target that exits more than once per fault op
// while faults are active can outlast it. A bound too long for a Duration is
// the longest Duration.
func Bound(t *target.Target, sequences ...Sequence) time.Duration {
	// Faults double the bound, so it is added up in float64, which does not
	// overflow.
	var total float64
	for _, s := range sequences {
		total += bound(t.Timeouts, s)
	}
	if total >= math.MaxInt64 {
		return math.MaxInt64
	}
	return time.Duration(total)
}

func bound(timeouts target.Timeouts, s Sequence) float64 {
	settle, deletion := float64(timeouts.Settle), float64(timeouts.Delete)
	bound := float64(defaultNamespaceDefaultsWithin)
	faults, stops, stopTogether := 0, 0, 0
	for _, op := range s.Ops {
		switch op.Type {
		case OpDelete, OpRecreate:
			bound += deletion
		case OpRestart:
			bound += float64(launch.DefaultGracePeriod)
		case OpFault:
			faults++
			if op.Fault.Until == (Trigger{}) {
				stopTogether = 1
			} else {
				stops++
			}
		}
		if op.Settles() {
			bound += settle
		}
	}
	// A target that exits while a fault excuses it is owed T_settle past a
	// restart that can take MaxBackoff. Faults that stopped are owed as long
	// as they lasted and T_settle. Faults with no trigger stop together.
	exit := float64(launch.MaxBackoff) + settle
	bound += float64(faults) * exit
	for range stops + stopTogether {
		bound = 2*bound + settle + exit
	}
	teardown := float64(timeouts.Stable) + deletion + float64(teardownMargin)
	return bound + teardown + float64(stopBudget)
}
