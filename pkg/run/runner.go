package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/cluster"
	"github.com/rosenhouse/botbox/pkg/invariant"
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

// Teardown and Recovery are the Checkpoint.Op of the checkpoints no op opened.
const (
	Teardown = invariant.Teardown
	Recovery = invariant.Recovery
)

// maxTail is how much of the target's output the harness reads to quote its
// last line.
const maxTail = 4096

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
	// At is the instant the check judged, which aligns the evidence below
	// (DESIGN.md §5.7).
	At time.Time
	// Evidence quotes what the run did, in one line.
	Evidence string
	// Requests and Versions are the evidence the violation named, which a
	// report quotes (DESIGN.md §5.7).
	Requests []proxy.Request
	Versions []observe.Version
	// RequestsTotal and VersionsTotal are what the check chose each excerpt
	// from, and VersionsOf names the object the timeline is the history of
	// (pkg/invariant).
	RequestsTotal, VersionsTotal int
	VersionsOf                   string
	// Managed is the state at the violation, one version per object the target
	// managed, and ManagedTotal how many there were. A check that did not ask
	// leaves the total nil (DESIGN.md §5.7, D39).
	Managed      []observe.Version
	ManagedTotal *int
	// Ready is what a readiness verdict read of the predicate and the CR.
	Ready *invariant.Readiness
}

// String is the violation in one line. A message that prints one wants the
// finding, not the evidence behind it.
func (v Violation) String() string {
	if v.Evidence == "" {
		return v.ID + " " + v.Statement
	}
	return fmt.Sprintf("%s %s; %s", v.ID, v.Statement, v.Evidence)
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
	// Recovery is the settle wait the teardown gave a target still owed time
	// to recover from the faults, or nil if it owed none.
	Recovery *Wait
	// Quiet is the T_stable the teardown waits before it deletes anything,
	// which §5.5 step 4 makes the run's last quiet window. It closes where
	// Deletion opens.
	Quiet Window
	// Deletion is the window G3 judges: it opens when the teardown deletes the
	// primary CR and closes when the namespace is clean or the window expires.
	Deletion Window
	// Cleaned is when the teardown saw the run namespace empty, or zero if it
	// never did (DESIGN.md §6, D34).
	Cleaned time.Time
	// Faults are the windows the proxy applied a fault op's spec in, one per
	// fault op. A window with no Start is a fault that matched no request,
	// which changed nothing and excuses nothing (D36). An open window has no
	// End: the fault outlived the run.
	Faults []Window
	// Forced names every object the teardown force-removed a finalizer from.
	// The run notes each one: G3 judged the deletion window, which closed
	// before any of this (DESIGN.md §5.5, D37).
	Forced []string
}

// AppliedOp is one op the Runner applied.
type AppliedOp struct {
	Op Op
	At time.Time
	// CR is the primary CR a CR op wrote.
	CR string
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
	// At is where the settle wait ended, or where the teardown's deletion
	// window closed.
	At time.Time
	// Began is where that settle wait began. The teardown's is zero.
	Began time.Time
	// Op is the op whose settle wait ended here, Recovery, or Teardown.
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
	if err := WriteRunSequence(opts.Dir, sequence); err != nil {
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

// WriteRunSequence puts the sequence in the run directory, so that a failing
// run carries what it executed (DESIGN.md §11).
func WriteRunSequence(dir string, sequence Sequence) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating the run directory: %w", err)
	}
	return WriteSequence(filepath.Join(dir, sequenceFile), sequence)
}

// harness is the live run the Runner drives. The unit tier fakes it; liveRun
// implements it over a started Harness.
type harness interface {
	namespace() string
	// settle waits for the target to converge, past T_settle while owed
	// returns a later instant.
	settle(ctx context.Context, owed func() time.Time) (bool, error)
	sleep(ctx context.Context, d time.Duration) error
	restart(ctx context.Context) error
	setFaults(specs []proxy.FaultSpec)
	// faultWindows is what the proxy has done with each spec setFaults was
	// last given, in that order.
	faultWindows() []proxy.FaultWindow
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
	// unresolvedOwners names each owner the collector could not resolve. It
	// is complete once stop has returned.
	unresolvedOwners() []cluster.Unresolved
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
	// skipped names what no check judged: what the run could not do, and what
	// botbox did to the run namespace itself (DESIGN.md §6).
	skipped []string
	failed  bool
	// cr is the primary CR the CR ops act on.
	cr     string
	faults []activeFault
}

