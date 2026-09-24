package run

import (
	"time"

	"github.com/rosenhouse/botbox/pkg/launch"
	"github.com/rosenhouse/botbox/pkg/target"
)

// stopBudget is what stopping a run takes: a grace period after SIGTERM,
// another after SIGKILL, and deleting the namespace.
const stopBudget = 2*launch.DefaultGracePeriod + teardownMargin

// Bound is the longest the Runner's waits can make one run of the sequence
// take. A target that keeps exiting while a fault is active can outlast it.
func Bound(t *target.Target, s Sequence) time.Duration {
	timeouts := t.Timeouts
	bound := defaultNamespaceDefaultsWithin
	faults := 0
	for _, op := range s.Ops {
		switch {
		case op.Type == OpRecreate, op.Type == OpDelete && op.Settles():
			bound += timeouts.Delete
		case op.Type == OpRestart:
			bound += launch.DefaultGracePeriod
		case op.Type == OpFault:
			faults++
		}
		if op.Settles() {
			bound += timeouts.Settle
		}
	}
	// A target is owed as long as the faults lasted and T_settle, and an exit
	// they excused T_settle past a restart that can take MaxBackoff.
	for range faults {
		bound = 2*bound + 2*timeouts.Settle + launch.MaxBackoff
	}
	return bound + teardownBudget(timeouts) + stopBudget
}

// teardownBudget bounds the teardown's steps after any recovery wait.
func teardownBudget(timeouts target.Timeouts) time.Duration {
	return timeouts.Stable + timeouts.Delete + teardownMargin
}
