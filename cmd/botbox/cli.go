package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

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
	defaultOut      = "botbox-out"
	defaultDeadline = 4 * time.Minute
	defaultRuns     = 10
)

const usage = `botbox exercises a controller against the generic invariants of DESIGN.md §6.

  botbox run    --target <yaml> [--runs N] [--seed S] [--out DIR] [--deadline D] [--launch-arg ARG]... [<sequence.json>...]
  botbox replay --target <yaml> [--out DIR] [--deadline D] [--launch-arg ARG]... <sequence.json>
  botbox matrix --target <yaml> --sequences <dir> [--out FILE] [--deadline D]
  botbox version
`

const (
	// shrinkDir holds the replays of a shrink pass, which the run directory
	// keeps none of (DESIGN.md §11).
	shrinkDir = "shrink"
	// sequenceFile is what run.WriteRunSequence writes.
	sequenceFile = "sequence.json"
	// shrunkFile holds a minimized sequence the deadline left unrun, which no
	// recording in the directory is of (DESIGN.md §11).
	shrunkFile = "sequence.shrunk.json"
)

// Generator draws a sequence from a seed, deterministically, for the target it
// was built for (DESIGN.md §5.4).
type Generator func(seed int64) (run.Sequence, error)

// rapidGenerator draws sequences from the target's CRD schema (DESIGN.md
// §5.4). It reads the CRDs once, because a draw itself does no I/O.
func rapidGenerator(t *target.Target) (Generator, error) {
	g, err := generate.New(t, generate.Options{})
	if err != nil {
		return nil, err
	}
	return g.Draw, nil
}

// cli is one invocation. Its writers, its generator and its test cluster are
// injected, so the unit tier needs no API server.
type cli struct {
	stdout, stderr io.Writer
	open           func(options, *target.Target) (session, error)
	// newGenerator builds the generator the invocation draws from, once, as
	// pkg/generate's does: reading the target's CRDs costs I/O, drawing does
	// not.
	newGenerator func(*target.Target) (Generator, error)
}

// session executes sequences against one test cluster. Runs share it, because
// starting a control plane costs seconds (DESIGN.md §5.5).
type session interface {
	execute(ctx context.Context, t *target.Target, sequence run.Sequence, dir string, check run.Checker) (run.Result, error)
	close() error
}

// options are the flags of DESIGN.md §11.
type options struct {
	command    string
	target     string
	sequences  string
	out        string
	kubeconfig string
	deadline   time.Duration
	launchArgs []string
	runs       int
	runsGiven  bool
	seed       int64
	seedGiven  bool
}

