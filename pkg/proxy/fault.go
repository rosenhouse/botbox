package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"path"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// FaultSpec is one fault the proxy injects (DESIGN.md §5.2).
type FaultSpec struct {
	Match  RequestMatcher
	Action FaultAction
	Until  Trigger
}

// RequestMatcher selects the requests a fault applies to. An unset field
// matches every request.
type RequestMatcher struct {
	Verb     string
	Resource string
	Name     string // exact, or a glob as path.Match reads it
	// Fraction is the share of matching requests the fault applies to, drawn
	// from Options.Seed so a replay repeats it. Values outside (0, 1) apply
	// the fault to every matching request.
	Fraction float64
}

// Matches reports whether r satisfies every field but Fraction, which the
// proxy draws per request.
func (m RequestMatcher) Matches(r Request) bool {
	if m.Verb != "" && m.Verb != r.Verb {
		return false
	}
	if m.Resource != "" && m.Resource != r.Resource {
		return false
	}
	if m.Name != "" {
		matched, err := path.Match(m.Name, r.Name)
		if err != nil || !matched {
			return false
		}
	}
	return true
}

// FaultAction is what the proxy does to a matched request: Error, Delay or
// Drop.
type FaultAction interface {
	fmt.Stringer
	faultAction()
}

// Error replaces the upstream response with a metav1.Status of this code.
type Error struct{ Code int }

// Delay holds the request back before forwarding it.
type Delay struct{ For time.Duration }

// Drop closes the connection without a response.
type Drop struct{}

func (Error) faultAction() {}
func (Delay) faultAction() {}
func (Drop) faultAction()  {}

func (a Error) String() string { return fmt.Sprintf("error(%d)", a.Code) }
func (a Delay) String() string { return fmt.Sprintf("delay(%s)", a.For) }
func (Drop) String() string    { return "drop" }

// Trigger ends a fault after a count of applications or a duration from the
// AddFault call. A fault with the zero Trigger ends only at RemoveFault or
// ClearFaults.
type Trigger struct {
	Count int
	For   time.Duration
}

// FaultID names a fault that AddFault gave the proxy. Two faults can have
// equal specs.
type FaultID int

// AddFault has the proxy apply the fault after those it already holds. The
// first fault that applies to a request wins.
func (p *Proxy) AddFault(spec FaultSpec) FaultID {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := FaultID(len(p.faults))
	p.faults = append(p.faults, &injectedFault{
		spec:   spec,
		since:  time.Now(),
		random: rand.New(rand.NewPCG(uint64(p.seed), uint64(id))),
	})
	return id
}

// RemoveFault retires the fault at once.
func (p *Proxy) RemoveFault(id FaultID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults[id].remove()
}

// ClearFaults retires every fault at once.
func (p *Proxy) ClearFaults() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, fault := range p.faults {
		fault.remove()
	}
}

// FaultWindow is what the proxy has done with one fault (DESIGN.md §5.2).
type FaultWindow struct {
	// First is when the proxy first applied the fault, and zero if it never
	// has: a fault that matches no request changes nothing about the run.
	First time.Time
	// Retired is when the proxy stopped applying the fault, and zero while it
	// would still apply it.
	Retired time.Time
}

// Window reports what the proxy has done with the fault, removed or not. The
// Runner reads it into the run's timeline: a fault excuses the target over the
// window the proxy applied it in, and a fault it never applied excuses nothing
// (DESIGN.md §6).
func (p *Proxy) Window(id FaultID) FaultWindow {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	fault := p.faults[id]
	return FaultWindow{First: fault.first, Retired: fault.retiredBy(now)}
}

type injectedFault struct {
	spec    FaultSpec
	since   time.Time
	random  *rand.Rand
	applied int
	// first is when the fault was applied to a request, spent is when the
	// request that used up Until.Count arrived, and removed is when the
	// caller retired it.
	first, spent, removed time.Time
}

// remove stamps the removal. The caller holds the proxy's lock, so no request
// the fault applied to comes after it.
func (f *injectedFault) remove() {
	if f.removed.IsZero() {
		f.removed = time.Now()
	}
}

func (f *injectedFault) expired(now time.Time) bool { return !f.retiredBy(now).IsZero() }

func (f *injectedFault) applies(r Request, now time.Time) bool {
	if !f.spec.Match.Matches(r) {
		return false
	}
	if fraction := f.spec.Match.Fraction; fraction > 0 && fraction < 1 && f.random.Float64() >= fraction {
		return false
	}
	f.applied++
	if f.applied == 1 {
		f.first = now
	}
	if f.spec.Until.Count > 0 && f.applied >= f.spec.Until.Count {
		f.spent = now
	}
	return true
}

// retiredBy is when the proxy stopped applying the fault, or the zero time
// while it still applies. A count runs out on the request that spends it, a
// window runs out on the clock, whether or not a request came, and a removal
// retires the fault at once.
func (f *injectedFault) retiredBy(now time.Time) time.Time {
	retired := earliest(f.spent, f.removed)
	if windowEnd := f.since.Add(f.spec.Until.For); f.spec.Until.For > 0 && !windowEnd.After(now) {
		retired = earliest(retired, windowEnd)
	}
	return retired
}

// earliest is the earlier of two instants, where a zero one has not come.
func earliest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

// faultFor returns the action of the first active fault the request matches.
func (p *Proxy) faultFor(r Request) FaultAction {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, fault := range p.faults {
		if !fault.expired(now) && fault.applies(r, now) {
			return fault.spec.Action
		}
	}
	return nil
}

// writeStatus replaces the upstream response with a metav1.Status, so the
// target decodes the error whatever content type it negotiated.
func writeStatus(w http.ResponseWriter, code int, r Request) {
	body, _ := json.Marshal(faultStatus(code, r)) // A Status always marshals.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(body)
}

func faultStatus(code int, r Request) metav1.Status {
	resource := schema.GroupResource{Group: r.Group, Resource: r.Resource}
	status := apierrors.NewGenericServerResponse(code, r.Verb, resource, r.Name, "", 0, false).ErrStatus
	status.TypeMeta = metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}
	status.Message = fmt.Sprintf("botbox fault: %s %s", r.Verb, r.Path)
	return status
}

// drop closes the connection without a response.
func drop(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	conn.Close()
}

// sleep reports whether the delay elapsed before the request was canceled.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
