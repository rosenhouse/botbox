package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/launch"
	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/proxy"
	"github.com/rosenhouse/botbox/pkg/target"
)

// defaultMaxManaged is N_objects of DESIGN.md §5.5: beyond it the run ends as
// a harness limit rather than a finding.
const defaultMaxManaged = 500

// teardownMargin is what the teardown gets beyond its own waits, so that a run
// that reached its deadline still takes itself down.
const teardownMargin = 30 * time.Second

// Teardown is the Checkpoint.Op of the checkpoint after the teardown deletion
// window, which no op opened.
const Teardown = -1

// deletionMargin holds the run namespace open past T_delete, because G3's
// window opens when the Observer recorded the deletion, which is after the
// teardown asked for it.
const deletionMargin = time.Second

// Checker evaluates the invariants and the target's properties at a checkpoint
// (DESIGN.md §5.6). Its error is a configuration error, never a finding.
type Checker interface {
	Check(Input) (Findings, error)
}

// Findings are what the checks made of the run at one checkpoint.
type Findings struct {
	Violations []Violation
	// Notes name what a check could not judge (DESIGN.md §6).
	Notes []string
}

// Input is what a check reads: the proxy's request log, the Observer's object
// history, the target's declaration and what the run has done so far.
type Input struct {
	Target   *target.Target
	Requests []proxy.Request
	Objects  *observe.Store
	Timeline Timeline
}

// Violation is one failed invariant or property.
type Violation struct {
	// ID is G1..Gn or P1..Pn.
	ID string
	// Statement is what the check requires and the run broke.
	Statement string
	// Evidence quotes what the run did, for the report (DESIGN.md §5.7).
	Evidence string
}

// Result is one run's outcome. An error alongside it is a configuration or
// harness error, never a finding (DESIGN.md §11).
type Result struct {
	Timeline Timeline
	// Violation is the first violation the run found, or nil. A run ends at
	// its first violation.
	Violation *Violation
	// Notes name what the checks could not judge, as the last checkpoint read
	// the run (DESIGN.md §6).
	Notes []string
	// Recorded is the whole run as the checks read it, sampled once the
	// teardown is done. A run that ended early recorded what it reached.
	Recorded Input
}

// Timeline is what the run did, in order (DESIGN.md §4).
type Timeline struct {
	// Namespace is private to this run and never reused (DESIGN.md §5.5).
	Namespace   string
	Ops         []AppliedOp
	Checkpoints []Checkpoint
	// Quiet is the T_stable the teardown waits before it deletes anything,
	// which §5.5 step 4 makes the run's last quiet window. It closes where
	// Deletion opens.
	Quiet Window
	// Deletion is the window G3 judges: it opens when the teardown deletes the
	// primary CR and closes when the namespace is clean or the window expires.
	Deletion Window
	// Faults are the windows the Runner had a fault op's spec injected in. An
	// open window has no End: the fault outlived the run.
	Faults []Window
	// Forced names every object the teardown force-removed a finalizer from.
	// One such removal invalidates G3 for the run (DESIGN.md §5.5).
	Forced []string
}

// AppliedOp is one op the Runner applied.
type AppliedOp struct {
	Op Op
	At time.Time
	// Resolved is the object a deleteManaged op chose (DESIGN.md §7).
	Resolved string
	// Settled is the settle wait that followed the op, or nil if none did.
	Settled *Wait
}

// Wait is one settle wait's outcome.
type Wait struct {
	Window    Window
	Converged bool
}

// Checkpoint is where the checks ran (DESIGN.md §4).
type Checkpoint struct {
	At time.Time
	// Op is the op whose settle wait ended here, or Teardown.
	Op int
	// Converged is whether that wait converged. At the teardown's checkpoint
	// it is whether the namespace came clean within the deletion window.
	Converged bool
}

// Window is a stretch of a run's time.
type Window struct{ Start, End time.Time }

// Run executes one sequence against a fresh namespace (DESIGN.md §5.5) and
// evaluates opts.Check at each checkpoint. The proxy's fault sampling follows
// the sequence's seed, so a replay faults the same requests.
func Run(ctx context.Context, t *target.Target, sequence Sequence, opts Options) (Result, error) {
	if err := validateRun(t, sequence, opts); err != nil {
		return Result{}, fmt.Errorf("running the sequence: %w", err)
	}
	if err := writeRunSequence(opts.Dir, sequence); err != nil {
		return Result{}, err
	}
	opts.Seed = sequence.Seed
	h, err := Start(ctx, t, opts)
	if err != nil {
		return Result{}, err
	}
	live, err := newLiveRun(h, t)
	if err != nil {
		return Result{}, errors.Join(err, h.Stop(ctx))
	}
	return runSequence(ctx, t, sequence, opts, live)
}

