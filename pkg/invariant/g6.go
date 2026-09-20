package invariant

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rosenhouse/botbox/pkg/proxy"
)

// NoErrorLoop is G6: the target does not make the same failing request, same
// verb, resource and name, more than N_errloop times within T_settle under a
// stable spec with no faults (DESIGN.md §6).
func NoErrorLoop(in Input) (Result, error) {
	threshold := in.errLoop()
	out := Result{ID: "G6"}
	for _, repeated := range in.repeatedFailures() {
		burst := densest(repeated.requests, in.timeouts().Settle)
		if len(burst) <= threshold {
			continue
		}
		out.violate(Violation{
			Statement: fmt.Sprintf("the target repeated the failing request %s %d times within %s, more than the %d §6 allows",
				repeated.key, len(burst), in.timeouts().Settle, threshold),
			At:       burst[0].Start,
			Requests: recent(burst),
		})
	}
	return out, nil
}

// failure is the failing requests of one verb, resource and name in one
// stretch of unchanged spec.
type failure struct {
	key      requestKey
	requests []proxy.Request
}

type requestKey struct{ verb, group, resource, name string }

func (k requestKey) String() string {
	resource := k.resource
	if k.group != "" {
		resource = k.group + "/" + resource
	}
	return fmt.Sprintf("%s %s/%s", k.verb, resource, k.name)
}

// repeatedFailures groups the failing requests by what the target asked for,
// within each stretch of unchanged spec, in a stable order.
func (in Input) repeatedFailures() []failure {
	grouped := map[time.Time]map[requestKey][]proxy.Request{}
	for _, r := range in.Requests {
		if r.Status < 400 || r.Fault != "" || in.faulted(r.Start, r.Start) {
			continue
		}
		key := requestKey{verb: r.Verb, group: r.Group, resource: r.Resource, name: r.Name}
		since := in.specSetAt(r.Start)
		if grouped[since] == nil {
			grouped[since] = map[requestKey][]proxy.Request{}
		}
		grouped[since][key] = append(grouped[since][key], r)
	}
	var failures []failure
	for _, keyed := range grouped {
		for key, requests := range keyed {
			slices.SortStableFunc(requests, func(a, b proxy.Request) int { return a.Start.Compare(b.Start) })
			failures = append(failures, failure{key: key, requests: requests})
		}
	}
	slices.SortFunc(failures, func(a, b failure) int {
		return cmp.Or(
			a.requests[0].Start.Compare(b.requests[0].Start),
			strings.Compare(a.key.String(), b.key.String()),
		)
	})
	return failures
}

// specSetAt is when the run last gave the target a new spec, which opens the
// stretch G6 counts within. Teardown closes the last one.
func (in Input) specSetAt(t time.Time) time.Time {
	var since time.Time
	for _, op := range in.Ops {
		if op.Type.touchesCR() && !op.Time.After(t) {
			since = op.Time
		}
	}
	if in.tornDown(t) && in.Teardown.After(since) {
		since = in.Teardown
	}
	return since
}

// densest returns the most repetitions of one request that fall within the
// window, which are in the order the proxy logged them.
func densest(requests []proxy.Request, window time.Duration) []proxy.Request {
	var burst []proxy.Request
	first := 0
	for last := range requests {
		for requests[last].Start.Sub(requests[first].Start) > window {
			first++
		}
		if last-first+1 > len(burst) {
			burst = requests[first : last+1]
		}
	}
	return burst
}
