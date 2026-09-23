package invariant

import (
	"fmt"

	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/proxy"
)

// NoChurn is G2: once converged under a stable spec, the primary CR, the set
// of managed objects and their resourceVersions do not change for T_stable
// (DESIGN.md §6). A status write whose content is unchanged moves no
// resourceVersion, so the proxy log supplies that half, which
// thresholds.quiet bounds.
func NoChurn(in Input) (Result, error) {
	out := Result{ID: "G2"}
	allowed := in.quiet()
	for _, window := range in.quietWindows() {
		if moved := in.changesIn(window); len(moved) > 0 {
			out.violate(Violation{
				Statement: fmt.Sprintf("the target changed %d objects in %s, where §6 requires none",
					len(moved), window),
				At: moved[0].Time,
			}.quotingVersions(Recent(moved)))
		}
		if written := in.requestsIn(window, writesStatus); len(written) > allowed {
			out.violate(Violation{
				Statement: fmt.Sprintf("the target made %d status writes in %s, where thresholds.quiet allows %d",
					len(written), window, allowed),
				At: written[allowed].Start,
			}.quotingRequests(Recent(written)))
		}
	}
	return out, nil
}

// changesIn returns the versions of the primary CR and of managed objects the
// Observer recorded inside the window.
func (in Input) changesIn(window quiet) []observe.Version {
	var moved []observe.Version
	for _, v := range in.versions() {
		if v.Time.Before(window.start) || v.Time.After(window.end) {
			continue
		}
		if in.attributes(v) {
			moved = append(moved, v)
		}
	}
	return moved
}

// attributes reports whether the version is the target's work: the primary CR
// or a managed object (DESIGN.md §6).
func (in Input) attributes(v observe.Version) bool {
	return v.GVK == in.Target.Primary || in.History.IsManaged(v.Key)
}

func writesStatus(r proxy.Request) bool { return r.Subresource == "status" && writes(r.Verb) }

func writes(verb string) bool {
	switch verb {
	case "create", "update", "patch", "delete", "deletecollection":
		return true
	}
	return false
}
