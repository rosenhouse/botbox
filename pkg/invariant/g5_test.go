package invariant_test

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rosenhouse/botbox/pkg/invariant"
	"github.com/rosenhouse/botbox/pkg/observe"
)

// restartRun converges at 5s, restarts the target at 10s and converges again
// at 15s. The Widget lives across the restart; the objects are what G5
// compares.
func restartRun(before, after []*unstructured.Unstructured) *run {
	return newRun().
		record(time.Second, append([]*unstructured.Unstructured{widget("10", spec(1), status(1, 1))}, before...)...).
		checkpoint(5*time.Second, invariant.Converged).
		op(invariant.OpRestart, 10*time.Second).
		record(12*time.Second, append([]*unstructured.Unstructured{widget("20", spec(1), status(1, 1))}, after...)...).
		checkpoint(15*time.Second, invariant.Converged)
}

// restarted returns a run whose one managed object changed across the restart.
func restarted(before, after *unstructured.Unstructured) invariant.Input {
	return restartRun([]*unstructured.Unstructured{before}, []*unstructured.Unstructured{after}).
		through(20 * time.Second)
}

func TestG5PassesWhenTheRestartChangesNothing(t *testing.T) {
	in := restarted(child("w-0", "11", data("0")), child("w-0", "21", data("0")))

	silent(t, invariant.RestartStable, in)
}

func TestG5FiresOnAnObjectOnlyTheRestartBroughtBack(t *testing.T) {
	in := restartRun(nil, []*unstructured.Unstructured{child("w-0", "21", data("0"))}).
		through(20 * time.Second)

	violation := fired(t, invariant.RestartStable, in)

	if violation.ID != "G5" {
		t.Errorf("The violation is %q, want G5.", violation.ID)
	}
	if !strings.Contains(violation.Statement, "w-0") {
		t.Errorf("The statement is %q, want it to name the ConfigMap the restart brought back.", violation.Statement)
	}
	if want := timelineOf(configMapGVK, "w-0"); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
	if len(violation.Versions) == 0 || violation.Versions[0].Name != "w-0" {
		t.Fatalf("The evidence holds %v, want the ConfigMap's timeline.", violation.Versions)
	}
}

func TestG5FiresOnAnObjectTheRestartDropped(t *testing.T) {
	in := restartRun([]*unstructured.Unstructured{child("w-0", "11", data("0"))}, nil).
		record(13*time.Second, child("w-0", "22", data("0"))).
		remove(14*time.Second, child("w-0", "23", data("0"))).
		through(20 * time.Second)

	violation := fired(t, invariant.RestartStable, in)

	if !strings.Contains(violation.Statement, "w-0") {
		t.Errorf("The statement is %q, want it to name the ConfigMap that went.", violation.Statement)
	}
}

func TestG5IgnoresThePathsSection6Names(t *testing.T) {
	for _, ignored := range []struct {
		path          string
		before, after option
	}{
		{"metadata.resourceVersion", nothing, nothing},
		{"metadata.uid", uid("uid-old"), uid("uid-new")},
		{"metadata.creationTimestamp", created(0), created(11 * time.Second)},
		{"metadata.generation", generation(1), generation(7)},
		{"metadata.managedFields", managedFields("one"), managedFields("two")},
		{"status.conditions[*].lastTransitionTime", condition("True", 0), condition("True", 12*time.Second)},
	} {
		t.Run(ignored.path, func(t *testing.T) {
			in := restarted(child("w-0", "11", ignored.before), child("w-0", "21", ignored.after))

			silent(t, invariant.RestartStable, in)
		})
	}
}

func TestG5IgnoresAnOwnerReferenceWhoseOwnerIsGone(t *testing.T) {
	in := restarted(child("w-0", "11", ownedByGhost), child("w-0", "21", orphaned))

	silent(t, invariant.RestartStable, in)
}

