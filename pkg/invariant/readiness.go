package invariant

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/proxy"
	"github.com/rosenhouse/botbox/pkg/target"
)

// ExpiredWait is the G4 of a settle wait that expired, with why it did. The
// Runner raises it where the wait ends, and the engine at every checkpoint
// after, so the two cannot disagree. Its error is a configuration error.
func (in Input) ExpiredWait(checkpoint Checkpoint) (Violation, error) {
	began, at := checkpoint.Began, checkpoint.Time
	walk, err := in.walkReady(began, at)
	if err != nil {
		return Violation{}, err
	}
	stable := in.timeouts().Stable
	changes := in.versionsIn(at.Add(-stable), at)
	seen := in.stateAt(at)
	violation := Violation{
		ID: "G4",
		Statement: fmt.Sprintf("the settle wait after %s expired with no fault active: in %s, %s%s",
			in.describeOp(checkpoint.Op), at.Sub(began).Round(time.Millisecond), walk.why(began, stable, changes),
			in.repeated(began, at)),
		At: at,
	}.quotingRequests(Recent(requestsUpTo(in.Requests, at))).
		quotingManaged(Sample(seen.managed(in)))
	cr, found := seen.cr(in.Target.Primary)
	switch {
	case (walk.held || walk.crs == 0) && len(changes) > 0:
		violation = violation.quotingVersions(Recent(changes))
	case found:
		violation = violation.quotingVersions(RecentHistory(cr.Key, upTo(in.History.History(cr.Key), at)))
	}
	if found {
		violation.Ready = in.readiness(cr, walk.err)
	}
	return violation, nil
}

// readyWalk is what Ready did over a settle wait, read as the wait read it:
// it holds where every live primary CR satisfies it.
type readyWalk struct {
	// crs is how many primary CRs were live at the end.
	crs  int
	held bool
	ever bool
	// turned is when Ready last began or stopped holding.
	turned time.Time
	// err is the last evaluation's.
	err error
}

// walkReady evaluates Ready on the CR as the wait found it and on every
// version the Observer recorded after, up to the end.
func (in Input) walkReady(began, end time.Time) (readyWalk, error) {
	var walk readyWalk
	versions := slices.DeleteFunc(in.versionsIn(time.Time{}, end), func(v observe.Version) bool {
		return v.GVK != in.Target.Primary
	})
	latest := map[observe.Key]observe.Version{}
	next := 0
	for ; next < len(versions) && !versions[next].Time.After(began); next++ {
		latest[versions[next].Key] = versions[next]
	}
	if err := walk.step(in, began, live(latest)); err != nil {
		return walk, err
	}
	for _, v := range versions[next:] {
		latest[v.Key] = v
		if err := walk.step(in, v.Time, live(latest)); err != nil {
			return walk, err
		}
	}
	return walk, nil
}

// step evaluates Ready at one instant, stopping at the first CR it fails on,
// as the wait does.
func (w *readyWalk) step(in Input, at time.Time, crs []observe.Version) error {
	held, err := len(crs) > 0, error(nil)
	for _, cr := range crs {
		held, err = in.Target.Ready(cr.Object)
		if errors.Is(err, target.ErrNotBool) {
			return err
		}
		if err != nil || !held {
			held = false
			break
		}
	}
	if held != w.held {
		w.turned = at
	}
	w.crs, w.held, w.err = len(crs), held, err
	w.ever = w.ever || held
	return nil
}

// why says what kept the wait from converging.
func (w readyWalk) why(began time.Time, stable time.Duration, changes []observe.Version) string {
	switch {
	case w.crs == 0:
		return "no CR was left to be ready, " + churn(stable, changes)
	case w.held:
		return fmt.Sprintf("ready held from %s on, %s", w.turned.Sub(began).Round(time.Millisecond), churn(stable, changes))
	case w.ever:
		return fmt.Sprintf("ready held until %s: %s", w.turned.Sub(began).Round(time.Millisecond), w.failure())
	default:
		return "ready never held: " + w.failure()
	}
}

func (w readyWalk) failure() string {
	if w.err != nil {
		return w.err.Error()
	}
	return "it evaluated to false"
}

// churn says what changed in the last stable, which a wait needs quiet.
func churn(stable time.Duration, changes []observe.Version) string {
	if len(changes) == 0 {
		return fmt.Sprintf("and nothing changed in the last stable (%s)", stable)
	}
	last := changes[len(changes)-1]
	return fmt.Sprintf("but the namespace never held still for stable (%s): %s in the last %s, the last to %s %s",
		stable, count(len(changes), "change"), stable, kindName(last.GVK), last.Name)
}

// readiness is what a verdict quotes of the predicate on the CR.
func (in Input) readiness(cr observe.Version, err error) *Readiness {
	ready := &Readiness{Expr: in.Target.ReadyExpr}
	if err != nil {
		ready.Error = err.Error()
	}
	ready.Status, _ = cr.Object.Object["status"].(map[string]any)
	return ready
}

// repeated names the failing request the target made most often in [from,
// to], where it made one more than once. An error loop under backoff can be
// too sparse for G6 and still be why the window failed.
func (in Input) repeated(from, to time.Time) string {
	counts := map[requestKey]int{}
	var worst requestKey
	var last proxy.Request
	for _, r := range in.Requests {
		if r.Start.Before(from) || r.Start.After(to) || !failed(r) {
			continue
		}
		key := keyOf(r)
		counts[key]++
		if key == worst || counts[key] > counts[worst] {
			worst, last = key, r
		}
	}
	if counts[worst] < 2 {
		return ""
	}
	return fmt.Sprintf("; the target repeated the failing request %s %d times, the last answered %d", worst, counts[worst], last.Status)
}

func requestsUpTo(requests []proxy.Request, t time.Time) []proxy.Request {
	var before []proxy.Request
	for _, r := range requests {
		if !r.Start.After(t) {
			before = append(before, r)
		}
	}
	return before
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
