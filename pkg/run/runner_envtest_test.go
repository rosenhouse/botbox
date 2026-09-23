//go:build envtest

package run_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse/botbox/pkg/invariant"
	"github.com/rosenhouse/botbox/pkg/run"
)

// toySequence drives the toy through every op the Runner executes in M3.
const toySequence = `{
  "seed": 20260920,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.botbox/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 3}}},
    {"i": 1, "t": "update", "patch": {"spec": {"count": 5}}},
    {"i": 2, "t": "settle"},
    {"i": 3, "t": "restart"},
    {"i": 4, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
    {"i": 5, "t": "delete"}
  ]
}`

// oneCreate settles, so the run waits for a reaction from a target that has
// stopped.
const oneCreate = `{
  "seed": 1,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.botbox/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}}
  ]
}`

// checkpointState is what a check saw when it ran.
type checkpointState struct {
	op          int
	converged   bool
	managed     []string
	ready       int64
	widgetWatch int
}

// recordingChecker records what each checkpoint was given, and reports fail if
// it is set.
type recordingChecker struct {
	fail        *run.Violation
	checkpoints []checkpointState
}

func (c *recordingChecker) Check(in run.Input) (run.Findings, error) {
	last := in.Timeline.Checkpoints[len(in.Timeline.Checkpoints)-1]
	state := checkpointState{op: last.Op, converged: last.Converged}
	for _, managed := range in.Objects.Managed() {
		state.managed = append(state.managed, managed.Name)
	}
	for _, cr := range in.Objects.Current(in.Target.Primary) {
		state.ready, _, _ = unstructured.NestedInt64(cr.Object.Object, "status", "ready")
	}
	for _, request := range in.Requests {
		if request.Verb == "watch" && request.Resource == "widgets" {
			state.widgetWatch++
		}
	}
	c.checkpoints = append(c.checkpoints, state)
	if c.fail != nil {
		return run.Findings{Violations: []run.Violation{*c.fail}}, nil
	}
	return run.Findings{}, nil
}

func (c *recordingChecker) at(op int) (checkpointState, bool) {
	for _, state := range c.checkpoints {
		if state.op == op {
			return state, true
		}
	}
	return checkpointState{}, false
}

func (c *recordingChecker) ops() []int {
	var ops []int
	for _, state := range c.checkpoints {
		ops = append(ops, state.op)
	}
	return ops
}