func TestG5ComparesEverythingElse(t *testing.T) {
	for _, compared := range []struct {
		path          string
		before, after option
	}{
		{"data", data("0"), data("1")},
		{"metadata.labels", labelled("one"), labelled("two")},
		{"metadata.annotations", annotated("one"), annotated("two")},
		{"metadata.finalizers", finalizers("keep/one"), finalizers("keep/two")},
		{"metadata.ownerReferences", ownedByWidget, orphaned},
		{"status.conditions[*].status", condition("True", 0), condition("False", 0)},
	} {
		t.Run(compared.path, func(t *testing.T) {
			in := restarted(child("w-0", "11", compared.before), child("w-0", "21", compared.after))

			violation := fired(t, invariant.RestartStable, in)
			if !strings.Contains(violation.Statement, "w-0") {
				t.Errorf("The statement is %q, want it to name the ConfigMap that changed.", violation.Statement)
			}
		})
	}
}

func TestG5IgnoresThePathsTheTargetExcludes(t *testing.T) {
	in := restarted(child("w-0", "11", data("0")), child("w-0", "21", data("1")))
	in.Target.EqualIgnore = []string{"data.index"}

	silent(t, invariant.RestartStable, in)
}

func TestG5UsesTheTargetsOwnEquality(t *testing.T) {
	in := restarted(child("w-0", "11", data("0")), child("w-0", "21", data("1")))
	in.Target.Equal = func(a, b observe.Snapshot) bool { return a.Name == b.Name }

	silent(t, invariant.RestartStable, in)
}

func TestG5SaysSoWhenASnapshotIsMissing(t *testing.T) {
	for _, missing := range []struct {
		side        string
		checkpoints []invariant.SettleResult
	}{
		{"before", []invariant.SettleResult{invariant.Expired, invariant.Converged}},
		{"after", []invariant.SettleResult{invariant.Converged, invariant.Expired}},
	} {
		t.Run(missing.side, func(t *testing.T) {
			in := restarted(child("w-0", "11", data("0")), child("w-0", "21", data("1")))
			in.Checkpoints[0].Settle = missing.checkpoints[0]
			in.Checkpoints[1].Settle = missing.checkpoints[1]

			result := silent(t, invariant.RestartStable, in)
			if len(result.Notes) != 1 || !strings.Contains(result.Notes[0], missing.side) {
				t.Fatalf("G5 noted %v, want one note saying the snapshot %s the Restart is missing.", result.Notes, missing.side)
			}
		})
	}
}

func TestG5RunsOncePerRestart(t *testing.T) {
	in := restartRun([]*unstructured.Unstructured{child("w-0", "11", data("0"))}, []*unstructured.Unstructured{child("w-0", "21", data("1"))}).
		op(invariant.OpRestart, 16*time.Second).
		record(17*time.Second, child("w-0", "31", data("2"))).
		checkpoint(18*time.Second, invariant.Converged).
		through(20 * time.Second)

	result, err := invariant.RestartStable(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Violations) != 2 {
		t.Fatalf("G5 reported %v, want one violation per Restart.", statements(result))
	}
}

// changedAcross converges at 5s and at 15s on states that differ, with
// whatever ops botbox runs in between.
func changedAcross(ops func(*run) *run) invariant.Input {
	return ops(newRun().
		record(time.Second, widget("10", spec(1), status(1, 1)), child("w-0", "11", data("0"))).
		checkpoint(5*time.Second, invariant.Converged)).
		record(13*time.Second, widget("20", spec(1), status(1, 1)), child("w-0", "21", data("1"))).
		checkpoint(15*time.Second, invariant.Converged).
		through(20 * time.Second)
}

// changedAround restarts the target at 10s between states converged at 5s and
// 15s that differ, and then runs whatever ops botbox runs next.
func changedAround(next func(*run) *run) invariant.Input {
	return changedAcross(func(r *run) *run { return next(r.op(invariant.OpRestart, 10*time.Second)) })
}