// activeFault is a fault op's spec while the proxy holds it. The Runner's
// faults are exactly the specs it last gave the proxy, in that order, so that
// the proxy's windows line up with them.
type activeFault struct {
	spec proxy.FaultSpec
	// until is the op index the fault ends at, or nil if only the proxy's own
	// trigger ends it (DESIGN.md §5.2).
	until *int
	// window is the fault's place in Timeline.Faults.
	window int
	// applied is whether the proxy has faulted a request with it, and retired
	// is whether the proxy has stopped applying it.
	applied, retired bool
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
	// The run's own notes come before the last checkpoint's.
	notes := slices.Concat(r.skipped, r.notes)
	result := Result{Timeline: r.timeline, Violation: r.violation, Notes: notes, Recorded: r.input()}
	return result, errors.Join(failure, teardown)
}

// applyOps applies the sequence in order and stops at the first violation
// (DESIGN.md §5.5).
func (r *runner) applyOps(ctx context.Context) error {
	for _, op := range r.sequence.Ops {
		if r.violation != nil {
			return nil
		}
		if err := r.applyOp(ctx, op); err != nil {
			return fmt.Errorf("op %d (%s): %w", op.Index, op.Type, err)
		}
	}
	return nil
}

// applyOp applies one op and waits for the target's reaction. A target that
// has stopped ends the run here, because the op would otherwise be applied to
// nothing (DESIGN.md §5.5).
func (r *runner) applyOp(ctx context.Context, op Op) error {
	if status := r.h.targetStatus(); !status.Running {
		return r.targetStopped(status)
	}
	r.expireFaults(op.Index)
	applied, err := r.apply(ctx, op)
	if op.Type.OnCR() {
		applied.CR = r.cr
	}
	r.timeline.Ops = append(r.timeline.Ops, applied)
	if err != nil {
		return err
	}
	if op.Settles() {
		return r.settle(ctx, op)
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
// deletes the one it names, behind the target's back (DESIGN.md §5.4). How
// many objects the target manages is its own doing, so an index that resolves
// to nothing skips the op and is reported as a note. A kind the target does
// not manage is still a configuration error: no run of that sequence can
// resolve it.
func (r *runner) applyDeleteManaged(ctx context.Context, op Op) (string, error) {
	gvk, err := managedKind(r.target, op.Kind)
	if err != nil {
		return "", err
	}
	names := r.h.managedObjects(gvk)
	if *op.Nth >= len(names) {
		r.skipped = append(r.skipped, fmt.Sprintf("op %d (deleteManaged) deleted nothing: index %d resolves to nothing, and the run holds %d managed %s",
			op.Index, *op.Nth, len(names), op.Kind))
		return "", nil
	}
	name := names[*op.Nth]
	return name, r.h.deleteManaged(ctx, gvk, name)
}

// settle waits for the target's reaction and checkpoints where the wait ends
// (DESIGN.md §4).
func (r *runner) settle(ctx context.Context, op Op) error {
	wait, err := r.wait(ctx)
	if err != nil {
		return err
	}
	r.timeline.Ops[len(r.timeline.Ops)-1].Settled = &wait
	excused := r.runSoFar().Recovering(wait.Window.End)
	return r.judge(op.Index, wait, excused)
}

// wait waits up to T_settle for the target to converge, or longer while it is
// owed time to recover from the faults.
func (r *runner) wait(ctx context.Context) (Wait, error) {
	wait := Wait{Window: Window{Start: r.now()}}
	converged, err := r.h.settle(ctx, r.owed)
	wait.Window.End, wait.Converged = r.now(), converged
	return wait, err
}

// owed is when the target must have recovered from the faults by, as the
// checks judge it.
func (r *runner) owed() time.Time { return r.runSoFar().Owed(r.now()) }

// runSoFar is the run's faults and settle waits, as the checks read them.
func (r *runner) runSoFar() invariant.Input {
	r.readFaultWindows()
	return invariant.Input{
		Target:      r.target,
		Checkpoints: engineCheckpoints(r.timeline.Checkpoints),
		Faults:      engineFaults(r.timeline.Faults),
	}
}

// judge checkpoints where a settle wait ended. A wait that expired where the
// faults did not excuse it is a G4 violation, which ends the run.
func (r *runner) judge(op int, wait Wait, excused bool) error {
	if !wait.Converged {
		// A target that is gone cannot converge, so that is the harness's
		// failure to report, not the target's to answer for.
		if status := r.h.targetStatus(); !status.Running {
			return r.targetStopped(status)
		}
	}
	return r.checkpoint(Checkpoint{At: wait.Window.End, Began: wait.Window.Start, Op: op, Converged: wait.Converged},
		!wait.Converged && !excused)
}

// targetStopped is the harness error for a target that is no longer running.
// A target that rejects its own flags writes one line and exits, and that line
// is what its reader acts on.
func (r *runner) targetStopped(status launch.Status) error {
	log := filepath.Join(r.dir, targetLogFile)
	stopped := "the target is no longer running"
	// The launcher knows no exit where it holds no process at all.
	if status.Exit != nil {
		stopped += ": " + status.Exit.Error()
	}
	if said := whyItStopped(log); said != "" {
		return fmt.Errorf("%s; it wrote %q, and the rest of its output is in %s", stopped, said, log)
	}
	return fmt.Errorf("%s; its output is in %s", stopped, log)
}

// panicked opens the report a Go runtime writes on the way out. The lines
// after it are the stack, so the last line of such a log is a frame.
var panicked = []string{"panic: ", "fatal error: "}

// whyItStopped is what the target said as it stopped: the line a panic opens
// with, or the last whole line it wrote. It is empty where the log holds
// neither, which leaves the reader the file itself.
func whyItStopped(path string) string {
	said := tailLines(path)
	for _, line := range said {
		if slices.ContainsFunc(panicked, func(opener string) bool { return strings.HasPrefix(line, opener) }) {
			return line
		}
	}
	if len(said) == 0 {
		return ""
	}
	return said[len(said)-1]
}

// tailLines are the whole lines at the end of the file, trimmed, innermost
// last. A line longer than maxTail has no whole form to quote, and a file
// botbox cannot read has nothing.
func tailLines(path string) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil
	}
	tail := make([]byte, min(info.Size(), maxTail))
	if _, err := file.ReadAt(tail, info.Size()-int64(len(tail))); err != nil {
		return nil
	}
	body := strings.TrimRight(string(tail), "\n")
	// A tail shorter than the file begins mid-line, so its first line is a
	// fragment: klog puts the level and the message at the front.
	if int64(len(tail)) < info.Size() {
		cut := strings.IndexByte(body, '\n')
		if cut < 0 {
			return nil
		}
		body = body[cut+1:]
	}
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

// checkpoint evaluates the checks and keeps the first violation. The G4 of a
// wait that expired unexcused comes first.
func (r *runner) checkpoint(checkpoint Checkpoint, expired bool) error {
	if count := r.h.managedCount(); count > r.limit {
		return fmt.Errorf("the run namespace holds %d managed objects, over the harness limit of %d", count, r.limit)
	}
	r.timeline.Checkpoints = append(r.timeline.Checkpoints, checkpoint)
	if expired {
		violation, err := engineInput(r.input()).ExpiredWait(engineCheckpoint(checkpoint))
		if err != nil {
			return err
		}
		r.violate(fromEngine(violation))
	}
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

// inject adds the op's fault to those the proxy applies. Its window opens
// where the proxy first applies it, which may be never (D36).
func (r *runner) inject(op Op) {
	r.timeline.Faults = append(r.timeline.Faults, Window{})
	r.faults = append(r.faults, activeFault{
		spec:   op.Fault.spec(),
		until:  op.Fault.Until.Op,
		window: len(r.timeline.Faults) - 1,
	})
	r.setFaults()
}

// expireFaults drops the faults whose until trigger names this op or an
// earlier one, and the ones the proxy has finished with (DESIGN.md §5.2).
func (r *runner) expireFaults(op int) {
	r.readFaultWindows()
	kept := make([]activeFault, 0, len(r.faults))
	for _, fault := range r.faults {
		switch {
		case fault.retired: // The proxy is done with it, and its window is closed.
		case fault.until != nil && *fault.until <= op:
			r.timeline.Faults[fault.window].End = r.now()
		default:
			kept = append(kept, fault)
		}
	}
	if len(kept) == len(r.faults) {
		return
	}
	r.faults = kept
	r.setFaults()
}

// readFaultWindows writes what the proxy has done with each fault into the
// timeline: a window opens where the proxy first applied the fault and closes
// where the proxy stopped applying it (D36). A fault the proxy never applied
// leaves its window unopened, because the run then ran as if the fault op
// were not there.
func (r *runner) readFaultWindows() {
	windows := r.h.faultWindows()
	for i := range min(len(r.faults), len(windows)) {
		fault, window := &r.faults[i], &r.timeline.Faults[r.faults[i].window]
		if !windows[i].First.IsZero() {
			fault.applied, window.Start = true, windows[i].First
		}
		if !windows[i].Retired.IsZero() {
			fault.retired, window.End = true, windows[i].Retired
		}
	}
}

func (r *runner) setFaults() {
	specs := make([]proxy.FaultSpec, len(r.faults))
	for i, fault := range r.faults {
		specs[i] = fault.spec
	}
	r.h.setFaults(specs)
}

// clearFaults takes every fault off the proxy and closes its window, which the
// teardown does before it measures anything (DESIGN.md §5.5).
func (r *runner) clearFaults() {
	r.readFaultWindows()
	for _, fault := range r.faults {
		if fault.applied && !fault.retired {
			r.timeline.Faults[fault.window].End = r.now()
		}
	}
	r.faults = nil
	r.h.setFaults(nil)
}

// awaitRecovery waits for a target still owed time to recover from the faults.
func (r *runner) awaitRecovery(ctx context.Context) error {
	if r.violation != nil || r.failed || !r.runSoFar().Recovering(r.now()) {
		return nil
	}
	wait, err := r.wait(ctx)
	if err == nil {
		r.timeline.Recovery = &wait
		// The faults are cleared, and the wait ran until the time they left
		// the target was up, so nothing excuses it.
		err = r.judge(Recovery, wait, false)
	}
	if err != nil {
		r.failed = true
		return fmt.Errorf("the settle wait after the last fault stopped: %w", err)
	}
	return nil
}

// teardown is step 4 of DESIGN.md §5.5. Every step runs even if one fails.
// The caller's deadline can end the recovery, which is judged as an op's wait
// is, but no step after it.
func (r *runner) teardown(ctx context.Context) error {
	r.clearFaults()
	failures := []error{r.awaitRecovery(ctx)}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.teardownBudget())
	defer cancel()
	r.timeline.Quiet.Start = r.now()
	failures = append(failures, r.h.sleep(ctx, r.target.Timeouts.Stable))

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
	if clean {
		r.timeline.Cleaned = r.timeline.Deletion.End
	}
	failures = append(failures, err)
	if r.violation == nil && !r.failed {
		failures = append(failures, r.teardownCheckpoint(clean))
	}

	forced, err := r.h.forceFinalizers(ctx)
	r.timeline.Forced = forced
	if len(forced) > 0 {
		// G3's window closed before this, so nothing forced here was ever
		// credited to the target. What a reader would otherwise miss is that
		// the namespace did not empty on its own (D37).
		r.skipped = append(r.skipped, fmt.Sprintf("the teardown force-removed the finalizers of %s, so the run namespace did not empty on its own",
			strings.Join(forced, ", ")))
	}
	failures = append(failures, err, r.h.empty(ctx), r.h.stop(ctx))
	for _, owner := range r.h.unresolvedOwners() {
		r.skipped = append(r.skipped, unresolvedNote(owner))
	}
	return errors.Join(failures...)
}

// unresolvedNote says why the collector kept an object: it counts an owner it
// cannot resolve as live.
func unresolvedNote(u cluster.Unresolved) string {
	why := "it does not watch " + kindName(u.OwnerKind)
	if u.Unserved {
		why = "the API server does not serve " + kindName(u.OwnerKind)
	}
	return fmt.Sprintf("botbox's garbage collector never deletes %s %s, because %s, the kind of its owner %s",
		kindName(u.DependentKind), u.DependentName, why, u.OwnerName)
}

// teardownCheckpoint judges the deletion window, unless the target stopped: a
// target that is gone cleaned nothing up (DESIGN.md §5.5).
func (r *runner) teardownCheckpoint(clean bool) error {
	if status := r.h.targetStatus(); !status.Running {
		return fmt.Errorf("the teardown: %w", r.targetStopped(status))
	}
	return r.checkpoint(Checkpoint{At: r.now(), Op: Teardown, Converged: clean}, false)
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
