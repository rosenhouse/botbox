//go:build envtest

package run_test

import (
	"path/filepath"
	"slices"
	"strings"
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

// The toy with no bug retries a refused create, backing off as it goes, so it
// recovers once the fault stops, however the fault stopped. B11 never asks
// again. fault.json is the README's example: the teardown clears its fault.
func TestAFaultLeavesTheTargetTimeToRecover(t *testing.T) {
	ctx := t.Context()
	binary := buildToy(t)
	testCluster := startCluster(t, loadTarget(t, binary).CRDs)
	const example = "../../targets/toy-widget/sequences/fault.json"
	runUnder := func(t *testing.T, launchArgs []string, sequence run.Sequence) run.Result {
		t.Helper()
		toy := loadTarget(t, binary)
		toy.Launch.Args = append(toy.Launch.Args, launchArgs...)
		result, err := run.Run(ctx, toy, sequence, run.Options{Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{}})
		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		return result
	}
	readExample := func(t *testing.T) run.Sequence {
		t.Helper()
		sequence, err := run.ReadSequence(example)
		if err != nil {
			t.Fatalf("Reading the sequence failed: %v", err)
		}
		return sequence
	}

	t.Run("after the teardown clears the fault", func(t *testing.T) {
		result := runUnder(t, nil, readExample(t))

		if result.Violation != nil {
			t.Errorf("The run reported %v, and the toy with no bug recovers.", result.Violation)
		}
		if recovery := result.Timeline.Recovery; recovery == nil || !recovery.Converged {
			t.Errorf("The teardown recorded the recovery %+v, want a wait that converged.", recovery)
		}
	})

	t.Run("after the fault runs out inside a settle wait", func(t *testing.T) {
		// Ten refusals back the toy off for 2.56s before its next create,
		// which then needs T_stable of quiet: more than T_settle after the
		// update.
		sequence := readExample(t)
		sequence.Ops[1].Fault.Until.Count = 10

		result := runUnder(t, nil, sequence)

		if result.Violation != nil {
			t.Errorf("The run reported %v, and the toy with no bug recovers.", result.Violation)
		}
		settle := loadTarget(t, binary).Timeouts.Settle
		if wait := result.Timeline.Ops[2].Settled; wait == nil || wait.Window.End.Sub(wait.Window.Start) <= settle {
			t.Errorf("The update's wait was %+v, want one that ran past T_settle of %v.", wait, settle)
		}
	})

	t.Run("never, under a bug that never asks again", func(t *testing.T) {
		result := runUnder(t, []string{seededBugB11}, readExample(t))

		if result.Violation == nil || result.Violation.ID != "G4" ||
			!strings.Contains(result.Violation.Statement, "after the last fault stopped") {
			t.Errorf("The run reported %v, want the G4 of the wait after the last fault stopped.", result.Violation)
		}
	})
}

// The fault names the toy's CRD, which the API server serves as widgets.
func TestAFaultOnAKindNameEndsTheRun(t *testing.T) {
	toy := loadTarget(t, buildToy(t))
	testCluster := startCluster(t, toy.CRDs)
	sequence := run.Sequence{Seed: 1, Target: toy.Name, Ops: []run.Op{
		{Index: 0, Type: run.OpCreate, Obj: toy.Sample.DeepCopy()},
		{Index: 1, Type: run.OpFault, Fault: &run.Fault{Match: run.Match{Resource: "Widget"}, Action: run.Action{Error: 500}}},
		{Index: 2, Type: run.OpSettle},
	}}

	_, err := run.Run(t.Context(), toy, sequence, run.Options{Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{}})

	if err == nil || !strings.Contains(err.Error(), "did you mean widgets?") {
		t.Errorf("The run returned %v, want it to name widgets.", err)
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