func validateRun(t *target.Target, sequence Sequence, opts Options) error {
	if err := validate(t, opts); err != nil {
		return err
	}
	if opts.Check == nil {
		return errors.New("a checker is required: the Runner evaluates it at each checkpoint")
	}
	if sequence.Target != t.Name {
		return fmt.Errorf("the sequence is for the target %q, and this run's target is %q", sequence.Target, t.Name)
	}
	return sequence.Validate()
}

// writeRunSequence puts the sequence in the run directory, so that a failing
// run carries what it executed (DESIGN.md §11).
func writeRunSequence(dir string, sequence Sequence) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating the run directory: %w", err)
	}
	return WriteSequence(filepath.Join(dir, sequenceFile), sequence)
}

// harness is the live run the Runner drives. The unit tier fakes it; liveRun
// implements it over a started Harness.
type harness interface {
	namespace() string
	settle(ctx context.Context) (bool, error)
	sleep(ctx context.Context, d time.Duration) error
	restart(ctx context.Context) error
	setFaults(specs []proxy.FaultSpec)
	createCR(ctx context.Context, obj *unstructured.Unstructured) (string, error)
	patchCR(ctx context.Context, name string, patch map[string]any) error
	deleteCR(ctx context.Context, name string) error
	awaitCRGone(ctx context.Context, name string) error
	// managedObjects names the managed objects of one kind, ordered by
	// creationTimestamp then name (DESIGN.md §7).
	managedObjects(gvk schema.GroupVersionKind) []string
	deleteManaged(ctx context.Context, gvk schema.GroupVersionKind, name string) error
	managedCount() int
	// awaitClean waits for the run namespace to empty, and reports whether it
	// did within the window.
	awaitClean(ctx context.Context, within time.Duration) (bool, error)
	// forceFinalizers removes every finalizer left in the run namespace and
	// names what it took them from.
	forceFinalizers(ctx context.Context) ([]string, error)
	empty(ctx context.Context) error
	requests() []proxy.Request
	objects() *observe.Store
	// targetStatus reports whether the target is still running, and why it
	// stopped if it is not.
	targetStatus() launch.Status
	stop(ctx context.Context) error
}

// runner executes one sequence. It always tears the run down.
type runner struct {
	target   *target.Target
	sequence Sequence
	check    Checker
	h        harness
	limit    int
	dir      string
	now      func() time.Time

	timeline  Timeline
	violation *Violation
	notes     []string
	failed    bool
	// cr is the primary CR the CR ops act on.
	cr     string
	faults []activeFault
}

// activeFault is a fault op's spec while it is injected.
type activeFault struct {
	spec proxy.FaultSpec
	// until is the op index the fault ends at, or nil if only the proxy's own
	// trigger ends it (DESIGN.md §5.2).
	until *int
	// window is the fault's place in Timeline.Faults, which closes when the
	// Runner clears it.
	window int
}

func runSequence(ctx context.Context, t *target.Target, sequence Sequence, opts Options, h harness) (Result, error) {
	r := &runner{
		target:   t,
		sequence: sequence,
		check:    opts.Check,
		h:        h,
		limit:    opts.maxManaged(),
		dir:      opts.Dir,
		now:      time.Now,
		timeline: Timeline{Namespace: h.namespace()},
	}
	failure := r.applyOps(ctx)
	r.failed = failure != nil
	teardown := r.teardown(ctx)
	result := Result{Timeline: r.timeline, Violation: r.violation, Notes: r.notes, Recorded: r.input()}
	return result, errors.Join(failure, teardown)
}

// applyOps applies the sequence in order and stops at the first violation
// (DESIGN.md §5.5).
func (r *runner) applyOps(ctx context.Context) error {
	for _, op := range r.sequence.Ops {
		if r.violation != nil {
			return nil
		}
		r.expireFaults(op.Index)
		applied, err := r.apply(ctx, op)
		r.timeline.Ops = append(r.timeline.Ops, applied)
		if err == nil && op.settles() {
			err = r.settle(ctx, op)
		}
		if err != nil {
			return fmt.Errorf("op %d (%s): %w", op.Index, op.Type, err)
		}
	}
	return nil
}

