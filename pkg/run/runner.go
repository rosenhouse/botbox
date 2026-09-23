package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// maxTail is how much of the target's output the harness reads to quote what
// it said as it stopped. A panic's stack follows the line it opens with.
const maxTail = 1 << 20

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
}

// quotingRequests carries an excerpt of the request log into the violation
// with the number it was chosen from, so that the two cannot disagree.
func (v Violation) quotingRequests(e invariant.Excerpt[proxy.Request]) Violation {
	v.Requests, v.RequestsTotal = e.Quoted, e.Total
	return v
}

// quotingVersions does the same for a timeline of object versions.
func (v Violation) quotingVersions(e invariant.Excerpt[observe.Version]) Violation {
	v.Versions, v.VersionsTotal, v.VersionsOf = e.Quoted, e.Total, e.Of
	return v
}

// quotingManaged does the same for the state at the verdict, whose total the
// check always knows: it asked.
func (v Violation) quotingManaged(e invariant.Excerpt[observe.Version]) Violation {
	v.Managed, v.ManagedTotal = e.Quoted, &e.Total
	return v
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
	// Faults are the windows the proxy applied each fault op's fault in, one
	// per fault op. A window with no Start is a fault the proxy applied to no
	// request, which changed nothing and excuses nothing (D36). A window with
	// no End is a fault the proxy still applies.
	Faults []Window
	// Forced names every object the teardown force-removed a finalizer from.
	// The run notes each one: G3 judged the deletion window, which closed
	// before any of this (DESIGN.md §5.5, D37).
	Forced []string
	// Exits are the times the target stopped on its own after the first
	// settle wait converged.
	Exits []Exit
}

// Exit is one time the target stopped on its own.
type Exit struct {
	At  time.Time
	Err error
	// Said is what the target wrote as it stopped, or empty.
	Said string
}

func (e Exit) String() string {
	if e.Said == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("%v after writing %q", e.Err, e.Said)
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
	// addFault has the proxy apply the fault after those it holds.
	addFault(spec proxy.FaultSpec) proxy.FaultID
	removeFault(id proxy.FaultID)
	clearFaults()
	// faultWindow is what the proxy has done with the fault.
	faultWindow(id proxy.FaultID) proxy.FaultWindow
	// servedResources is what the API server serves now, which includes the
	// CRDs a target installed.
	servedResources() ([]metav1.APIResource, error)
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
	// supervise restarts the target from now on whenever it exits, and
	// records each exit in exits.
	supervise()
	exits() []Exit
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
	// converged is where the last settle wait that converged ended. The first
	// shows the target works, so botbox supervises it from there on.
	converged time.Time
	// teardownStart is where the teardown began.
	teardownStart time.Time
	// cr is the primary CR the CR ops act on.
	cr     string
	faults []heldFault
	// faultOps is the op index of each of Timeline.Faults.
	faultOps []int
}

// heldFault is a fault op's fault. The Runner holds it to read its window from
// the proxy.
type heldFault struct {
	id proxy.FaultID
	// until is the op index the fault ends at, or nil if only the proxy's own
	// trigger ends it (DESIGN.md §5.2).
	until *int
	// window is the fault's place in Timeline.Faults.
	window int
	// retired is whether the proxy has stopped applying it.
	retired bool
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
	r.timeline.Exits = h.exits()
	// The run's own notes come before the last checkpoint's.
	notes := slices.Concat(r.exitNotes(), r.skipped, r.notes)
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
		if err := r.checkResource(op.Fault.Match.Resource); err != nil {
			return applied, err
		}
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
	excused := r.faultsAndWaits().Recovering(wait.Window.End)
	return r.judge(op.Index, fmt.Sprintf("op %d (%s)", op.Index, op.Type), wait, excused)
}

// wait waits up to T_settle for the target to converge, or longer while it is
// owed time to recover from the faults.
func (r *runner) wait(ctx context.Context) (Wait, error) {
	wait := Wait{Window: Window{Start: r.now()}}
	converged, err := r.h.settle(ctx, r.owed)
	wait.Window.End, wait.Converged = r.now(), converged
	if converged {
		if r.converged.IsZero() {
			r.h.supervise()
		}
		r.converged = wait.Window.End
	}
	return wait, err
}

// owed is when the target must have recovered from the faults by, as the
// checks judge it.
func (r *runner) owed() time.Time { return r.faultsAndWaits().Owed(r.now()) }

