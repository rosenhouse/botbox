package invariant_test

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rosenhouse/botbox/pkg/invariant"
	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/target"
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

func TestG5CountsAnEmptyConditionListAsNone(t *testing.T) {
	noConditions := func(u *unstructured.Unstructured) { u.Object["status"] = map[string]any{"conditions": []any{}} }
	in := restarted(child("w-0", "11", noConditions), child("w-0", "21"))

	silent(t, invariant.RestartStable, in)
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
	in := restarted(
		child("w-0", "11", data("0"), annotations(map[string]string{startedAt: "1"})),
		child("w-0", "21", data("1"), annotations(map[string]string{startedAt: "2"})))
	in.Target.EqualIgnore = []target.Path{
		target.MustParsePath("data.index"),
		target.MustParsePath(`metadata.annotations["` + startedAt + `"]`),
	}

	silent(t, invariant.RestartStable, in)
}

// The owner's UID is what tells a live owner from a dangling reference, so
// G5 reads it before it ignores it.
func TestG5ComparesALiveOwnerWhoseUIDItIgnores(t *testing.T) {
	in := restarted(child("w-0", "11", ownedByWidget), child("w-0", "21", orphaned))
	in.Target.EqualIgnore = []target.Path{target.MustParsePath("metadata.ownerReferences[*].uid")}

	fired(t, invariant.RestartStable, in)
}

const startedAt = "probe.example.com/started-at"

func annotations(values map[string]string) option {
	return func(u *unstructured.Unstructured) { u.SetAnnotations(values) }
}

func TestG5IgnoresAnAnnotationWhoseKeyHoldsADot(t *testing.T) {
	for _, stamped := range []struct {
		name          string
		before, after option
	}{
		{"again", annotations(map[string]string{startedAt: "1", "note": "a"}), annotations(map[string]string{startedAt: "2", "note": "a"})},
		{"only by the restart", nothing, annotations(map[string]string{startedAt: "2"})},
	} {
		t.Run(stamped.name, func(t *testing.T) {
			in := restarted(child("w-0", "11", stamped.before), child("w-0", "21", stamped.after))
			in.Target.EqualIgnore = []target.Path{target.MustParsePath(`metadata.annotations["` + startedAt + `"]`)}

			silent(t, invariant.RestartStable, in)
		})
	}
}

func TestG5ComparesTheAnnotationsTheTargetDoesNotIgnore(t *testing.T) {
	in := restarted(
		child("w-0", "11", annotations(map[string]string{startedAt: "1", "note": "a"})),
		child("w-0", "21", annotations(map[string]string{startedAt: "2", "note": "b"})))
	in.Target.EqualIgnore = []target.Path{target.MustParsePath(`metadata.annotations["` + startedAt + `"]`)}

	fired(t, invariant.RestartStable, in)
}

// conditions sets two conditions, each with the heartbeat and the message.
func conditions(heartbeat time.Duration, message string) option {
	return func(u *unstructured.Unstructured) {
		stamp := at(heartbeat).Format(time.RFC3339)
		u.Object["status"] = map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "True", "lastHeartbeatTime": stamp, "message": message},
			map[string]any{"type": "Synced", "status": "True", "lastHeartbeatTime": stamp, "message": message},
		}}
	}
}

func TestG5IgnoresAFieldOfEveryCondition(t *testing.T) {
	heartbeats := []target.Path{target.MustParsePath("status.conditions[*].lastHeartbeatTime")}

	t.Run("the ignored field", func(t *testing.T) {
		in := restarted(child("w-0", "11", conditions(0, "ok")), child("w-0", "21", conditions(12*time.Second, "ok")))
		in.Target.EqualIgnore = heartbeats

		if notes := silent(t, invariant.RestartStable, in).Notes; len(notes) > 0 {
			t.Errorf("G5 noted %v, want nothing.", notes)
		}
	})
	t.Run("another field", func(t *testing.T) {
		in := restarted(child("w-0", "11", conditions(0, "ok")), child("w-0", "21", conditions(12*time.Second, "retrying")))
		in.Target.EqualIgnore = heartbeats

		fired(t, invariant.RestartStable, in)
	})
}

func TestG5NotesAnIgnoredKeyThatMeetsAList(t *testing.T) {
	in := restarted(child("w-0", "11", conditions(0, "ok")), child("w-0", "21", conditions(12*time.Second, "ok")))
	in.Target.EqualIgnore = []target.Path{target.MustParsePath("status.conditions.lastHeartbeatTime")}

	result := evaluate(t, invariant.RestartStable, in)

	if len(result.Violations) != 1 {
		t.Errorf("G5 reported %v, want the heartbeat it could not ignore.", statements(result))
	}
	want := "G5 could not follow equalIgnore status.conditions.lastHeartbeatTime: status.conditions is a list; write status.conditions[*].lastHeartbeatTime"
	if len(result.Notes) != 1 || result.Notes[0] != want {
		t.Errorf("G5 noted %q, want only %q.", result.Notes, want)
	}
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

func TestG5ComparesTheMetadataSection6DoesNotIgnore(t *testing.T) {
	in := restarted(child("w-0", "11", data("0")), child("w-0", "21", data("0"), deleting(12*time.Second)))

	violation := fired(t, invariant.RestartStable, in)

	if !strings.Contains(violation.Statement, "w-0") {
		t.Errorf("The statement is %q, want it to name the ConfigMap the restart left terminating.", violation.Statement)
	}
}
