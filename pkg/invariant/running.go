package invariant

import (
	"slices"
	"time"

	"github.com/rosenhouse/botbox/pkg/proxy"
)

// Back is when the target first made a request after since that shows it
// running, and whether it has. botbox has no other sign that a process it
// started is running. A process starting up can request leader election and
// paths that name no resource, such as discovery. A process that elects a
// leader can start its informers before it leads, so only a lease it won
// shows it running.
func Back(requests []proxy.Request, since time.Time) (time.Time, bool) {
	shows := func(r proxy.Request) bool { return r.Resource != "" && !leaderElection(r) }
	if electing(requests, since) {
		shows = won
	}
	var first time.Time
	for _, r := range requests {
		if r.Start.After(since) && shows(r) && (first.IsZero() || r.Start.Before(first)) {
			first = r.Start
		}
	}
	return first, !first.IsZero()
}

// notBack says what a target not back since since had not done.
func notBack(requests []proxy.Request, since time.Time) string {
	if electing(requests, since) {
		return "won no lease"
	}
	return "requested no resource outside leader election"
}

// electing reports whether the target got a lease after since, as leader
// election does to learn who holds it. An informer lists and watches Leases
// instead.
func electing(requests []proxy.Request, since time.Time) bool {
	return slices.ContainsFunc(requests, func(r proxy.Request) bool {
		return r.Start.After(since) && isLease(r) && r.Verb == "get"
	})
}

// won reports whether a request won the target a lease: the API server
// accepted its create or update of one.
func won(r proxy.Request) bool {
	return isLease(r) && (r.Verb == "create" || r.Verb == "update") && r.Status/100 == 2
}

func isLease(r proxy.Request) bool { return leaderElection(r) && r.Resource == "leases" }

// settledBy is when a target botbox restarted at restart must have converged:
// T_settle past its return where it returned within T_settle, or else T_settle
// past the restart.
func (in Input) settledBy(restart time.Time) time.Time {
	settle := in.timeouts().Settle
	if back, found := Back(in.Requests, restart); found && back.Before(restart.Add(settle)) {
		return back.Add(settle)
	}
	return restart.Add(settle)
}

// restartOwed is settledBy of the last Restart op before t, or zero.
func (in Input) restartOwed(t time.Time) time.Time {
	var owed time.Time
	for _, op := range in.Ops {
		if op.Type == OpRestart && op.Time.Before(t) {
			owed = in.settledBy(op.Time)
		}
	}
	return owed
}

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
