package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"path"
	"slices"
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
// SetFaults call. The zero Trigger never ends, which is how the Runner drives
// the op-index trigger of DESIGN.md §5.2.
type Trigger struct {
	Count int
	For   time.Duration
}

// SetFaults replaces the active faults. A spec the proxy already holds keeps
// what it has done so far, so that adding or dropping one fault does not
// restart another's trigger. The first spec that matches a request wins.
func (p *Proxy) SetFaults(specs []FaultSpec) {
	p.mu.Lock()
	defer p.mu.Unlock()
	held := slices.Clone(p.faults)
	faults := make([]*activeFault, len(specs))
	for i, spec := range specs {
		if j := slices.IndexFunc(held, func(f *activeFault) bool { return f != nil && f.spec == spec }); j >= 0 {
			faults[i], held[j] = held[j], nil
			continue
		}
		faults[i] = &activeFault{
			spec:   spec,
			since:  time.Now(),
			random: rand.New(rand.NewPCG(uint64(p.seed), uint64(i))),
		}
	}
	p.faults = faults
}

// ClearFaults removes every active fault.
func (p *Proxy) ClearFaults() { p.SetFaults(nil) }

// FaultWindow is what the proxy has done with one fault (DESIGN.md §5.2).
type FaultWindow struct {
	// First is when the proxy first applied the fault, and zero if it never
	// has: a fault that matches no request changes nothing about the run.
	First time.Time
	// Retired is when the proxy stopped applying the fault, and zero while it
	// would still apply it.
	Retired time.Time
}

// Windows reports what the proxy has done with each fault of the last
// SetFaults call, in that order. The Runner reads them into the run's timeline:
// a fault excuses the target over the window the proxy applied it in, and a
// fault it never applied excuses nothing (DESIGN.md §6).
func (p *Proxy) Windows() []FaultWindow {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	windows := make([]FaultWindow, len(p.faults))
	for i, fault := range p.faults {
		windows[i] = FaultWindow{First: fault.first, Retired: fault.retiredBy(now)}
	}
	return windows
}

type activeFault struct {
	spec    FaultSpec
	since   time.Time
	random  *rand.Rand
	applied int
	// first is when the fault was applied to a request, and spent is when the
	// request that used up Until.Count arrived.
	first, spent time.Time
}

func (f *activeFault) expired(now time.Time) bool {
	if f.spec.Until.Count > 0 && f.applied >= f.spec.Until.Count {
		return true
	}
	return f.spec.Until.For > 0 && now.Sub(f.since) >= f.spec.Until.For
}

func (f *activeFault) applies(r Request, now time.Time) bool {
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
// while it still applies. A count runs out on the request that spends it, and
// a window runs out on the clock, whether or not a request came.
func (f *activeFault) retiredBy(now time.Time) time.Time {
	retired := f.spent
	if f.spec.Until.For > 0 {
		if ends := f.since.Add(f.spec.Until.For); !ends.After(now) && (retired.IsZero() || ends.Before(retired)) {
			retired = ends
		}
	}
	return retired
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