// updatedAround is changedAround with botbox's update at the given time.
func updatedAround(update time.Duration) invariant.Input {
	if update > 10*time.Second {
		return changedAround(func(r *run) *run { return r.op(invariant.OpUpdate, update) })
	}
	return changedAcross(func(r *run) *run {
		return r.op(invariant.OpUpdate, update).op(invariant.OpRestart, 10*time.Second)
	})
}

// An op stamped at the instant a state converged came after that state.
func TestG5LeavesARestartUnjudgedOnlyForAnUpdateBetweenTheStatesItCompares(t *testing.T) {
	for _, update := range []struct {
		at   time.Duration
		note string
	}{
		{0, ""},
		{5 * time.Second, "for op 1 (restart): op 0 (update) ran"},
		{7 * time.Second, "for op 1 (restart): op 0 (update) ran"},
		{11 * time.Second, "for op 0 (restart): op 1 (update) ran"},
		{15 * time.Second, ""},
		{16 * time.Second, ""},
	} {
		t.Run(update.at.String(), func(t *testing.T) {
			in := updatedAround(update.at)

			if update.note == "" {
				fired(t, invariant.RestartStable, in)
				return
			}
			noted(t, invariant.RestartStable, in, update.note)
		})
	}
}

func TestG5LeavesARestartUnjudgedOnlyForAnOpThatChangesTheRun(t *testing.T) {
	for _, between := range []struct {
		name     string
		apply    func(*run) *run
		confound bool
	}{
		{"create", func(r *run) *run { return r.op(invariant.OpCreate, 11*time.Second) }, true},
		{"update", func(r *run) *run { return r.op(invariant.OpUpdate, 11*time.Second) }, true},
		{"delete", func(r *run) *run { return r.op(invariant.OpDelete, 11*time.Second) }, true},
		{"recreate", func(r *run) *run { return r.op(invariant.OpRecreate, 11*time.Second) }, true},
		{"deleteManaged of w-0", func(r *run) *run { return r.deletedManaged(11*time.Second, "w-0") }, true},
		{"deleteManaged of nothing", func(r *run) *run { return r.op(invariant.OpDeleteManaged, 11*time.Second) }, false},
		{"fault", func(r *run) *run { return r.op(invariant.OpFault, 11*time.Second) }, false},
		{"settle", func(r *run) *run { return r.op(invariant.OpSettle, 11*time.Second) }, false},
		{"restart", func(r *run) *run { return r.op(invariant.OpRestart, 11*time.Second) }, false},
	} {
		t.Run(between.name, func(t *testing.T) {
			in := changedAround(between.apply)

			if between.confound {
				noted(t, invariant.RestartStable, in, "for op 0 (restart): op 1 ("+string(in.Ops[1].Type)+") ran")
				return
			}
			result := evaluate(t, invariant.RestartStable, in)
			if len(result.Violations) == 0 || len(result.Notes) > 0 {
				t.Fatalf("G5 reported %v and noted %v, want the restart judged.", statements(result), result.Notes)
			}
		})
	}
}

func TestG5NamesTheFirstOpThatKeptItFromJudgingARestart(t *testing.T) {
	in := changedAround(func(r *run) *run {
		return r.op(invariant.OpUpdate, 11*time.Second).op(invariant.OpDelete, 12*time.Second)
	})

	noted(t, invariant.RestartStable, in, "op 1 (update) ran")
}

func TestG5ComparesTheMetadataSection6DoesNotIgnore(t *testing.T) {
	in := restarted(child("w-0", "11", data("0")), child("w-0", "21", data("0"), deleting(12*time.Second)))

	violation := fired(t, invariant.RestartStable, in)

	if !strings.Contains(violation.Statement, "w-0") {
		t.Errorf("The statement is %q, want it to name the ConfigMap the restart left terminating.", violation.Statement)
	}
}
