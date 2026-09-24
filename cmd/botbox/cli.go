package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/cluster"
	"github.com/rosenhouse/botbox/pkg/generate"
	"github.com/rosenhouse/botbox/pkg/report"
	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
)

// botbox exits with one of these codes (DESIGN.md §11).
const (
	exitOK        = 0
	exitViolation = 1
	exitError     = 2
)

const (
	defaultOut  = "botbox-out"
	defaultRuns = 10
	// minimizing is what a derived deadline gives a shrink pass beyond the runs.
	minimizing = 4 * time.Minute
)

const usage = `botbox exercises a controller against the generic invariants of DESIGN.md §6.

  botbox run    --target <yaml> [--runs N] [--seed S] [--out DIR] [--deadline D] [--junit FILE] [--kubeconfig FILE] [--launch-arg ARG]... [<sequence.json>...]
  botbox replay --target <yaml> [--out DIR] [--deadline D] [--junit FILE] [--kubeconfig FILE] [--launch-arg ARG]... <sequence.json>
  botbox matrix --target <yaml> --sequences <dir> [--out FILE] [--deadline D] [--kubeconfig FILE] [--launch-arg ARG]...
  botbox version
`

const (
	// shrinkDir holds the replays of a shrink pass, which the run directory
	// keeps none of (DESIGN.md §11).
	shrinkDir = "shrink"
	// sequenceFile is what run.WriteRunSequence writes.
	sequenceFile = "sequence.json"
	// shrunkFile holds a minimized sequence the deadline or an interrupt left
	// unrun, which no recording in the directory is of (DESIGN.md §11).
	shrunkFile = "sequence.shrunk.json"
)

// Generator draws a sequence from a seed, deterministically, for the target it
// was built for (DESIGN.md §5.4).
type Generator func(seed int64) (run.Sequence, error)

// rapidGenerator draws sequences from the target's CRD schema (DESIGN.md
// §5.4). It reads the CRDs once, because a draw itself does no I/O. It also
// says which spec paths generation leaves alone.
func rapidGenerator(t *target.Target) (Generator, []string, error) {
	g, err := generate.New(t, generate.Options{})
	if err != nil {
		return nil, nil, err
	}
	return g.Draw, g.LeftAlone(), nil
}

// cli is one invocation. Its writers, its generator, its test cluster, its
// clock and what discards a passing run are injected, so the unit tier needs
// no API server.
type cli struct {
	stdout, stderr io.Writer
	open           func(options, *target.Target) (session, error)
	// newGenerator builds the generator the invocation draws from, once, as
	// pkg/generate's does: reading the target's CRDs costs I/O, drawing does
	// not.
	newGenerator func(*target.Target) (Generator, []string, error)
	now          func() time.Time
	discard      func(out *run.Output, n int) error
}

func newCLI(stdout, stderr io.Writer) *cli {
	return &cli{stdout: stdout, stderr: stderr, open: openSession, newGenerator: rapidGenerator,
		now: time.Now, discard: (*run.Output).Discard}
}

// session executes sequences against one test cluster. Runs share it, because
// starting a control plane costs seconds (DESIGN.md §5.5).
type session interface {
	// vet refuses a target the cluster cannot run, before any run starts.
	vet(t *target.Target) error
	execute(ctx context.Context, t *target.Target, sequence run.Sequence, dir string, check run.Checker) (run.Result, error)
	close() error
}

// options are the flags of DESIGN.md §11.
type options struct {
	command       string
	target        string
	sequences     string
	out           string
	junit         string
	kubeconfig    string
	deadline      time.Duration
	deadlineGiven bool
	launchArgs    []string
	runs          int
	runsGiven     bool
	seed          int64
	seedGiven     bool
}

// main returns what botbox exits with. An invocation a signal interrupted
// returns 128 plus the signal's number, as a shell reports it.
func (c *cli) main(ctx context.Context, args []string) int {
	return exitStatus(ctx, c.dispatch(ctx, args))
}