func TestRunner(t *testing.T) {
	ctx := t.Context()
	binary := buildToy(t)
	testCluster := startCluster(t, loadTarget(t, binary).CRDs)

	t.Run("drives the toy through a sequence and checkpoints where the design says", func(t *testing.T) {
		toy := loadTarget(t, binary)
		check := &recordingChecker{}
		dir := t.TempDir()

		result, err := run.Run(ctx, toy, readSequence(t, toySequence), run.Options{
			Dir: dir, Config: testCluster.Config(), Check: check,
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation != nil {
			t.Errorf("The run reported %+v, want none: the toy runs without a bug.", result.Violation)
		}
		requireCheckpointsOfSection4(t, check, result)
		requireOpsTookEffect(t, check, result)
		requireNamespaceEmpty(t, ctx, testCluster.Config(), result.Timeline.Namespace)
		if len(result.Timeline.Forced) > 0 {
			t.Errorf("The teardown forced the finalizers off %v, want the toy to clear its own.", result.Timeline.Forced)
		}
	})

	t.Run("stops at the first violation and leaves the evidence in run-1", func(t *testing.T) {
		toy := loadTarget(t, binary)
		failed := run.Violation{ID: "P1", Statement: "status.ready never exceeds the ConfigMaps present"}
		check := &recordingChecker{fail: &failed}
		out, err := run.OpenOutput(t.TempDir(), 20260920, time.Now())
		if err != nil {
			t.Fatalf("Opening the output directory failed: %v", err)
		}

		result, err := run.Run(ctx, toy, readSequence(t, toySequence), run.Options{
			Dir: out.RunDir(1), Config: testCluster.Config(), Check: check,
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation == nil || result.Violation.ID != failed.ID {
			t.Fatalf("The run reported %+v, want the violation the check gave it.", result.Violation)
		}
		if len(check.checkpoints) != 1 {
			t.Errorf("The checks ran at %v, want the run to end at the first violation.", check.ops())
		}
		if len(result.Timeline.Ops) != 1 {
			t.Errorf("The run applied %d ops, want it to stop after the first violation.", len(result.Timeline.Ops))
		}
		requireRunFiles(t, out.RunDir(1))
		requireNamespaceEmpty(t, ctx, testCluster.Config(), result.Timeline.Namespace)
	})

	// b10.json scales the toy down right after a restart, before anything
	// settles, so botbox's update lies between the states G5 compares.
	t.Run("passes the toy without a bug on b10.json and notes the restart", func(t *testing.T) {
		toy := loadTarget(t, binary)
		sequence, err := run.ReadSequence(repoRoot + "/targets/toy-widget/sequences/b10.json")
		if err != nil {
			t.Fatalf("Reading the sequence failed: %v", err)
		}

		result, err := run.Run(ctx, toy, sequence, run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation != nil {
			t.Errorf("The run reported %s, want none: the toy runs without a bug.", result.Violation)
		}
		if !slices.ContainsFunc(result.Notes, func(note string) bool {
			return strings.HasPrefix(note, "G5") && strings.Contains(note, "for op 1 (restart): op 2 (update) ran")
		}) {
			t.Errorf("The run noted %q, want G5 to say the update of op 2 kept it from judging the restart of op 1.", result.Notes)
		}
	})

	// B8 never recreates the child botbox deleted until a restart does. P1
	// would end the run before the restart, so the target declares none.
	t.Run("names what the restart of b8.json brought back", func(t *testing.T) {
		toy := loadTarget(t, binary)
		toy.Launch.Args = append(toy.Launch.Args, "--bug=8")
		toy.Properties = nil
		sequence, err := run.ReadSequence(repoRoot + "/targets/toy-widget/sequences/b8.json")
		if err != nil {
			t.Fatalf("Reading the sequence failed: %v", err)
		}

		result, err := run.Run(ctx, toy, sequence, run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation == nil || result.Violation.ID != "G5" {
			t.Fatalf("The run reported %v, want G5.", result.Violation)
		}
		got := result.Violation.Differences
		if len(got) != 1 {
			t.Fatalf("G5 quoted %+v, want the ConfigMap the restart brought back.", got)
		}
		want := invariant.Difference{
			Object: "v1/ConfigMap widget-0", ResourceVersions: [2]string{"", got[0].ResourceVersions[1]},
			Before: "(absent)", After: "(present)",
		}
		if got[0] != want || want.ResourceVersions[1] == "" {
			t.Errorf("G5 quoted %+v, want %+v with the resourceVersion the restart created.", got[0], want)
		}
		if want := "the state converged after op 1 (deleteManaged) and the one after op 3 (settle)"; result.Violation.Compared != want {
			t.Errorf("G5 says it compared %q, want %q.", result.Violation.Compared, want)
		}
	})

	t.Run("notes an owner the collector cannot resolve", func(t *testing.T) {
		toy := loadTarget(t, binary)
		ownedBySecret := fixtureConfigMap()
		ownedBySecret.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "v1", Kind: "Secret", Name: "absent", UID: "8a1d0f2c-5b3e-4d7a-9c6f-1e2b3c4d5e6f",
		}})
		toy.Fixtures = append(toy.Fixtures, ownedBySecret)

		result, err := run.Run(ctx, toy, readSequence(t, oneCreate), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: &recordingChecker{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		want := "botbox's garbage collector never deletes v1/ConfigMap " + fixtureName +
			", because it does not watch v1/Secret, the kind of its owner absent"
		if !slices.Equal(result.Notes, []string{want}) {
			t.Errorf("The run carried the notes %q, want %q.", result.Notes, want)
		}
	})

	// A target that dies mid-run takes the run with it, and no wait outlives
	// it (DESIGN.md §5.5).
	t.Run("ends the settle wait where the target stopped", func(t *testing.T) {
		const ranFor = 2 * time.Second
		toy := loadTarget(t, diesAfter(t, ranFor, "toy-widget: bind: address already in use"))
		// A target that is gone never converges, so a wait that ignored its
		// exit would take all of T_settle.
		toy.Timeouts.Settle = 30 * time.Second
		toy.Timeouts.Stable = time.Second
		toy.Timeouts.Delete = time.Second

		result, err := run.Run(ctx, toy, readSequence(t, oneCreate), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: &recordingChecker{},
		})

		if err == nil {
			t.Fatal("The run reported no error although the target had stopped.")
		}
		for _, want := range []string{"op 0 (create)", "no longer running", "bind: address already in use"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("The run reported %q, which does not mention %q.", err, want)
			}
		}
		if result.Violation != nil {
			t.Errorf("The run reported %+v against the target; a target that stopped is the harness's failure.", result.Violation)
		}
		if len(result.Timeline.Ops) != 1 || result.Timeline.Ops[0].Settled == nil {
			t.Fatalf("The run recorded %+v, want the create and the wait that followed it.", result.Timeline.Ops)
		}
		window := result.Timeline.Ops[0].Settled.Window
		if waited := window.End.Sub(window.Start); waited > ranFor+5*time.Second {
			t.Errorf("The settle wait took %v, want it to end where the target stopped, %v in.", waited, ranFor)
		}
	})
}

// diesAfter is a target that runs for d, says why it is stopping and exits.
func diesAfter(t *testing.T, d time.Duration, says string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dies-after")
	script := fmt.Sprintf("#!/bin/sh\nsleep %v\necho %q >&2\nexit 1\n", d.Seconds(), says)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// requireCheckpointsOfSection4 asserts a checkpoint where each settle wait
// ended and one after the teardown deletion window. The restart of op 3 is
// checked at the settle that follows it.
func requireCheckpointsOfSection4(t *testing.T, check *recordingChecker, result run.Result) {
	t.Helper()
	want := []int{0, 1, 2, 4, 5, run.Teardown}
	if got := check.ops(); !slices.Equal(got, want) {
		t.Errorf("The checks ran after the ops %v, want %v.", got, want)
	}
	if got := checkpointOps(result.Timeline); !slices.Equal(got, want) {
		t.Errorf("The timeline holds checkpoints at %v, want %v.", got, want)
	}
	for _, state := range check.checkpoints {
		if !state.converged {
			t.Errorf("The checkpoint after op %d reports no convergence.", state.op)
		}
	}
}

// requireOpsTookEffect asserts what the cluster did: the update scaled the
// children, the restart started a second process, the deleteManaged took the
// first child and the delete emptied the namespace.
func requireOpsTookEffect(t *testing.T, check *recordingChecker, result run.Result) {
	t.Helper()
	created, _ := check.at(0)
	if len(created.managed) != 3 || created.ready != 3 {
		t.Errorf("After the create the run held the ConfigMaps %v and status.ready %d, want 3 of each.",
			created.managed, created.ready)
	}
	updated, _ := check.at(1)
	if len(updated.managed) != 5 || updated.ready != 5 {
		t.Errorf("After the update to count 5 the run held the ConfigMaps %v and status.ready %d, want 5 of each.",
			updated.managed, updated.ready)
	}
	restarted, _ := check.at(4)
	if before, _ := check.at(2); restarted.widgetWatch <= before.widgetWatch {
		t.Errorf("The target watched widgets %d times before the restart and %d after, want the restart to open another.",
			before.widgetWatch, restarted.widgetWatch)
	}
	if want := "widget-0"; result.Timeline.Ops[4].Resolved != want {
		t.Errorf("The deleteManaged op took %q, want the first ConfigMap by creationTimestamp then name, %q.",
			result.Timeline.Ops[4].Resolved, want)
	}
	if len(restarted.managed) != 5 {
		t.Errorf("After the deleted child the run held the ConfigMaps %v, want the target to have made it again.",
			restarted.managed)
	}
	deleted, _ := check.at(5)
	if len(deleted.managed) != 0 {
		t.Errorf("After the delete the run still held the ConfigMaps %v.", deleted.managed)
	}
}

// requireNamespaceEmpty asserts what the teardown left: a namespace holding
// none of the run's objects. envtest never finishes terminating one, so its
// contents stay readable (DESIGN.md §5.8).
func requireNamespaceEmpty(t *testing.T, ctx context.Context, config *rest.Config, namespace string) {
	t.Helper()
	if namespace == "" {
		t.Fatal("The run recorded no namespace.")
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("Building a client failed: %v", err)
	}
	remaining, err := client.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Listing what the run left failed: %v", err)
	}
	for _, configMap := range remaining.Items {
		if configMap.DeletionTimestamp == nil {
			t.Errorf("The run namespace still holds the ConfigMap %s.", configMap.Name)
		}
	}
}

// requireRunFiles asserts the run directory of DESIGN.md §11.
func requireRunFiles(t *testing.T, dir string) {
	t.Helper()
	if filepath.Base(dir) != "run-1" {
		t.Errorf("The failing run wrote to %s, want run-1.", dir)
	}
	for _, name := range []string{"sequence.json", "requests.jsonl", "objects.jsonl", "target.log"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("The failing run wrote no %s: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("The failing run's %s is empty.", name)
		}
	}
}

func readSequence(t *testing.T, text string) run.Sequence {
	t.Helper()
	sequence, err := run.UnmarshalSequence([]byte(text))
	if err != nil {
		t.Fatalf("Reading the sequence failed: %v", err)
	}
	return sequence
}

func checkpointOps(timeline run.Timeline) []int {
	var ops []int
	for _, checkpoint := range timeline.Checkpoints {
		ops = append(ops, checkpoint.Op)
	}
	return ops
}