// faultsAndWaits is what the checks read of the run's faults and settle waits.
func (r *runner) faultsAndWaits() invariant.Input {
	r.readFaultWindows()
	return invariant.Input{
		Target:      r.target,
		Checkpoints: engineCheckpoints(r.timeline.Checkpoints),
		Faults:      engineFaults(r.timeline.Faults),
	}
}

// judge checkpoints where a settle wait ended. A wait that expired where the
// faults did not excuse it is a G4 violation, which ends the run.
func (r *runner) judge(op int, after string, wait Wait, excused bool) error {
	if !wait.Converged {
		// A target that is gone cannot converge, so that is the harness's
		// failure to report, not the target's to answer for.
		if status := r.h.targetStatus(); !status.Running {
			return r.targetStopped(status)
		}
		if !excused {
			r.violate(r.expired(after, wait))
		}
	}
	return r.checkpoint(op, wait.Converged)
}

// expired is the G4 of a settle wait that ran out with no fault to excuse it.
func (r *runner) expired(after string, wait Wait) Violation {
	// One read of the managed objects answers both, so that the count cannot
	// disagree with the state quoted beside it.
	managed := r.h.objects().Managed()
	cr := observe.Key{GVK: r.target.Primary, Namespace: r.timeline.Namespace, Name: r.cr}
	evidence := fmt.Sprintf("in %v the target never held its Ready predicate with %v of quiet behind it; %s",
		wait.Window.End.Sub(wait.Window.Start).Round(time.Millisecond), r.target.Timeouts.Stable,
		managedClause(len(managed)))
	exits := slices.DeleteFunc(r.h.exits(), func(exit Exit) bool { return exit.At.Before(r.converged) })
	if len(exits) > 0 {
		evidence += fmt.Sprintf("; the target exited %s since it last converged, last with %v",
			count(len(exits), "time"), exits[len(exits)-1])
	}
	return Violation{
		ID:        "G4",
		Statement: fmt.Sprintf("the settle wait after %s expired with no fault active", after),
		At:        wait.Window.End,
		Evidence:  evidence,
	}.quotingRequests(invariant.Recent(r.h.requests())).
		quotingVersions(invariant.RecentHistory(cr, r.h.objects().History(cr))).
		quotingManaged(invariant.Sample(managed))
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
	said, _ := whyItStopped(log, 0)
	err := fmt.Errorf("%s; its output is in %s", stopped, log)
	if said != "" {
		err = fmt.Errorf("%s; it wrote %q, and the rest of its output is in %s", stopped, said, log)
	}
	if strings.Contains(said, "address already in use") {
		err = fmt.Errorf("%w; another process holds that port, perhaps a concurrent run of this target, so give the target a free one in launch.args", err)
	}
	if r.cr != "" {
		err = fmt.Errorf("%w; botbox had created the CR, so the CR may have crashed the target, and %s replays the run",
			err, filepath.Join(r.dir, sequenceFile))
	}
	return err
}

// panicked opens the report a Go runtime writes on the way out.
var panicked = []string{"panic: ", "fatal error: "}

var (
	// frameLocation is the file line of a Go stack frame, below its function.
	frameLocation   = regexp.MustCompile(`^\t.+:\d+( \+0x[0-9a-f]+)?$`)
	goroutineHeader = regexp.MustCompile(`^goroutine \d+ .*:$`)
)

// whyItStopped is what the target said as it stopped, in the log past from:
// the line the last panic opens with, or else the last whole line above any
// stack trace. It is empty where the log holds neither, which leaves the
// reader the file itself. It also returns where the log ends.
func whyItStopped(path string, from int64) (string, int64) {
	said, end := tailLines(path, from)
	for _, line := range slices.Backward(said) {
		if slices.ContainsFunc(panicked, func(opener string) bool { return strings.HasPrefix(line, opener) }) {
			return line, end
		}
	}
	for i, line := range slices.Backward(said) {
		inTrace := strings.TrimSpace(line) == "" || frameLocation.MatchString(line) || goroutineHeader.MatchString(line) ||
			(i+1 < len(said) && frameLocation.MatchString(said[i+1]))
		if !inTrace {
			return strings.TrimSpace(line), end
		}
	}
	return "", end
}

