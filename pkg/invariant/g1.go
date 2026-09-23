package invariant

import (
	"fmt"

	"github.com/rosenhouse/botbox/pkg/proxy"
)

// leases are the objects a leader-electing target keeps reading and writing
// however quiet it is (DESIGN.md §6, G1).
const leaseGroup, leaseResource = "coordination.k8s.io", "leases"

// BoundedReconciliation is G1: once the settle wait has ended, the target
// makes no more API requests in T_stable than thresholds.quiet allows
// (DESIGN.md §6).
func BoundedReconciliation(in Input) (Result, error) {
	out := Result{ID: "G1"}
	allowed := in.quiet()
	for _, window := range in.quietWindows() {
		noisy := in.requestsIn(window, reconciles)
		if len(noisy) <= allowed {
			continue
		}
		out.violate(Violation{
			Statement: fmt.Sprintf("the target made %d API requests in %s, where thresholds.quiet allows %d%s",
				len(noisy), window, allowed, in.repeated(window.start, window.end)),
			At: noisy[allowed].Start,
		}.quotingRequests(Recent(noisy)))
	}
	return out, nil
}

// requestsIn returns the requests keep accepts that the target started inside
// the window.
func (in Input) requestsIn(window quiet, keep func(proxy.Request) bool) []proxy.Request {
	var inside []proxy.Request
	for _, r := range in.Requests {
		if r.Start.Before(window.start) || r.Start.After(window.end) || !keep(r) {
			continue
		}
		inside = append(inside, r)
	}
	return inside
}

// reconciles reports whether a request counts towards the rate G1 bounds. A
// watch is the target waiting, lease traffic is it holding leadership, and a
// request that names no resource is a health probe or a discovery read
// (DESIGN.md §6).
func reconciles(r proxy.Request) bool {
	if r.Watch || r.Verb == "watch" || r.Resource == "" {
		return false
	}
	return !(r.Group == leaseGroup && r.Resource == leaseResource)
}
