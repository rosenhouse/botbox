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
// SetFaults call. The zero Trigger never ends, which is how the Runner drives
// the op-index trigger of DESIGN.md §5.2.
type Trigger struct {
	Count int
	For   time.Duration
}

// SetFaults replaces the active faults. The first spec that matches a request
// wins.
func (p *Proxy) SetFaults(specs []FaultSpec) {
	faults := make([]*activeFault, len(specs))
	for i, spec := range specs {
		faults[i] = &activeFault{
			spec:   spec,
			since:  time.Now(),
			random: rand.New(rand.NewPCG(uint64(p.seed), uint64(i))),
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults = faults
}

// ClearFaults removes every active fault.
func (p *Proxy) ClearFaults() { p.SetFaults(nil) }

type activeFault struct {
	spec    FaultSpec
	since   time.Time
	random  *rand.Rand
	applied int
}

func (f *activeFault) expired(now time.Time) bool {
	if f.spec.Until.Count > 0 && f.applied >= f.spec.Until.Count {
		return true
	}
	return f.spec.Until.For > 0 && now.Sub(f.since) >= f.spec.Until.For
}

func (f *activeFault) applies(r Request) bool {
	if !f.spec.Match.Matches(r) {
		return false
	}
	if fraction := f.spec.Match.Fraction; fraction > 0 && fraction < 1 && f.random.Float64() >= fraction {
		return false
	}
	f.applied++
	return true
}

// faultFor returns the action of the first active fault the request matches.
func (p *Proxy) faultFor(r Request) FaultAction {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, fault := range p.faults {
		if !fault.expired(now) && fault.applies(r) {
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