func (r *runner) apply(ctx context.Context, op Op) (AppliedOp, error) {
	applied := AppliedOp{Op: op, At: r.now()}
	switch op.Type {
	case OpCreate:
		return applied, r.create(ctx, op)
	case OpUpdate:
		if err := r.haveCR(); err != nil {
			return applied, err
		}
		return applied, r.h.patchCR(ctx, r.cr, op.Patch)
	case OpDelete:
		if err := r.haveCR(); err != nil {
			return applied, err
		}
		return applied, r.h.deleteCR(ctx, r.cr)
	case OpRecreate:
		return applied, r.recreate(ctx, op)
	case OpRestart:
		return applied, r.h.restart(ctx)
	case OpFault:
		r.inject(op)
		return applied, nil
	case OpDeleteManaged:
		name, err := r.applyDeleteManaged(ctx, op)
		applied.Resolved = name
		return applied, err
	case OpSettle:
		return applied, nil
	}
	return applied, fmt.Errorf("%q is not an op type", op.Type)
}

func (r *runner) create(ctx context.Context, op Op) error {
	name, err := r.h.createCR(ctx, op.Obj)
	if err != nil {
		return err
	}
	r.cr = name
	return nil
}

// recreate deletes the CR, waits for it to disappear and creates the op's
// object (DESIGN.md §7).
func (r *runner) recreate(ctx context.Context, op Op) error {
	if err := r.haveCR(); err != nil {
		return err
	}
	if err := r.h.deleteCR(ctx, r.cr); err != nil {
		return err
	}
	if err := r.h.awaitCRGone(ctx, r.cr); err != nil {
		return err
	}
	return r.create(ctx, op)
}

func (r *runner) haveCR() error {
	if r.cr == "" {
		return errors.New("no CR has been created yet")
	}
	return nil
}

// applyDeleteManaged resolves the op's index against the managed objects and
// deletes the one it names, behind the target's back (DESIGN.md §5.4).
func (r *runner) applyDeleteManaged(ctx context.Context, op Op) (string, error) {
	gvk, err := managedKind(r.target, op.Kind)
	if err != nil {
		return "", err
	}
	names := r.h.managedObjects(gvk)
	if *op.Nth >= len(names) {
		return "", fmt.Errorf("index %d resolves to nothing: the run holds %d managed %s", *op.Nth, len(names), op.Kind)
	}
	name := names[*op.Nth]
	return name, r.h.deleteManaged(ctx, gvk, name)
}

// settle waits for the target's reaction and checkpoints where the wait ends
// (DESIGN.md §4). A wait that expires while no fault is active is a G4
// violation, which ends the run.
func (r *runner) settle(ctx context.Context, op Op) error {
	wait := Wait{Window: Window{Start: r.now()}}
	converged, err := r.h.settle(ctx)
	if err != nil {
		return err
	}
	wait.Window.End, wait.Converged = r.now(), converged
	r.timeline.Ops[len(r.timeline.Ops)-1].Settled = &wait
	if !converged {
		// A target that is gone cannot converge, so that is the harness's
		// failure to report, not the target's to answer for.
		if status := r.h.targetStatus(); !status.Running {
			return fmt.Errorf("the target is no longer running: %v; its output is in %s",
				status.Exit, filepath.Join(r.dir, targetLogFile))
		}
		if !r.faultActive() {
			r.violate(Violation{
				ID:        "G4",
				Statement: "the target's Ready predicate holds within T_settle after a spec change",
				Evidence: fmt.Sprintf("the settle wait after op %d (%s) expired after %v with no fault active",
					op.Index, op.Type, r.target.Timeouts.Settle),
			})
		}
	}
	return r.checkpoint(op.Index, converged)
}

// checkpoint evaluates the checks and keeps the first violation.
func (r *runner) checkpoint(op int, converged bool) error {
	if count := r.h.managedCount(); count > r.limit {
		return fmt.Errorf("the run namespace holds %d managed objects, over the harness limit of %d", count, r.limit)
	}
	r.timeline.Checkpoints = append(r.timeline.Checkpoints, Checkpoint{At: r.now(), Op: op, Converged: converged})
	found, err := r.check.Check(r.input())
	if err != nil {
		return fmt.Errorf("evaluating the checks: %w", err)
	}
	r.notes = found.Notes
	for _, violation := range found.Violations {
		r.violate(violation)
		break
	}
	return nil
}

