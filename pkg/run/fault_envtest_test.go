//go:build envtest

package run_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/rosenhouse/botbox/pkg/proxy"
	"github.com/rosenhouse/botbox/pkg/run"
)

// seededBugB11 selects the bug of DESIGN.md §9.1 that a refused create makes
// permanent. A later --bug wins over the one target.yaml declares (§11).
const seededBugB11 = "--bug=11"

// The acceptance of DESIGN.md §10 M6: a fault makes the toy fail an invariant
// it passes without the fault.
//
// The fault refuses one ConfigMap create, and B11 goes on believing in the
// child it never made. The toy falls quiet one child short of its spec, so the
// settle wait after the scale-up expires however wide the windows are: neither
// a backoff interval nor a window width enters into the outcome. The fault's
// own count ends it, so the run is judged from the refusal on (D36).
func TestAFaultMakesTheToyFailAnInvariantItOtherwisePasses(t *testing.T) {
	ctx := t.Context()
	// One target for both runs, so that the fault is the only difference.
	toy := loadTarget(t, buildToy(t))
	toy.Launch.Args = append(toy.Launch.Args, seededBugB11)
	testCluster := startCluster(t, toy.CRDs)
	dir := t.TempDir()
	faulted, err := run.ReadSequence("../../targets/toy-widget/sequences/b11-fault.json")
	if err != nil {
		t.Fatalf("Reading the sequence failed: %v", err)
	}

	found, err := run.Run(ctx, toy, faulted, run.Options{
		Dir: filepath.Join(dir, "faulted"), Config: testCluster.Config(), Check: run.Engine{},
	})
	if err != nil {
		t.Fatalf("The faulted run failed: %v", err)
	}
	if found.Violation == nil {
		t.Fatal("The faulted run found nothing, and the create the fault refused leaves B11 a child short for good.")
	}
	// Pin the check. If the fault starts tripping something else, the test
	// would otherwise pass for a reason it does not describe.
	if found.Violation.ID != "G4" {
		t.Errorf("The faulted run reported %s, and this sequence is written to break G4: %s",
			found.Violation.ID, found.Violation.Evidence)
	}
	// The toy asked the API server for its one child once and never again,
	// which is what makes the state permanent rather than slow to recover.
	if asked, denied := configMapCreates(found.Recorded.Requests); asked != 1 || denied != 1 {
		t.Errorf("The toy made %d creates of configmaps, %d of them refused, want the one child asked for once and refused.",
			asked, denied)
	}
	// §10 M6 asks the report to name the evidence, and this is the run that
	// shows it: the create the fault refused is in what the violation carries.
	if !slices.ContainsFunc(found.Violation.Requests, refusedCreate) {
		t.Errorf("The violation carries %d requests, none of them the create the fault refused.", len(found.Violation.Requests))
	}
	if len(found.Violation.Versions) == 0 {
		t.Error("The violation carries no version of the CR, and its report has a table to quote them in.")
	}

	// The control is this sequence with the fault taken out, so nothing but the
	// fault can account for the difference.
	clean, err := run.Run(ctx, toy, without(faulted, run.OpFault), run.Options{
		Dir: filepath.Join(dir, "clean"), Config: testCluster.Config(), Check: run.Engine{},
	})
	if err != nil {
		t.Fatalf("The control run failed: %v", err)
	}
	if clean.Violation != nil {
		t.Errorf("The control reports %+v, so the %s the faulted run found is not the fault's.",
			clean.Violation, found.Violation.ID)
	}
}

// configMapCreates counts what the toy asked the API server to create and how
// many of those the proxy refused.
func configMapCreates(log []proxy.Request) (asked, denied int) {
	for _, request := range log {
		if !configMapCreate(request) {
			continue
		}
		asked++
		if refusedCreate(request) {
			denied++
		}
	}
	return asked, denied
}

func configMapCreate(request proxy.Request) bool {
	return request.Verb == "create" && request.Resource == "configmaps"
}

// refusedCreate is the create the fault answered with a 500.
func refusedCreate(request proxy.Request) bool {
	return configMapCreate(request) && request.Fault != ""
}

// without is the sequence with every op of that type removed, renumbered so it
// stays legal (DESIGN.md §7).
func without(s run.Sequence, remove run.OpType) run.Sequence {
	shorter := s
	shorter.Ops = nil
	for _, op := range s.Ops {
		if op.Type == remove {
			continue
		}
		op.Index = len(shorter.Ops)
		shorter.Ops = append(shorter.Ops, op)
	}
	return shorter
}
