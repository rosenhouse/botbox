package invariant_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rosenhouse/botbox/pkg/invariant"
	"github.com/rosenhouse/botbox/pkg/target"
)

// noSuchKey is the ready of a target that misspelled a field.
func noSuchKey(*unstructured.Unstructured) (bool, error) {
	return false, &target.EvalError{Predicate: "ready", Expr: "status.readyy == spec.count", Err: errors.New("no such key: readyy")}
}

// yieldsAnInt is the ready of a target that left out the comparison.
func yieldsAnInt(*unstructured.Unstructured) (bool, error) {
	return false, &target.EvalError{Predicate: "ready", Expr: "status.ready", Err: fmt.Errorf("%w: it yielded int64", target.ErrNotBool)}
}

// unreadyCreate is a create whose CR the Observer recorded at 1s in the status
// given. G4 judges it at 5s.
func unreadyCreate(ready, observed int64) *run {
	return newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(3), status(ready, observed)))
}

// expiredSettle is a settle op whose wait ran from 1s to 6s on a CR that never
// became ready. No spec changed, so G4 judges the expired wait alone.
func expiredSettle() *run {
	return newRun().
		record(500*time.Millisecond, widget("10", spec(3), status(0, 1))).
		op(invariant.OpSettle, time.Second).
		checkpoint(6*time.Second, invariant.Expired)
}

// expiredWait returns the G4 of the expired settle wait, beside which G4 may
// also judge the CR at a deadline.
func expiredWait(t *testing.T, in invariant.Input) invariant.Violation {
	t.Helper()
	result := evaluate(t, invariant.Convergence, in)
	for _, violation := range result.Violations {
		if strings.HasPrefix(violation.Statement, "the settle wait") {
			return violation
		}
	}
	t.Fatalf("G4 reported %v, want the expired settle wait.", statements(result))
	return invariant.Violation{}
}

func requireStatement(t *testing.T, violation invariant.Violation, want string) {
	t.Helper()
	if !strings.Contains(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to say %q.", violation.Statement, want)
	}
}

func TestAnExpiredWaitSaysReadyNeverHeld(t *testing.T) {
	in := unreadyCreate(0, 1).checkpoint(5*time.Second, invariant.Expired).through(8 * time.Second)

	violation := expiredWait(t, in)

	requireStatement(t, violation, "the settle wait after op 0 (create) expired with no fault active: in 5s, ready never held: it evaluated to false")
}

func TestAnExpiredWaitQuotesWhyReadyCouldNotBeEvaluated(t *testing.T) {
	r := unreadyCreate(0, 1)
	r.in.Target.Ready = noSuchKey
	in := r.checkpoint(5*time.Second, invariant.Expired).through(8 * time.Second)

	violation := expiredWait(t, in)

	requireStatement(t, violation, `in 5s, ready never held: evaluating ready "status.readyy == spec.count": no such key: readyy`)
}

func TestAnExpiredWaitSaysWhenReadyStoppedHolding(t *testing.T) {
	in := unreadyCreate(3, 1).
		record(2500*time.Millisecond, widget("11", spec(3), status(2, 1))).
		checkpoint(5*time.Second, invariant.Expired).
		through(8 * time.Second)

	violation := expiredWait(t, in)

	requireStatement(t, violation, "in 5s, ready held until 2.5s: it evaluated to false")
}

// A target that holds Ready while it keeps writing never gives the wait its
// quiet, and the changes are what the timeline quotes.
func TestAnExpiredWaitSaysWhatKeptTheNamespaceFromHoldingStill(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(500*time.Millisecond, widget("10", spec(1), status(1, 1))).
		record(time.Second, child("w-0", "11")).
		record(3500*time.Millisecond, widget("12", spec(1), status(1, 1))).
		record(4*time.Second, child("w-0", "13")).
		record(4500*time.Millisecond, child("w-0", "14")).
		checkpoint(5*time.Second, invariant.Expired).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	requireStatement(t, violation, "in 5s, ready held from 500ms on, but the namespace never held still for stable (2s): "+
		"3 changes in the last 2s, the last to v1/ConfigMap w-0")
	if got := quoted(violation); strings.Join(got, ",") != "w@12,w-0@13,w-0@14" {
		t.Errorf("The timeline holds %v, want the 3 changes that broke the quiet.", got)
	}
	if violation.VersionsOf != "" {
		t.Errorf("The timeline says it is of %q, and it quotes two objects.", violation.VersionsOf)
	}
}