func exitStatus(ctx context.Context, code int) int {
	if stop, ok := interruption(ctx); ok {
		return 128 + int(stop.signal)
	}
	return code
}

func (c *cli) dispatch(ctx context.Context, args []string) int {
	opts, paths, err := parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(c.stdout, usage)
		return exitOK
	}
	if err != nil {
		return c.failUnrecorded(opts, nil, err)
	}
	switch opts.command {
	case "version":
		fmt.Fprintf(c.stdout, "botbox %s\n", version())
		return exitOK
	case "matrix":
		return c.bugMatrix(ctx, opts)
	}
	return c.exercise(ctx, opts, paths)
}

// exercise executes the sequences the caller named, or the ones botbox draws
// from the seed, and stops at the first that fails. Once the runs are planned,
// the invocation's directory records how each ended.
func (c *cli) exercise(ctx context.Context, opts options, paths []string) int {
	exercised, err := target.Load(opts.target)
	if err != nil {
		return c.failUnrecorded(opts, nil, err)
	}
	exercised.Launch.Args = append(exercised.Launch.Args, opts.launchArgs...)
	if len(paths) == 0 && !opts.seedGiven {
		opts.seed = newSeed()
	}
	runs, err := c.plan(opts, exercised, paths)
	if err != nil {
		return c.failUnrecorded(opts, exercised, err)
	}
	start := c.now()
	out, err := run.OpenOutput(opts.out, opts.invocationSeed(runs[0].sequence), start)
	if err != nil {
		return c.failUnrecorded(opts, exercised, err)
	}

	record := newSummary(opts, exercised, runs, start)
	// The last save warns of a file botbox cannot write.
	_ = c.save(record, opts, out.Dir())
	var code int
	s, err := c.startSession(opts, exercised)
	if err != nil {
		code = c.stop(record, err)
	} else {
		code = c.runAll(ctx, opts, s, exercised, runs, out, record)
	}
	record.finish(ctx, code, c.now())
	// A second signal kills botbox at once, and stopping the cluster takes
	// seconds, so the summary comes first.
	c.warn(c.save(record, opts, out.Dir()))
	if s != nil {
		c.warn(s.close())
	}
	// An interrupt that arrived since changes what botbox exits with.
	if exitStatus(ctx, code) != *record.ExitCode {
		record.finish(ctx, code, record.Finish)
		c.warn(c.save(record, opts, out.Dir()))
	}
	return code
}

// save writes the summary, and the JUnit file if one was asked for.
func (c *cli) save(record *summary, opts options, dir string) error {
	err := record.write(dir)
	if opts.junit != "" {
		err = errors.Join(err, record.writeJUnit(opts.junit, dir))
	}
	return err
}

// runAll executes the runs in order, records each, and stops at the first
// that fails.
func (c *cli) runAll(ctx context.Context, opts options, s session, t *target.Target,
	runs []planned, out *run.Output, record *summary) int {
	if err := s.vet(t); err != nil {
		return c.stop(record, err)
	}
	sequences := make([]run.Sequence, len(runs))
	for i, planned := range runs {
		sequences[i] = planned.sequence
	}
	c.derive(&opts, t, sequences, runs[0].generated())
	record.deadline(opts)

	ctx, cancel := context.WithTimeout(ctx, opts.deadline)
	defer cancel()
	for i, planned := range runs {
		if _, ok := interruption(ctx); ok {
			return c.stop(record, fmt.Errorf("an interrupt stopped the invocation after %d of %d runs", i, len(runs)))
		}
		// The deadline is the invocation's budget (DESIGN.md §11): the next
		// run does not start. The first always starts, so a spent deadline is
		// blamed on a run. An invocation the deadline stopped tested less than
		// asked, so it does not pass.
		if i > 0 && ctx.Err() != nil {
			return c.stop(record, fmt.Errorf("%s stopped the invocation after %d of %d runs", opts.deadlineName(), i, len(runs)))
		}
		number := i + 1
		fmt.Fprintf(c.stdout, "run %d: seed %d, %s\n", number, planned.sequence.Seed, planned.source())
		dir := out.RunDir(number)
		ran := &record.Runs[i]
		ran.Outcome = outcomeUnfinished
		_ = c.save(record, opts, out.Dir())
		started := c.now()
		result, err := s.execute(ctx, t, planned.sequence, dir, run.Engine{})
		ran.ran(result, c.now().Sub(started))
		code := exitCode(result, err)
		if code == exitViolation {
			// Minimizing can take minutes, so the summary keeps the find first.
			ran.found(*result.Violation, reportNotes(opts, t, result), dir)
			_ = c.save(record, opts, out.Dir())
			violation, notes := c.reportFailure(ctx, opts, s, t, planned, result, number, dir, func() {
				ran.rerunning = true
				_ = c.save(record, opts, out.Dir())
			})
			ran.found(violation, notes, dir)
			return exitViolation
		}
		c.printNotes(number, result.Notes)
		if code == exitError {
			err = opts.named(ctx, err)
			ran.stopped(err, dir)
			return c.failRun(number, planned, dir, err)
		}
		ran.Outcome = outcomePassed
		if err := c.discard(out, number); err != nil {
			return c.stop(record, err)
		}
	}
	fmt.Fprintln(c.stdout, "every run passed.")
	return exitOK
}

