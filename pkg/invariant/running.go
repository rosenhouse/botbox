package invariant

import (
	"time"

	"github.com/rosenhouse/botbox/pkg/proxy"
)

// Back is when the target first made a request after since that shows it
// running, and whether it has. botbox has no other sign that a process it
// started is running.
func Back(requests []proxy.Request, since time.Time) (time.Time, bool) {
	var first time.Time
	for _, r := range requests {
		if r.Start.After(since) && showsRunning(r) && (first.IsZero() || r.Start.Before(first)) {
			first = r.Start
		}
	}
	return first, !first.IsZero()
}

// showsRunning reports whether a request shows the target past starting up. A
// process waiting to lead requests only leader election and paths that name no
// resource, such as discovery.
func showsRunning(r proxy.Request) bool { return r.Resource != "" && !leaderElection(r) }

// lastRestart names the target's last start before t, by a Restart op or by
// the supervisor after an exit, or is empty where it has not restarted.
func (in Input) lastRestart(t time.Time) (time.Time, string) {
	var start time.Time
	var named string
	for _, op := range in.Ops {
		if op.Type == OpRestart && op.Time.Before(t) {
			start, named = op.Time, describe(op)
		}
	}
	for _, exit := range in.Exits {
		if exit.Restart.Before(t) && exit.Restart.After(start) {
			start, named = exit.Restart, "the restart after its exit during "+describe(in.opBy(exit.At))
		}
	}
	return start, named
}