// Ready holds on the CR as it was when the wait began, which the Observer
// recorded before then.
func TestAnExpiredWaitReadsTheCRAsTheWaitFoundIt(t *testing.T) {
	in := newRun().
		record(time.Second, widget("10", spec(1), status(1, 1))).
		op(invariant.OpSettle, 10*time.Second).
		record(14*time.Second, child("w-0", "11")).
		checkpoint(15*time.Second, invariant.Expired).
		through(18 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	requireStatement(t, violation, "in 5s, ready held from 0s on, but the namespace never held still for stable (2s): 1 change")
}

// A wait can begin before the Observer sees the op's write, and the CR it
// found then is the one the op replaced.
func TestAnExpiredWaitJudgesTheCRFromTheWriteOn(t *testing.T) {
	for name, r := range map[string]*run{
		"an update": newRun().
			record(time.Second, widget("10", spec(3), status(3, 1))).
			op(invariant.OpUpdate, 2*time.Second).
			record(2001*time.Millisecond, widget("11", spec(4), generation(2), status(3, 1))),
		"an update, the CR recorded as it was applied": newRun().
			record(2*time.Second, widget("10", spec(3), status(3, 1))).
			op(invariant.OpUpdate, 2*time.Second).
			record(2001*time.Millisecond, widget("11", spec(4), generation(2), status(3, 1))),
	} {
		t.Run(name, func(t *testing.T) {
			violation := expiredWait(t, r.checkpoint(7*time.Second, invariant.Expired).through(9*time.Second))

			requireStatement(t, violation, "in 5s, ready never held: it evaluated to false")
		})
	}
}

// Ready need not hold on a CR being deleted, so a wait that ends on one names
// what holds it.
func TestAnExpiredWaitSaysTheCRWasStillBeingDeleted(t *testing.T) {
	stuck := []option{spec(3), finalizers("example.com/stuck", "toy"), deleting(2 * time.Second)}
	for name, r := range map[string]*run{
		"ready never held": newRun().
			record(time.Second, widget("10", spec(3), finalizers("example.com/stuck"), status(3, 1))).
			op(invariant.OpDelete, 2*time.Second).
			record(2001*time.Millisecond, widget("11", append(stuck, generation(2), status(3, 1))...)),
		"ready held and stopped": newRun().
			record(time.Second, widget("10", spec(3), finalizers("example.com/stuck"), status(3, 1))).
			op(invariant.OpDelete, 2*time.Second).
			record(2001*time.Millisecond, widget("11", append(stuck, status(3, 1))...)).
			record(3*time.Second, widget("12", append(stuck, status(2, 1))...)),
	} {
		t.Run(name, func(t *testing.T) {
			violation := expiredWait(t, r.checkpoint(7*time.Second, invariant.Expired).through(9*time.Second))

			requireStatement(t, violation, "in 5s, the CR w was still being deleted, held by the finalizers example.com/stuck, toy")
		})
	}
}

// The Observer can record the op's write before the wait begins or after, and
// the verdict does not depend on which.
func TestAnExpiredWaitJudgesTheWriteWhereverTheWaitBegan(t *testing.T) {
	for _, c := range []struct {
		name  string
		began time.Duration
		want  string
	}{
		{"the write recorded after the wait began", 2 * time.Second, "in 5s, ready held until 1s: it evaluated to false"},
		{"the write recorded before it", 2010 * time.Millisecond, "in 4.99s, ready held until 990ms: it evaluated to false"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := newRun().
				record(time.Second, widget("10", spec(3), status(3, 1))).
				op(invariant.OpUpdate, 2*time.Second).
				record(2005*time.Millisecond, widget("11", spec(3), labelled("x"), status(3, 1))).
				record(3*time.Second, widget("12", spec(3), labelled("x"), status(2, 1))).
				checkpoint(7*time.Second, invariant.Expired).
				waitBegan(c.began).
				through(9 * time.Second)

			violation := expiredWait(t, in)

			requireStatement(t, violation, c.want)
		})
	}
}

// A restart writes no CR, so the CR the wait found is the target's.
func TestAnExpiredWaitSaysReadyStoppedHoldingAfterARestart(t *testing.T) {
	in := newRun().
		record(time.Second, widget("10", spec(3), status(3, 1))).
		op(invariant.OpRestart, 2*time.Second).
		record(3*time.Second, widget("11", spec(3), status(0, 1))).
		checkpoint(7*time.Second, invariant.Expired).
		through(9 * time.Second)

	violation := expiredWait(t, in)

	requireStatement(t, violation, "in 5s, ready held until 1s: it evaluated to false")
}