// failUnrecorded ends an invocation that has no directory to write its summary
// in. Its JUnit file still says what stopped it.
func (c *cli) failUnrecorded(opts options, t *target.Target, err error) int {
	if opts.junit != "" {
		now := c.now().UTC()
		record := &summary{Botbox: version(), Target: summaryTarget{Name: "botbox"}, Start: now, Finish: now, Error: err.Error()}
		if t != nil {
			record.Target.Name = t.Name
		}
		c.warn(record.writeJUnit(opts.junit, ""))
	}
	return c.fail(err)
}

// stop ends the invocation on an error no run carries.
func (c *cli) stop(record *summary, err error) int {
	record.Error = err.Error()
	return c.fail(err)
}

// planned is one run's sequence and the file it was read from. botbox drew
// the sequences of a run that names no file.
type planned struct {
	sequence run.Sequence
	path     string
}

func (p planned) generated() bool { return p.path == "" }

func (p planned) source() string {
	if p.generated() {
		return "generated"
	}
	return "sequence " + p.path
}

// plan reads the sequences the caller named, or draws one per run from
// consecutive seeds, so that each run replays from its own.
func (c *cli) plan(opts options, t *target.Target, paths []string) ([]planned, error) {
	if len(paths) > 0 {
		sequences, err := readSequences(paths)
		if err != nil {
			return nil, err
		}
		runs := make([]planned, len(sequences))
		for i, sequence := range sequences {
			runs[i] = planned{sequence: sequence, path: paths[i]}
		}
		return runs, nil
	}
	draw, leftAlone, err := c.newGenerator(t)
	if err != nil {
		return nil, err
	}
	for _, note := range leftAlone {
		fmt.Fprintln(c.stdout, note)
	}
	runs := make([]planned, opts.runs)
	for i := range runs {
		sequence, err := draw(opts.seed + int64(i))
		if err != nil {
			return nil, fmt.Errorf("generating run %d: %w", i+1, err)
		}
		runs[i] = planned{sequence: sequence}
	}
	return runs, nil
}