func (c *cli) main(ctx context.Context, args []string) int {
	opts, paths, err := parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(c.stdout, usage)
		return exitOK
	}
	if err != nil {
		return c.fail(err)
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
// from the seed, and stops at the first that fails.
func (c *cli) exercise(ctx context.Context, opts options, paths []string) int {
	exercised, err := target.Load(opts.target)
	if err != nil {
		return c.fail(err)
	}
	exercised.Launch.Args = append(exercised.Launch.Args, opts.launchArgs...)
	if len(paths) == 0 && !opts.seedGiven {
		opts.seed = newSeed()
	}
	runs, err := c.plan(opts, exercised, paths)
	if err != nil {
		return c.fail(err)
	}

	s, err := c.open(opts, exercised)
	if err != nil {
		return c.fail(err)
	}
	defer func() { c.warn(s.close()) }()
	out, err := run.OpenOutput(opts.out, opts.invocationSeed(runs[0].sequence), time.Now())
	if err != nil {
		return c.fail(err)
	}

	ctx, cancel := context.WithTimeout(ctx, opts.deadline)
	defer cancel()
	for i, planned := range runs {
		// The deadline is the invocation's budget (DESIGN.md §11): a run that
		// began keeps its own, and the next does not start. The first always
		// runs, so an invocation is never vacuously green.
		if i > 0 && ctx.Err() != nil {
			fmt.Fprintf(c.stdout, "the deadline stopped the invocation after %d runs.\n", i)
			break
		}
		number := i + 1
		fmt.Fprintf(c.stdout, "run %d: seed %d, %s\n", number, planned.sequence.Seed, planned.source())
		dir := out.RunDir(number)
		result, err := s.execute(ctx, exercised, planned.sequence, dir, run.Engine{})
		for _, note := range result.Notes {
			fmt.Fprintf(c.stdout, "run %d: %s\n", number, note)
		}
		switch exitCode(result, err) {
		case exitError:
			return c.fail(opts.named(ctx, err))
		case exitViolation:
			return c.reportFailure(ctx, opts, s, exercised, planned, result, number, dir)
		}
		if err := out.Discard(number); err != nil {
			return c.fail(err)
		}
	}
	fmt.Fprintln(c.stdout, "every run passed.")
	return exitOK
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
	draw, err := c.newGenerator(t)
	if err != nil {
		return nil, err
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
// caller wrote is reported as it was written.
func (c *cli) reportFailure(ctx context.Context, opts options, s session, t *target.Target,
	failed planned, result run.Result, number int, dir string) int {
	violation := *result.Violation
	if !failed.generated() {
		// The caller's file is a better thing to replay than a copy of it.
		c.warn(c.writeReport(dir, opts, t, failed.path, failed.sequence, result))
		c.report(number, violation, dir)
		return exitViolation
	}
	shrunk := run.Shrink(ctx, failed.sequence, violation, func(ctx context.Context, candidate run.Sequence) (run.Result, error) {
		return s.execute(ctx, t, candidate, filepath.Join(dir, shrinkDir), run.Engine{})
	})
	reported := shrunk
	simplified := simpler(shrunk, failed.sequence)
	switch {
	case ctx.Err() != nil && simplified:
		// The deadline ended the pass before the smaller sequence could be
		// run into the directory, which still holds the run of the sequence
		// botbox drew. The directory reports the sequence its evidence is
		// of, and keeps the smaller one beside it.
		reported = failed.sequence
		c.warn(run.WriteSequence(filepath.Join(dir, shrunkFile), shrunk))
		c.warn(fmt.Errorf("the deadline ended the shrink pass with %s, left unrun in %s",
			ops(shrunk), filepath.Join(dir, shrunkFile)))
		result.Notes = append(result.Notes, fmt.Sprintf(
			"the deadline ended minimization with %s, left unrun in %s: this is the sequence botbox drew",
			ops(shrunk), shrunkFile))
	case ctx.Err() != nil:
		// §5.7 says a report carries the minimized sequence, and the pass
		// never got to a smaller one (D31).
		result.Notes = append(result.Notes,
			"the deadline ended minimization before it found a smaller sequence: this is the sequence botbox drew")
	case simplified:
		if again := c.rerun(ctx, opts, s, t, shrunk, dir); again.Violation != nil {
			result, violation = again, *again.Violation
		} else {
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
	c.report(number, violation, dir)
	fmt.Fprintf(c.stdout, "  the sequence is %s, in %s\n", ops(reported), filepath.Join(dir, sequenceFile))
	return exitViolation
}

// rerun executes the minimized sequence into the run directory, so that the
// recordings there are of the sequence the run reports, and returns what that
// run found. A run that reproduced nothing says so: the directory then holds
// a run that passed.
func (c *cli) rerun(ctx context.Context, opts options, s session, t *target.Target, shrunk run.Sequence, dir string) run.Result {
	result, err := s.execute(ctx, t, shrunk, dir, run.Engine{})
	switch {
	case err != nil:
		c.warn(opts.named(ctx, err))
	case result.Violation == nil:
		c.warn(fmt.Errorf("the minimized sequence passed when it ran again, so %s holds that run", dir))
	}
	return result
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
	return strings.Join(append(command, sequence), " ")
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
		Check:    report.Check{ID: violation.ID, Statement: violation.Statement, Evidence: violation.Evidence},
		Target:   report.Target{Name: t.Name, Version: t.Version},
		Botbox:   version(),
		Seed:     sequence.Seed,
		Notes:    result.Notes,
		Replay:   opts.replayCommand(replay),
		Sequence: encoded,
		Applied:  len(result.Timeline.Ops),
		Ops:      len(sequence.Ops),
		Requests: violation.Requests,
		Versions: violation.Versions,
	})
}

// warn reports what went wrong beside a finding, which stands whether or not
// the run directory could be tidied (DESIGN.md §11).
func (c *cli) warn(err error) {
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

func (c *cli) report(number int, violation run.Violation, dir string) {
	fmt.Fprintf(c.stdout, "run %d: %s %s\n", number, violation.ID, violation.Statement)
	if violation.Evidence != "" {
		fmt.Fprintf(c.stdout, "  %s\n", violation.Evidence)
	}
	fmt.Fprintf(c.stdout, "  the evidence is in %s\n", dir)
}

func (c *cli) fail(err error) int {
	fmt.Fprintln(c.stderr, "botbox:", err)
	return exitError
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

// named blames the --deadline for a run its own budget cut short. §11 makes
// this exit 2, which a reader has to be able to tell from a broken target. The
// context botbox built from the flag is what it asks: the teardown runs on a
// budget of its own, and that one is nobody's flag (DESIGN.md §5.5).
func (o options) named(ctx context.Context, err error) error {
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("the --deadline of %s ended the run: %w", o.deadline, err)
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
	})

	sequences := flags.Args()
	if opts.target == "" {
		return opts, nil, errors.New("the --target flag is required")
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
	flags.DurationVar(&o.deadline, "deadline", defaultDeadline, "how long the invocation may take")
	flags.Var((*stringList)(&o.launchArgs), "launch-arg", "append an argument to the target's launch.args (repeatable)")
	if o.command == "matrix" {
		flags.StringVar(&o.out, "out", defaultMatrix, "the Markdown file to write")
		flags.StringVar(&o.sequences, "sequences", "", "the directory holding one b<id>.json per seeded bug")
	} else {
		flags.StringVar(&o.out, "out", defaultOut, "where failing runs are written")
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
	config *rest.Config
	// collected says the cluster collects owned objects itself, which envtest
	// does not.
	collected bool
	stop      func() error
}

func openSession(opts options, t *target.Target) (session, error) {
	if opts.kubeconfig != "" {
		config, err := clientcmd.BuildConfigFromFlags("", opts.kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("reading the kubeconfig %s: %w", opts.kubeconfig, err)
		}
		return &clusterSession{config: config, collected: true}, nil
	}
	started, err := cluster.Start(cluster.Options{CRDPaths: t.CRDs})
	if err != nil {
		return nil, err
	}
	return &clusterSession{config: started.Config(), stop: started.Stop}, nil
}

func (s *clusterSession) execute(ctx context.Context, t *target.Target, sequence run.Sequence, dir string, check run.Checker) (run.Result, error) {
	return run.Run(ctx, t, sequence, run.Options{
		Dir:              dir,
		Config:           s.config,
		GarbageCollected: s.collected,
		Check:            check,
	})
}

func (s *clusterSession) close() error {
	if s.stop == nil {
		return nil
	}
	return s.stop()
}

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}