// The wait holds Ready only where it holds on every CR, as the wait reads it.
func TestAnExpiredWaitHoldsReadyOnlyOnEveryCR(t *testing.T) {
	in := newRun().
		record(500*time.Millisecond, object(widgetGVK, "v", "9", spec(3), status(0, 1))).
		op(invariant.OpCreate, time.Second).
		record(1100*time.Millisecond, widget("10", spec(3), status(3, 1))).
		checkpoint(6*time.Second, invariant.Expired).
		through(9 * time.Second)

	violation := expiredWait(t, in)

	requireStatement(t, violation, "in 5s, ready never held: it evaluated to false")
}

func TestAnExpiredWaitQuotesTheCRReadyFailedOn(t *testing.T) {
	in := newRun().
		record(500*time.Millisecond, object(widgetGVK, "v", "9", generation(1), spec(3), status(3, 1))).
		op(invariant.OpCreate, time.Second).
		record(1100*time.Millisecond, widget("10", spec(3), status(0, 1))).
		checkpoint(6*time.Second, invariant.Expired).
		through(9 * time.Second)

	violation := expiredWait(t, in)

	requireStatement(t, violation, "in 5s, ready never held: it evaluated to false")
	if ready := violation.Ready; ready == nil || ready.CR != "w" || ready.Status["ready"] != int64(0) {
		t.Errorf("The violation quotes %+v, want the status of w, where ready failed.", ready)
	}
	if want := "toy.botbox/v1/Widget w"; violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
}

func TestAnExpiredWaitSaysWhereNothingChanged(t *testing.T) {
	in := unreadyCreate(3, 1).checkpoint(5*time.Second, invariant.Expired).through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	requireStatement(t, violation, "in 5s, ready held from 1s on, and nothing changed in the last stable (2s)")
	if got := quoted(violation); strings.Join(got, ",") != "w@10" || violation.Ready == nil || violation.Ready.CR != widgetName {
		t.Errorf("The violation quotes the timeline %v and the ready of %+v, want the CR's.", got, violation.Ready)
	}
}