// reportFailure minimizes a sequence botbox drew and leaves it in the run
// directory with the evidence of a run of it (DESIGN.md §5.5). A sequence the
// caller wrote is reported as it was written. It calls rerunning before it
// runs the minimized sequence, and returns the violation and notes the report
// carries.
func (c *cli) reportFailure(ctx context.Context, opts options, s session, t *target.Target,
	failed planned, result run.Result, number int, dir string, rerunning func()) (run.Violation, []string) {
	violation := *result.Violation
	if !failed.generated() {
		// The caller's file is a better thing to replay than a copy of it.
		c.warn(c.writeReport(dir, opts, t, failed.path, failed.sequence, result))
		c.printNotes(number, result.Notes)
		c.report(number, violation, dir)
		return violation, reportNotes(opts, t, result)
	}
	shrunk := run.Shrink(ctx, failed.sequence, violation, func(ctx context.Context, candidate run.Sequence) (run.Result, error) {
		return s.execute(ctx, t, candidate, filepath.Join(dir, shrinkDir), run.Engine{})
	})
	reported := shrunk
	simplified := simpler(shrunk, failed.sequence)
	// notes are the reported run's. The report adds notes of its own below.
	notes := result.Notes
	switch {
	case ctx.Err() != nil && simplified:
		// The deadline ended the pass before the smaller sequence could be
		// run into the directory, which still holds the run of the sequence
		// botbox drew. The directory reports the sequence its evidence is
		// of, and keeps the smaller one beside it.
		reported = failed.sequence
		c.warn(run.WriteSequence(filepath.Join(dir, shrunkFile), shrunk))
		c.warn(fmt.Errorf("%s ended the shrink pass with %s, left unrun in %s",
			ended(ctx), ops(shrunk), filepath.Join(dir, shrunkFile)))
		result.Notes = append(result.Notes, fmt.Sprintf(
			"%s ended minimization with %s, left unrun in %s: this is the sequence botbox drew",
			ended(ctx), ops(shrunk), shrunkFile))
	case ctx.Err() != nil:
		// §5.7 says a report carries the minimized sequence, and the pass
		// never got to a smaller one (D31).
		result.Notes = append(result.Notes, ended(ctx)+
			" ended minimization before it found a smaller sequence: this is the sequence botbox drew")
	case simplified:
		rerunning()
		again, err := c.rerun(ctx, opts, s, t, shrunk, dir)
		switch {
		case again.Violation != nil:
			result, violation, notes = again, *again.Violation, again.Notes
		case err != nil:
			result.Timeline = again.Timeline
			result.Notes = append(result.Notes, fmt.Sprintf(
				"the minimized sequence did not finish when it ran again, so this directory holds that partial run and not the one %s was found in",
				violation.ID))
		default:
			// The recordings are of the rerun, so the report counts its ops.
			result.Timeline = again.Timeline
			// The directory now holds a run of the minimized sequence that
			// found nothing. The finding stands, and the report says which
			// run these recordings are of rather than leaving a reader to
			// infer it from a passing log (D31, D35).
			result.Notes = append(result.Notes, fmt.Sprintf(
				"the minimized sequence passed when it ran again, so this directory holds that run and not the one %s was found in",
				violation.ID))
		}
	}
	// What the pass replayed is nobody's evidence (DESIGN.md §11).
	c.warn(os.RemoveAll(filepath.Join(dir, shrinkDir)))
	c.warn(run.WriteRunSequence(dir, reported))
	c.warn(c.writeReport(dir, opts, t, filepath.Join(dir, sequenceFile), reported, result))
	// The violation is reported once the directory holds the run it belongs to.
	c.printNotes(number, notes)
	c.report(number, violation, dir)
	fmt.Fprintf(c.stdout, "  the sequence is %s, in %s\n", ops(reported), filepath.Join(dir, sequenceFile))
	return violation, reportNotes(opts, t, result)
}

// rerun executes the minimized sequence into the run directory, so that the
// recordings there are of the sequence the run reports, and returns what that
// run found. A run that did not finish or reproduced nothing says what the
// directory then holds.
func (c *cli) rerun(ctx context.Context, opts options, s session, t *target.Target, shrunk run.Sequence, dir string) (run.Result, error) {
	result, err := s.execute(ctx, t, shrunk, dir, run.Engine{})
	switch {
	case err != nil:
		c.warn(fmt.Errorf("the minimized sequence did not finish when it ran again, so %s holds that partial run: %w",
			dir, opts.named(ctx, err)))
	case result.Violation == nil:
		c.warn(fmt.Errorf("the minimized sequence passed when it ran again, so %s holds that run", dir))
	}
	return result, err
}

