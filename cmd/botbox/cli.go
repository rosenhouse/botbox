package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rosenhouse/botbox/pkg/cluster"
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
)

const usage = `botbox exercises a controller against the generic invariants of DESIGN.md §6.

  botbox run    --target <yaml> [--runs N] [--seed S] [--out DIR] [--deadline D] [--launch-arg ARG]... <sequence.json>...
  botbox replay --target <yaml> [--out DIR] [--deadline D] [--launch-arg ARG]... <sequence.json>
  botbox matrix --target <yaml> --sequences <dir> [--out FILE] [--deadline D]
  botbox version
`

// generationArrivesInM5 is what botbox run says until it generates sequences
// itself (DESIGN.md §10).
const generationArrivesInM5 = "generated sequences arrive in M5; name the sequence files to execute"

// cli is one invocation. Its writers and its test cluster are injected, so the
// unit tier needs no API server.
type cli struct {
	stdout, stderr io.Writer
	open           func(options, *target.Target) (session, error)
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

// exercise executes every named sequence and stops at the first that fails.
func (c *cli) exercise(ctx context.Context, opts options, paths []string) int {
	if opts.command == "run" && (len(paths) == 0 || opts.runs != 1) {
		return c.fail(errors.New(generationArrivesInM5))
	}
	exercised, err := target.Load(opts.target)
	if err != nil {
		return c.fail(err)
	}
	exercised.Launch.Args = append(exercised.Launch.Args, opts.launchArgs...)
	sequences, err := readSequences(paths)
	if err != nil {
		return c.fail(err)
	}

	s, err := c.open(opts, exercised)
	if err != nil {
		return c.fail(err)
	}
	defer func() {
		if err := s.close(); err != nil {
			fmt.Fprintln(c.stderr, "botbox:", err)
		}
	}()
	out, err := run.OpenOutput(opts.out, opts.invocationSeed(sequences), time.Now())
	if err != nil {
		return c.fail(err)
	}

	ctx, cancel := context.WithTimeout(ctx, opts.deadline)
	defer cancel()
	for i, sequence := range sequences {
		number := i + 1
		fmt.Fprintf(c.stdout, "run %d: seed %d, sequence %s\n", number, sequence.Seed, paths[i])
		result, err := s.execute(ctx, exercised, sequence, out.RunDir(number), run.Engine{})
		for _, note := range result.Notes {
			fmt.Fprintf(c.stdout, "run %d: %s\n", number, note)
		}
		switch exitCode(result, err) {
		case exitError:
			return c.fail(err)
		case exitViolation:
			c.report(number, *result.Violation, out.RunDir(number))
			return exitViolation
		}
		if err := out.Discard(number); err != nil {
			return c.fail(err)
		}
	}
	fmt.Fprintln(c.stdout, "every run passed.")
	return exitOK
}

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

// invocationSeed names the output directory: the seed the caller gave, or the
// first sequence's, so that the directory is reproducible from it.
func (o options) invocationSeed(sequences []run.Sequence) int64 {
	if o.seedGiven || len(sequences) == 0 {
		return o.seed
	}
	return sequences[0].Seed
}

func parse(args []string) (options, []string, error) {
	if len(args) == 0 {
		return options{}, nil, errors.New("no command\n" + usage)
	}
	opts := options{command: args[0]}
	switch opts.command {
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
	flags.Visit(func(f *flag.Flag) { opts.seedGiven = opts.seedGiven || f.Name == "seed" })

	sequences := flags.Args()
	if opts.target == "" {
		return opts, nil, errors.New("the --target flag is required")
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
		flags.IntVar(&o.runs, "runs", 1, "how many generated sequences to run")
		flags.Int64Var(&o.seed, "seed", 0, "the seed the output directory is named after")
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