func TestAnExpiredWaitSaysNoCRWasLeft(t *testing.T) {
	in := newRun().
		record(0, widget("10", spec(1), status(1, 1)), child("w-0", "11")).
		op(invariant.OpDelete, time.Second).
		remove(1100*time.Millisecond, widget("12", spec(1), status(1, 1))).
		record(5*time.Second, child("w-1", "13")).
		checkpoint(6*time.Second, invariant.Expired).
		through(9 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	requireStatement(t, violation, "in 5s, no CR was left to be ready, but the namespace never held still for stable (2s): 1 change")
	if got := quoted(violation); strings.Join(got, ",") != "w-1@13" {
		t.Errorf("The timeline holds %v, want the change that broke the quiet.", got)
	}
}

func TestAnExpiredWaitQuotesNoRequestMadeAfterIt(t *testing.T) {
	in := expiredSettle().
		request(3*time.Second, get("w-0")).
		request(6*time.Second, get("w-1")).
		request(7*time.Second, get("w-2")).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if len(violation.Requests) != 2 || violation.Requests[1].Name != "w-1" || violation.RequestsTotal != 2 {
		t.Errorf("The violation quotes %v of %d requests, want the two made by its end.", violation.Requests, violation.RequestsTotal)
	}
}

// A ready that yields a non-bool is a configuration error wherever it is first
// evaluated.
func TestAReadyThatYieldsNoBoolIsAConfigurationError(t *testing.T) {
	for name, r := range map[string]*run{
		"at a deadline":      unreadyCreate(0, 1),
		"at an expired wait": expiredSettle(),
	} {
		t.Run(name, func(t *testing.T) {
			r.in.Target.Ready = yieldsAnInt

			result, err := invariant.Convergence(r.through(8 * time.Second))

			if !errors.Is(err, target.ErrNotBool) {
				t.Errorf("G4 returned %v and the error %v, want the error of a ready that yields no bool.", statements(result), err)
			}
		})
	}
}

// A readiness verdict quotes the predicate and the status it read.
func TestAReadinessVerdictQuotesTheReadyPredicate(t *testing.T) {
	for name, r := range map[string]*run{
		"at a deadline":      unreadyCreate(0, 1),
		"at an expired wait": expiredSettle(),
	} {
		t.Run(name, func(t *testing.T) {
			r.in.Target.Ready = noSuchKey
			r.in.Target.ReadyExpr = "status.readyy == spec.count"

			violation := fired(t, invariant.Convergence, r.through(8*time.Second))

			ready := violation.Ready
			if ready == nil {
				t.Fatal("The violation quotes no ready predicate.")
			}
			if ready.Expr != "status.readyy == spec.count" {
				t.Errorf("The violation quotes the expression %q.", ready.Expr)
			}
			if ready.CR != widgetName {
				t.Errorf("The violation names the CR %q, want %q.", ready.CR, widgetName)
			}
			if !strings.Contains(ready.Error, "no such key: readyy") {
				t.Errorf("The violation quotes the error %q, want the one evaluating it.", ready.Error)
			}
			if got := ready.Status["ready"]; got != int64(0) {
				t.Errorf("The violation quotes status.ready %v, want the CR's 0.", got)
			}
		})
	}
}

// An error loop under backoff can stay under G6's threshold, and the verdict
// it causes names the request instead.
func TestAVerdictNamesAFailingRequestTheTargetRepeated(t *testing.T) {
	for _, c := range []struct {
		name  string
		check invariant.Check
		run   *run
	}{
		{"G4 at a deadline", invariant.Convergence, unreadyCreate(0, 1)},
		{"G4 at an expired wait", invariant.Convergence, expiredSettle()},
		{"G1", invariant.BoundedReconciliation, newRun().op(invariant.OpCreate, 0).settled(time.Second, invariant.Converged)},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := c.run.
				request(1100*time.Millisecond, failedGet("w-0", http.StatusInternalServerError)).
				request(1200*time.Millisecond, failedGet("w-1", http.StatusNotFound)).
				request(1300*time.Millisecond, failedGet("w-0", http.StatusBadRequest)).
				through(8 * time.Second)

			violation := fired(t, c.check, in)

			requireStatement(t, violation, "; the target repeated the failing request get configmaps/w-0 2 times, the last answered 400")
		})
	}
}

func TestAVerdictNamesTheRequestThatFirstFailedMostOften(t *testing.T) {
	in := unreadyCreate(0, 1).
		request(1100*time.Millisecond, failedGet("w-0", http.StatusNotFound)).
		request(1200*time.Millisecond, failedGet("w-1", http.StatusNotFound)).
		request(1300*time.Millisecond, failedGet("w-0", http.StatusNotFound)).
		request(1400*time.Millisecond, failedGet("w-1", http.StatusNotFound)).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	requireStatement(t, violation, "; the target repeated the failing request get configmaps/w-0 2 times")
}

func TestAVerdictNamesNoRequestTheTargetDidNotRepeat(t *testing.T) {
	refused := failedGet("w-0", http.StatusInternalServerError)
	refused.Fault = "error 500"
	for _, c := range []struct {
		name string
		run  *run
	}{
		{"one failure", unreadyCreate(0, 1).request(1100*time.Millisecond, failedGet("w-0", http.StatusNotFound))},
		{"successes", unreadyCreate(0, 1).requests(time.Second, 100*time.Millisecond, 3, get("w-0"))},
		{"lost races", unreadyCreate(0, 1).requests(time.Second, 100*time.Millisecond, 3, conflicted("update", "w-0"))},
		{"what a fault refused", unreadyCreate(0, 1).requests(time.Second, 100*time.Millisecond, 3, refused)},
		{"failures before the window", newRun().
			requests(0, 100*time.Millisecond, 3, failedGet("w-0", http.StatusNotFound)).
			op(invariant.OpCreate, time.Second).
			record(1100*time.Millisecond, widget("10", spec(3), status(0, 1)))},
		{"failures after the window", unreadyCreate(0, 1).requests(5100*time.Millisecond, 100*time.Millisecond, 3, failedGet("w-0", http.StatusNotFound))},
	} {
		t.Run(c.name, func(t *testing.T) {
			violation := fired(t, invariant.Convergence, c.run.through(9*time.Second))

			if strings.Contains(violation.Statement, "repeated") {
				t.Errorf("The statement is %q, and the target repeated no failing request.", violation.Statement)
			}
		})
	}
}