func (r *runner) input() Input {
	return Input{Target: r.target, Requests: r.h.requests(), Objects: r.h.objects(), Timeline: r.timeline}
}

func (r *runner) violate(violation Violation) {
	if r.violation == nil {
		r.violation = &violation
	}
}

// inject adds the op's fault to those the proxy applies and opens its window.
func (r *runner) inject(op Op) {
	r.timeline.Faults = append(r.timeline.Faults, Window{Start: r.now()})
	r.faults = append(r.faults, activeFault{
		spec:   op.Fault.spec(),
		until:  op.Fault.Until.Op,
		window: len(r.timeline.Faults) - 1,
	})
	r.setFaults()
}

// expireFaults drops the faults whose until trigger names this op or an
// earlier one (DESIGN.md §5.2).
func (r *runner) expireFaults(op int) {
	kept := make([]activeFault, 0, len(r.faults))
	for _, fault := range r.faults {
		if fault.until == nil || *fault.until > op {
			kept = append(kept, fault)
			continue
		}
		r.timeline.Faults[fault.window].End = r.now()
	}
	if len(kept) == len(r.faults) {
		return
	}
	r.faults = kept
	r.setFaults()
}

func (r *runner) setFaults() {
	specs := make([]proxy.FaultSpec, len(r.faults))
	for i, fault := range r.faults {
		specs[i] = fault.spec
	}
	r.h.setFaults(specs)
}

func (r *runner) faultActive() bool { return len(r.faults) > 0 }

// clearFaults takes every fault off the proxy and closes its window, which the
// teardown does before it measures anything (DESIGN.md §5.5).
func (r *runner) clearFaults() {
	for _, fault := range r.faults {
		r.timeline.Faults[fault.window].End = r.now()
	}
	r.faults = nil
	r.h.setFaults(nil)
}

// teardown is step 4 of DESIGN.md §5.5. Every step runs even if one fails, and
// the caller's deadline does not cut it short.
func (r *runner) teardown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.teardownBudget())
	defer cancel()

	r.clearFaults()
	r.timeline.Quiet.Start = r.now()
	failures := []error{r.h.sleep(ctx, r.target.Timeouts.Stable)}

	// Stamped before the delete, not after it: from here on botbox is the one
	// changing the namespace, and no invariant window reaches past this instant
	// (§6). The T_stable sleep above is the last quiet window, and still the
	// target's to answer for.
	r.timeline.Quiet.End = r.now()
	r.timeline.Deletion.Start = r.timeline.Quiet.End
	if r.cr != "" {
		failures = append(failures, r.h.deleteCR(ctx, r.cr))
	}
	clean, err := r.h.awaitClean(ctx, r.target.Timeouts.Delete+deletionMargin)
	r.timeline.Deletion.End = r.now()
	failures = append(failures, err)
	if r.violation == nil && !r.failed {
		failures = append(failures, r.checkpoint(Teardown, clean))
	}

	forced, err := r.h.forceFinalizers(ctx)
	r.timeline.Forced = forced
	failures = append(failures, err, r.h.empty(ctx), r.h.stop(ctx))
	return errors.Join(failures...)
}

func (r *runner) teardownBudget() time.Duration {
	return r.target.Timeouts.Stable + r.target.Timeouts.Delete + teardownMargin
}

// spec is the fault in the form the proxy injects (DESIGN.md §5.2).
func (f Fault) spec() proxy.FaultSpec {
	return proxy.FaultSpec{
		Match: proxy.RequestMatcher{
			Verb:     f.Match.Verb,
			Resource: f.Match.Resource,
			Name:     f.Match.Name,
			Fraction: f.Match.Fraction,
		},
		Action: f.Action.action(),
		Until:  proxy.Trigger{Count: f.Until.Count, For: time.Duration(f.Until.For)},
	}
}

func (a Action) action() proxy.FaultAction {
	switch {
	case a.Error != 0:
		return proxy.Error{Code: a.Error}
	case a.Delay != 0:
		return proxy.Delay{For: time.Duration(a.Delay)}
	default:
		return proxy.Drop{}
	}
}
