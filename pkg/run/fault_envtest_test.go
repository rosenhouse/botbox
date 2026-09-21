//go:build envtest

package run_test

import (
	"path/filepath"
	"testing"

	"github.com/rosenhouse/botbox/pkg/run"
)

// The acceptance of DESIGN.md §10 M6: a fault makes the toy fail an invariant
// it passes without the fault.
//
// The fault errors the toy's ConfigMap creates until thirty have been refused.
// controller-runtime backs off exponentially, so by the time the fault clears
// the toy is still retrying, and the create it finally lands falls inside the
// quiet window the teardown waits. That is G1's business. The toy has no
// seeded bug: what fails here is its recovery time, which is a property of
// every controller that retries.
func TestAFaultMakesTheToyFailAnInvariantItOtherwisePasses(t *testing.T) {
	ctx := t.Context()
	toy := loadTarget(t, buildToy(t))
	testCluster := startCluster(t, toy.CRDs)
	dir := t.TempDir()
	faulted, err := run.ReadSequence("../../targets/toy-widget/sequences/fault.json")
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
		t.Fatalf("The faulted run found nothing, and the fault refuses thirty creates.")
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