// replayCommand is the one line §5.7 asks a report to carry. It repeats the
// flags that select what ran, because a command that leaves them out runs a
// different target and reproduces nothing.
func (o options) replayCommand(sequence string) string {
	command := []string{"botbox", "replay", "--target", o.target}
	if o.kubeconfig != "" {
		command = append(command, "--kubeconfig", o.kubeconfig)
	}
	for _, arg := range o.launchArgs {
		command = append(command, "--launch-arg", arg)
	}
	if strings.HasPrefix(sequence, "-") {
		sequence = "./" + sequence
	}
	command = append(command, sequence)
	for i, word := range command {
		command[i] = shellQuote(word)
	}
	return strings.Join(command, " ")
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@%+,-][A-Za-z0-9_./=:@%+,-]*$`)

// shellQuote single-quotes a word unless shellSafe shows that sh and zsh read
// it literally.
func shellQuote(word string) string {
	if shellSafe.MatchString(word) {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// writeReport leaves §5.7's report beside the recordings it describes. replay
// names the sequence file a reader should run to see this again.
func (c *cli) writeReport(dir string, opts options, t *target.Target,
	replay string, sequence run.Sequence, result run.Result) error {
	if result.Violation == nil {
		return nil
	}
	encoded, err := sequence.Marshal()
	if err != nil {
		return err
	}
	violation := *result.Violation
	return report.Write(dir, report.Report{
		Check:            report.Check{ID: violation.ID, Statement: violation.Statement, At: violation.At, Evidence: violation.Evidence},
		Target:           report.Target{Name: t.Name, Version: t.Version},
		Botbox:           version(),
		Seed:             sequence.Seed,
		Notes:            reportNotes(opts, t, result),
		Replay:           opts.replayCommand(replay),
		Sequence:         encoded,
		Applied:          len(result.Timeline.Ops),
		Ops:              len(sequence.Ops),
		Requests:         violation.Requests,
		RequestsTotal:    violation.RequestsTotal,
		Versions:         violation.Versions,
		VersionsTotal:    violation.VersionsTotal,
		VersionsOf:       violation.VersionsOf,
		Managed:          violation.Managed,
		ManagedTotal:     violation.ManagedTotal,
		Ready:            violation.Ready,
		Differences:      violation.Differences,
		DifferencesTotal: violation.DifferencesTotal,
		Compared:         violation.Compared,
	})
}

// reportNotes are the notes of a failing run's report.
func reportNotes(opts options, t *target.Target, result run.Result) []string {
	if limit := envtestLimit(opts, t); limit != "" && result.Violation.ID == "G4" {
		return slices.Concat(result.Notes, []string{limit})
	}
	return result.Notes
}

// warn reports what went wrong beside a finding, which stands whether or not
// the run directory could be tidied (DESIGN.md §11).
func (c *cli) warn(err error) {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, err := range joined.Unwrap() {
			c.warn(err)
		}
		return
	}
	if err != nil {
		fmt.Fprintln(c.stderr, "botbox:", err)
	}
}

func ops(s run.Sequence) string {
	if len(s.Ops) == 1 {
		return "1 op"
	}
	return fmt.Sprintf("%d ops", len(s.Ops))
}

// newSeed is the seed of a run the caller gave none for. Every run prints its
// seed, so that the failure is reproducible from it (DESIGN.md §11).
func newSeed() int64 { return rand.Int64() }

func (c *cli) printNotes(number int, notes []string) {
	for _, note := range notes {
		fmt.Fprintf(c.stdout, "run %d: %s\n", number, note)
	}
}

func (c *cli) report(number int, violation run.Violation, dir string) {
	fmt.Fprintf(c.stdout, "run %d: %s %s\n", number, violation.ID, violation.Statement)
	if said := quotes(violation); said != "" {
		fmt.Fprintf(c.stdout, "  %s\n", said)
	}
	fmt.Fprintf(c.stdout, "  the evidence is in %s\n", dir)
}

// quotes is what the CLI prints under a violation: when the check judged, and
// what it quoted (DESIGN.md §5.7).
func quotes(violation run.Violation) string {
	var said []string
	if !violation.At.IsZero() {
		said = append(said, "at "+violation.At.Format(time.RFC3339Nano))
	}
	if violation.Evidence != "" {
		said = append(said, violation.Evidence)
	}
	return strings.Join(said, "; ")
}

func (c *cli) fail(err error) int {
	fmt.Fprintln(c.stderr, "botbox:", err)
	return exitError
}

// failRun reports a run that could not finish and says where to look.
func (c *cli) failRun(number int, failed planned, dir string, err error) int {
	fmt.Fprintf(c.stderr, "botbox: run %d: %v\n", number, err)
	var refused *run.Refused
	switch {
	case !errors.As(err, &refused):
		c.showRunFiles(dir)
	case failed.generated():
		fmt.Fprintf(c.stderr, "  the op is in %s\n", filepath.Join(dir, sequenceFile))
		fmt.Fprintln(c.stderr, "  botbox drew it to pass the CRD's schema and rules without the status the controller"+
			" writes. So a CRD rule that reads status refused it, or a rule botbox cannot see, such as an admission"+
			" webhook's. Keep drawn values inside that rule with generate.mutate or generate.overlay.")
	default:
		fmt.Fprintf(c.stderr, "  the op is in %s\n", failed.path)
	}
	return exitError
}

// showRunFiles prints the directory a failed run left, if it exists.
func (c *cli) showRunFiles(dir string) {
	if _, err := os.Stat(dir); err == nil {
		fmt.Fprintf(c.stderr, "  the run's files are in %s\n", dir)
	}
}

// exitCode maps a run's outcome to the codes of DESIGN.md §11.
func exitCode(result run.Result, err error) int {
	switch {
	case err != nil:
		return exitError
	case result.Violation != nil:
		return exitViolation
	default:
		return exitOK
	}
}

var errRunInterrupted = errors.New("an interrupt stopped the run")

// named blames an interrupt, or the deadline, for a run its context cut short.
// §11 makes the deadline exit 2, which a reader has to be able to tell from a
// broken target. The context botbox built from the deadline is what it asks.
// The teardown's cleanup runs on a budget of its own.
func (o options) named(ctx context.Context, err error) error {
	if _, ok := interruption(ctx); ok && errors.Is(err, context.Canceled) {
		return errRunInterrupted
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s ended the run: %w", o.deadlineName(), err)
}

// deadlineName names the deadline, and says whether botbox derived it.
func (o options) deadlineName() string {
	if o.deadlineGiven {
		return "the --deadline of " + o.deadline.String()
	}
	return "the derived deadline of " + o.deadline.String()
}

// derive gives an invocation that set no deadline what its runs can take at
// the target's timeouts, and time to minimize a failure where botbox drew the
// sequences.
func (c *cli) derive(opts *options, t *target.Target, sequences []run.Sequence, minimizes bool) {
	if opts.deadlineGiven {
		return
	}
	runs := run.Bound(t, sequences...)
	opts.deadline = runs
	these := "this run"
	if len(sequences) > 1 {
		these = fmt.Sprintf("these %d runs", len(sequences))
	}
	if !minimizes {
		fmt.Fprintf(c.stdout, "the deadline is %s: %s can take that long at the target's timeouts. --deadline sets another.\n",
			opts.deadline, these)
		return
	}
	opts.deadline = min(runs, math.MaxInt64-minimizing) + minimizing
	fmt.Fprintf(c.stdout, "the deadline is %s: %s can take %s at the target's timeouts, and minimizing a failure gets %s. --deadline sets another.\n",
		opts.deadline, these, runs, minimizing)
}

// ended names what ended ctx: an interrupt, or else the deadline.
func ended(ctx context.Context) string {
	if _, ok := interruption(ctx); ok {
		return "an interrupt"
	}
	return "the deadline"
}

// simpler reports whether the shrink pass changed the sequence at all. It
// removes ops and weakens faults, and a sequence the report carries has to be
// the one the directory holds a run of, however the pass simplified it.
func simpler(shrunk, failing run.Sequence) bool {
	was, err := failing.Marshal()
	if err != nil {
		return false
	}
	now, err := shrunk.Marshal()
	if err != nil {
		return false
	}
	return !bytes.Equal(was, now)
}

// invocationSeed names the output directory: the seed the caller gave, or the
// first sequence's, so that the directory is reproducible from it.
func (o options) invocationSeed(first run.Sequence) int64 {
	if o.seedGiven {
		return o.seed
	}
	return first.Seed
}

func parse(args []string) (options, []string, error) {
	if len(args) == 0 {
		return options{}, nil, errors.New("no command\n" + usage)
	}
	opts := options{command: args[0]}
	switch opts.command {
	case "help", "-h", "--help":
		return opts, nil, flag.ErrHelp
	case "version":
		return opts, nil, nil
	case "run", "replay", "matrix":
	default:
		return opts, nil, fmt.Errorf("%q is not a botbox command\n%s", opts.command, usage)
	}

	flags := opts.flags()
	if err := flags.Parse(args[1:]); err != nil {
		return opts, nil, err
	}
	flags.Visit(func(f *flag.Flag) {
		opts.seedGiven = opts.seedGiven || f.Name == "seed"
		opts.runsGiven = opts.runsGiven || f.Name == "runs"
		opts.deadlineGiven = opts.deadlineGiven || f.Name == "deadline"
	})

	sequences := flags.Args()
	if opts.target == "" {
		return opts, nil, errors.New("the --target flag is required")
	}
	if opts.deadlineGiven && opts.deadline <= 0 {
		return opts, nil, fmt.Errorf("--deadline is %s, and an invocation needs time to run: leave the flag out, and botbox derives one", opts.deadline)
	}
	if opts.command == "run" {
		if err := opts.validateRuns(len(sequences)); err != nil {
			return opts, nil, err
		}
	}
	if opts.command == "replay" && len(sequences) != 1 {
		return opts, nil, fmt.Errorf("botbox replay takes one sequence file, and %d were given", len(sequences))
	}
	if opts.command == "matrix" {
		if opts.sequences == "" {
			return opts, nil, errors.New("the --sequences flag is required: it holds one sequence per seeded bug")
		}
		if len(sequences) > 0 {
			return opts, nil, fmt.Errorf("botbox matrix takes no sequence file, and %d were given: it runs the --sequences directory", len(sequences))
		}
	}
	return opts, sequences, nil
}

// validateRuns holds --runs to the sequences botbox draws itself: named
// sequence files are what they are.
func (o options) validateRuns(named int) error {
	switch {
	case o.runs < 1:
		return fmt.Errorf("--runs is %d, and an invocation runs at least one sequence", o.runs)
	case named > 0 && o.runsGiven:
		return errors.New("--runs draws sequences, and naming sequence files runs those: give one or the other")
	}
	return nil
}

func (o *options) flags() *flag.FlagSet {
	flags := flag.NewFlagSet("botbox "+o.command, flag.ContinueOnError)
	flags.SetOutput(io.Discard) // The caller prints what Parse returns.
	flags.StringVar(&o.target, "target", "", "the target.yaml to exercise")
	flags.StringVar(&o.kubeconfig, "kubeconfig", "", "an existing cluster to run against, instead of envtest")
	flags.DurationVar(&o.deadline, "deadline", 0, "how long the invocation may take")
	flags.Var((*stringList)(&o.launchArgs), "launch-arg", "append an argument to the target's launch.args (repeatable)")
	if o.command == "matrix" {
		flags.StringVar(&o.out, "out", defaultMatrix, "the Markdown file to write")
		flags.StringVar(&o.sequences, "sequences", "", "the directory holding one b<id>.json per seeded bug")
	} else {
		flags.StringVar(&o.out, "out", defaultOut, "where failing runs are written")
	}
	if o.command != "matrix" {
		flags.StringVar(&o.junit, "junit", "", "a JUnit XML file to write each run's outcome to")
	}
	if o.command == "run" {
		flags.IntVar(&o.runs, "runs", defaultRuns, "how many sequences to draw and run")
		flags.Int64Var(&o.seed, "seed", 0, "the seed the first sequence is drawn from, which the output directory is named after")
	}
	return flags
}

// stringList collects a repeatable flag in the order it was given, so that a
// later --launch-arg wins where the target parses its flags.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, " ") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func readSequences(paths []string) ([]run.Sequence, error) {
	sequences := make([]run.Sequence, 0, len(paths))
	for _, path := range paths {
		sequence, err := run.ReadSequence(path)
		if err != nil {
			return nil, err
		}
		sequences = append(sequences, sequence)
	}
	return sequences, nil
}

// clusterSession runs against one test cluster: an envtest control plane
// botbox starts, or the cluster a kubeconfig names (DESIGN.md §5.8).
type clusterSession struct {
	*cluster.Cluster
	// controllerManager says the cluster runs kube-controller-manager, which
	// envtest does not.
	controllerManager bool
}

// envtestStatic are the kinds that run Pods, and the claims Pods mount. Only
// kube-controller-manager and a kubelet move their status.
var envtestStatic = []schema.GroupKind{
	{Group: "apps", Kind: "Deployment"},
	{Group: "apps", Kind: "StatefulSet"},
	{Group: "apps", Kind: "DaemonSet"},
	{Group: "apps", Kind: "ReplicaSet"},
	{Group: "batch", Kind: "Job"},
	{Group: "batch", Kind: "CronJob"},
	{Kind: "ReplicationController"},
	{Kind: "Pod"},
	{Kind: "PersistentVolumeClaim"},
}

// envtestLimit names the managed kinds envtest never moves, or is empty.
func envtestLimit(opts options, t *target.Target) string {
	if opts.kubeconfig != "" {
		return ""
	}
	var static []string
	for _, gvk := range t.Manages {
		if slices.Contains(envtestStatic, gvk.GroupKind()) {
			static = append(static, gvk.Kind)
		}
	}
	if len(static) == 0 {
		return ""
	}
	return "envtest runs no controller manager and no kubelet, so no Pod runs, and the status of these kinds never changes: " +
		strings.Join(static, ", ") + ". A ready predicate that waits on that status never holds, and a controller that" +
		" requeues while it waits can hide a missed watch. Run this target with --kubeconfig against a kind cluster."
}

// startSession warns of what envtest never runs, then opens the session.
func (c *cli) startSession(opts options, t *target.Target) (session, error) {
	if limit := envtestLimit(opts, t); limit != "" {
		fmt.Fprintln(c.stderr, "botbox:", limit)
	}
	return c.open(opts, t)
}

func openSession(opts options, t *target.Target) (session, error) {
	if err := t.Launch.Check(); err != nil {
		return nil, err
	}
	crds := cluster.Options{CRDPaths: t.CRDs}
	if opts.kubeconfig != "" {
		connected, err := cluster.Connect(opts.kubeconfig, crds)
		if err != nil {
			return nil, err
		}
		return &clusterSession{Cluster: connected, controllerManager: true}, nil
	}
	started, err := cluster.Start(crds)
	if err != nil {
		return nil, err
	}
	return &clusterSession{Cluster: started}, nil
}

// vet refuses the kinds the cluster serves at cluster scope, which the target's
// CRDs cannot show for a built-in kind.
func (s *clusterSession) vet(t *target.Target) error {
	mapper, err := cluster.NewRESTMapper(s.Config())
	if err != nil {
		return err
	}
	return t.CheckScopes(mapper)
}

func (s *clusterSession) execute(ctx context.Context, t *target.Target, sequence run.Sequence, dir string, check run.Checker) (run.Result, error) {
	return run.Run(ctx, t, sequence, run.Options{
		Dir:               dir,
		Config:            s.Config(),
		ControllerManager: s.controllerManager,
		Check:             check,
	})
}

func (s *clusterSession) close() error { return s.Stop() }

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}