// tailLines are the whole lines of the file past from, at most maxTail bytes
// of them, innermost last, and where the file ends. A line longer than
// maxTail has no whole form to quote, and a file botbox cannot read has
// nothing.
func tailLines(path string, from int64) ([]string, int64) {
	file, err := os.Open(path)
	if err != nil {
		return nil, from
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, from
	}
	start := max(from, info.Size()-maxTail)
	tail := make([]byte, info.Size()-start)
	if _, err := file.ReadAt(tail, start); err != nil {
		return nil, from
	}
	lines := strings.Split(strings.TrimRight(string(tail), "\n"), "\n")
	// A tail cut short begins mid-line, so its first line is a fragment: klog
	// puts the level and the message at the front.
	if start > from {
		lines = lines[1:]
	}
	return lines, info.Size()
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

// checkResource refuses a fault on a resource the API server does not serve,
// since the proxy records a request's resource as the plural it is served at.
func (r *runner) checkResource(resource string) error {
	if resource == "" {
		return nil
	}
	served, err := r.h.servedResources()
	if err != nil {
		return err
	}
	var meant string
	for _, s := range served {
		if s.Name == resource {
			return nil
		}
		if strings.EqualFold(s.Name, resource) || strings.EqualFold(s.SingularName, resource) {
			meant = s.Name
		}
	}
	if meant != "" {
		return fmt.Errorf("match.resource %q is not a resource the API server serves; did you mean %s?", resource, meant)
	}
	return fmt.Errorf("match.resource %q is not a resource the API server serves; it takes a plural such as configmaps", resource)
}

// inject adds the op's fault to those the proxy applies. Its window opens
// where the proxy first applies it, which may be never (D36).
func (r *runner) inject(op Op) {
	r.timeline.Faults = append(r.timeline.Faults, Window{})
	r.faultOps = append(r.faultOps, op.Index)
	r.faults = append(r.faults, heldFault{
		id:     r.h.addFault(op.Fault.spec()),
		until:  op.Fault.Until.Op,
		window: len(r.timeline.Faults) - 1,
	})
}

// expireFaults removes the faults whose until trigger names this op or an
// earlier one. It drops a fault once its window is closed, so that the window
// holds every request the proxy faulted with it.
func (r *runner) expireFaults(op int) {
	for _, fault := range r.faults {
		if fault.until != nil && *fault.until <= op {
			r.h.removeFault(fault.id)
		}
	}
	r.readFaultWindows()
	r.faults = slices.DeleteFunc(r.faults, func(fault heldFault) bool { return fault.retired })
}

// readFaultWindows writes what the proxy has done with each fault into the
// timeline: a window opens where the proxy first applied the fault and closes
// where the proxy stopped applying it (D36). A fault the proxy never applied
// leaves its window unopened, because the run then ran as if the fault op
// were not there.
func (r *runner) readFaultWindows() {
	for i := range r.faults {
		fault := &r.faults[i]
		proxied := r.h.faultWindow(fault.id)
		r.timeline.Faults[fault.window] = Window{Start: proxied.First, End: proxied.Retired}
		fault.retired = !proxied.Retired.IsZero()
	}
}

// clearFaults takes every fault off the proxy, which closes its window. The
// teardown does it before it measures anything.
func (r *runner) clearFaults() {
	r.h.clearFaults()
	r.readFaultWindows()
}

// awaitRecovery waits for a target still owed time to recover from the faults.
func (r *runner) awaitRecovery(ctx context.Context) error {
	if r.violation != nil || r.failed || !r.faultsAndWaits().Recovering(r.now()) {
		return nil
	}
	wait, err := r.wait(ctx)
	if err == nil {
		r.timeline.Recovery = &wait
		// The faults are cleared, and the wait ran until the time they left
		// the target was up, so nothing excuses it.
		err = r.judge(Recovery, "the last fault stopped", wait, false)
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
	r.teardownStart = r.now()
	r.clearFaults()
	for i, window := range r.timeline.Faults {
		if window.Start.IsZero() {
			r.skipped = append(r.skipped, fmt.Sprintf("the proxy applied the fault of op %d to no request", r.faultOps[i]))
		}
	}
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

func (r *runner) exitNotes() []string {
	notes := make([]string, len(r.timeline.Exits))
	for i, exit := range r.timeline.Exits {
		notes[i] = fmt.Sprintf("the target exited during %s with %v", r.during(exit.At), exit)
	}
	return notes
}

// during names what the run was doing at t: the op it had applied last, or the
// teardown.
func (r *runner) during(t time.Time) string {
	if !t.Before(r.teardownStart) {
		return "the teardown"
	}
	var last AppliedOp
	for _, applied := range r.timeline.Ops {
		if !applied.At.After(t) {
			last = applied
		}
	}
	return fmt.Sprintf("op %d (%s)", last.Op.Index, last.Op.Type)
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
	return r.checkpoint(Teardown, clean)
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
