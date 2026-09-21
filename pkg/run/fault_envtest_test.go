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
// The fault errors the toy's ConfigMap creates. controller-runtime backs off
// exponentially, so about ten are refused before the teardown clears the fault
// — the count trigger is an upper bound and is never reached. The toy is then
// still retrying, and the create it finally lands falls inside the quiet
// window the teardown waits, which is G1's business. The toy has no seeded
// bug: what fails is its recovery time, which every controller that retries
// has.
//
// The margin is thin. The create lands about 115ms into a window T_stable
// wide, because controller-runtime's backoff ladder puts it at 5.115s while
// the window opens at T_settle, 5s. A harness stall of that order puts the
// retry back under the fault, and the one after it is 10s later, outside
// everything. Issue #11 tracks making this deterministic rather than close.
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
		t.Fatalf("The faulted run found nothing. The toy's retry after the fault clears is " +
			"meant to land in the teardown's quiet window, and it has about 115ms of room: " +
			"if this is a stall rather than a real change, see issue #11.")
	}
	// Pin the check. If the fault starts tripping something else, the test
	// would otherwise pass for a reason it does not describe.
	if found.Violation.ID != "G1" {
		t.Errorf("The faulted run reported %s, and this sequence is written to break G1: %s",
			found.Violation.ID, found.Violation.Evidence)
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
