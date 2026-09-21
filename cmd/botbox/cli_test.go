package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse/botbox/pkg/report"
	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
)

const toyTargetYAML = "../../targets/toy-widget/target.yaml"

// fakeSession executes nothing: it records what the CLI asked for and answers
// from results.
type fakeSession struct {
	results  []run.Result
	failures []error
	// fails answers every execute the results do not, which is how a shrink
	// pass is given its verdicts. It reads the directory too, because that is
	// what tells a replay of the pass from the run of the minimized sequence.
	fails func(sequence run.Sequence, dir string) *run.Violation
	// after runs once a sequence has executed, which is where a test expires
	// the deadline.
	after     func()
	sequences []run.Sequence
	checks    []run.Checker
	dirs      []string
	args      [][]string
	closed    bool
}

func (s *fakeSession) execute(_ context.Context, t *target.Target, sequence run.Sequence, dir string, check run.Checker) (run.Result, error) {
	s.sequences = append(s.sequences, sequence)
	s.dirs = append(s.dirs, dir)
	s.args = append(s.args, t.Launch.Args)
	s.checks = append(s.checks, check)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return run.Result{}, err
	}
	if s.after != nil {
		s.after()
	}
	n := len(s.sequences) - 1
	var failure error
	if n < len(s.failures) {
		failure = s.failures[n]
	}
	if n < len(s.results) {
		return s.results[n], failure
	}
	if s.fails != nil {
		return run.Result{Violation: s.fails(sequence, dir)}, failure
	}
	return run.Result{}, failure
}

func (s *fakeSession) close() error {
	s.closed = true
	return nil
}

// invoke runs the CLI over args with a fake session, and returns its exit code
// and what it wrote.
func invoke(t *testing.T, fake *fakeSession, args ...string) (int, string, string) {
	t.Helper()
	return invokeWith(t, fake, countingGenerator(nil), args...)
}

func invokeWith(t *testing.T, fake *fakeSession, newGenerator func(*target.Target) (Generator, error), args ...string) (int, string, string) {
	t.Helper()
	return invokeCtx(t, t.Context(), fake, newGenerator, args...)
}

// invokeCtx is invokeWith under a context the test controls, for the deadline.
func invokeCtx(t *testing.T, ctx context.Context, fake *fakeSession,
	newGenerator func(*target.Target) (Generator, error), args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	c := &cli{
		stdout:       &stdout,
		stderr:       &stderr,
		open:         func(options, *target.Target) (session, error) { return fake, nil },
		newGenerator: newGenerator,
	}
	return c.main(ctx, args), stdout.String(), stderr.String()
}

// countingGenerator draws sequences of the ops given, or of one settle, and
// records every seed it was asked for.
func countingGenerator(seeds *[]int64, ops ...run.OpType) func(*target.Target) (Generator, error) {
	if len(ops) == 0 {
		ops = []run.OpType{run.OpSettle}
	}
	return func(t *target.Target) (Generator, error) {
		return func(seed int64) (run.Sequence, error) {
			if seeds != nil {
				*seeds = append(*seeds, seed)
			}
			sequence := run.Sequence{Seed: seed, Target: t.Name}
			for i, opType := range ops {
				sequence.Ops = append(sequence.Ops, run.Op{Index: i, Type: opType})
			}
			return sequence, nil
		}, nil
	}
}

// writeSequence writes a one-op sequence for the toy target.
func writeSequence(t *testing.T, seed int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sequence.json")
	sequence := run.Sequence{Seed: seed, Target: "toy-widget", Ops: []run.Op{{Type: run.OpSettle}}}
	if err := run.WriteSequence(path, sequence); err != nil {
		t.Fatalf("Writing the test sequence failed: %v", err)
	}
	return path
}

func TestVersionPrintsTheVersion(t *testing.T) {
	code, stdout, _ := invoke(t, &fakeSession{}, "version")

	if code != exitOK {
		t.Errorf("botbox version exited %d, want %d.", code, exitOK)
	}
	if !strings.HasPrefix(stdout, "botbox ") {
		t.Errorf("botbox version printed %q.", stdout)
	}
}

func TestHelpPrintsTheUsage(t *testing.T) {
	code, stdout, _ := invoke(t, &fakeSession{}, "run", "--help")

	if code != exitOK {
		t.Errorf("botbox run --help exited %d, want %d.", code, exitOK)
	}
	if !strings.Contains(stdout, "botbox replay") {
		t.Errorf("botbox run --help printed %q, want the usage.", stdout)
	}
}

func TestConfigurationErrorsExitTwo(t *testing.T) {
	sequence := writeSequence(t, 1)
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "no command", args: nil, want: "run"},
		{name: "an unknown command", args: []string{"frobnicate"}, want: "frobnicate"},
		{name: "an unknown flag", args: []string{"run", "--target", toyTargetYAML, "--loud"}, want: "loud"},
		{name: "no target", args: []string{"run", sequence}, want: "--target"},
		{name: "a target that does not load", args: []string{"replay", "--target", "absent.yaml", sequence}, want: "absent.yaml"},
		{name: "a sequence that does not load", args: []string{"replay", "--target", toyTargetYAML, "absent.json"}, want: "absent.json"},
		{name: "replay without a sequence", args: []string{"replay", "--target", toyTargetYAML}, want: "sequence"},
		{name: "replay with two sequences", args: []string{"replay", "--target", toyTargetYAML, sequence, sequence}, want: "one sequence"},
		{name: "--runs with a named sequence", args: []string{"run", "--target", toyTargetYAML, "--runs", "5", sequence}, want: "--runs"},
		{name: "no runs at all", args: []string{"run", "--target", toyTargetYAML, "--runs", "0"}, want: "--runs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, _, stderr := invoke(t, &fakeSession{}, test.args...)

			if code != exitError {
				t.Errorf("botbox %v exited %d, want %d.", test.args, code, exitError)
			}
			if !strings.Contains(stderr, test.want) {
				t.Errorf("botbox %v reported %q, want it to name %q.", test.args, stderr, test.want)
			}
		})
	}
}

func TestRunGeneratesOneSequencePerRun(t *testing.T) {
	session := &fakeSession{}
	var seeds []int64

	code, stdout, stderr := invokeWith(t, session, countingGenerator(&seeds),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3", "--seed", "42")

	if code != exitOK {
		t.Fatalf("botbox run exited %d: %s", code, stderr)
	}
	if want := []int64{42, 43, 44}; !slices.Equal(seeds, want) {
		t.Errorf("The generator drew the seeds %v, want %v: one run per seed, from the one given.", seeds, want)
	}
	if len(session.sequences) != 3 {
		t.Errorf("The session executed %d sequences, want one per run.", len(session.sequences))
	}
	for _, seed := range seeds {
		if !strings.Contains(stdout, fmt.Sprint(seed)) {
			t.Errorf("botbox run printed %q, want every seed printed.", stdout)
		}
	}
}

// DESIGN.md §11: seeds are always printed, so that every failure is
// reproducible from the seed and the sequence.
func TestRunWithoutASeedPicksOneAndPrintsIt(t *testing.T) {
	session := &fakeSession{}
	var seeds []int64

	code, stdout, stderr := invokeWith(t, session, countingGenerator(&seeds),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1")

	if code != exitOK {
		t.Fatalf("botbox run exited %d: %s", code, stderr)
	}
	if len(seeds) != 1 || seeds[0] == 0 {
		t.Fatalf("The generator drew the seeds %v, want one the CLI picked.", seeds)
	}
	if !strings.Contains(stdout, fmt.Sprint(seeds[0])) {
		t.Errorf("botbox run printed %q, want the seed %d it picked.", stdout, seeds[0])
	}
}

func TestANamedSequenceTakesPrecedenceOverGeneration(t *testing.T) {
	session := &fakeSession{}
	var seeds []int64

	code, _, stderr := invokeWith(t, session, countingGenerator(&seeds),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), writeSequence(t, 8675309))

	if code != exitOK {
		t.Fatalf("botbox run exited %d: %s", code, stderr)
	}
	if len(seeds) != 0 {
		t.Errorf("The generator drew %v, want nothing: the caller named the sequence.", seeds)
	}
	if len(session.sequences) != 1 || session.sequences[0].Seed != 8675309 {
		t.Errorf("The session executed %v, want the named sequence.", session.sequences)
	}
}

func opTypesOf(s run.Sequence) []run.OpType {
	var types []run.OpType
	for _, op := range s.Ops {
		types = append(types, op.Type)
	}
	return types
}

func TestRunShrinksTheFailingSequence(t *testing.T) {
	violation := run.Violation{ID: "G4", Statement: "the target converges", Evidence: "the settle wait after op 2 expired"}
	minimized := run.Violation{ID: "G4", Statement: "the target converges", Evidence: "the settle wait after op 0 expired"}
	session := &fakeSession{
		results: []run.Result{{Violation: &violation}},
		// Only the restart is needed to fail, so the pass removes every other
		// op but the settle that a sequence cannot end without (DESIGN.md §7).
		fails: func(candidate run.Sequence, _ string) *run.Violation {
			if slices.ContainsFunc(candidate.Ops, func(op run.Op) bool { return op.Type == run.OpRestart }) {
				return &minimized
			}
			return nil
		},
	}
	generate := countingGenerator(nil, run.OpSettle, run.OpRestart, run.OpSettle)

	code, stdout, _ := invokeWith(t, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("botbox run exited %d, want %d.", code, exitViolation)
	}
	written, err := run.ReadSequence(filepath.Join(session.dirs[0], sequenceFile))
	if err != nil {
		t.Fatalf("The run directory holds no minimized sequence: %v", err)
	}
	if want := []run.OpType{run.OpRestart, run.OpSettle}; !slices.Equal(opTypesOf(written), want) {
		t.Errorf("The run directory holds the sequence %v, want the minimized %v.", written.Ops, want)
	}
	if !strings.Contains(stdout, "G4") || !strings.Contains(stdout, "2 ops") {
		t.Errorf("botbox run printed %q, want the violation and what it shrank the sequence to.", stdout)
	}
	if !strings.Contains(stdout, minimized.Evidence) || strings.Contains(stdout, violation.Evidence) {
		t.Errorf("botbox run printed %q, want the evidence of the run the directory holds.", stdout)
	}
	last := len(session.sequences) - 1
	if session.dirs[last] != session.dirs[0] || len(session.sequences[last].Ops) != 2 {
		t.Errorf("The last run executed %d ops in %s, want the minimized sequence in the run directory: its evidence is what the run directory reports.",
			len(session.sequences[last].Ops), session.dirs[last])
	}
	if _, err := os.Stat(filepath.Join(session.dirs[0], shrinkDir)); !os.IsNotExist(err) {
		t.Errorf("The run directory keeps the shrink pass's replays: %v", err)
	}
}

// DESIGN.md §11: the run directory reports the sequence its recordings are of.
// A pass the deadline cut short never ran the smaller sequence, so what the
// directory holds is still the run of the sequence botbox drew.
func TestADeadlineDuringTheShrinkPassLeavesTheDrawnSequence(t *testing.T) {
	ctx, expire := context.WithCancel(t.Context())
	violation := run.Violation{ID: "G4", Statement: "the target converges"}
	session := &fakeSession{fails: func(run.Sequence, string) *run.Violation { return &violation }}
	// The run itself, then one candidate the pass accepts, then the deadline.
	session.after = func() {
		if len(session.sequences) == 2 {
			expire()
		}
	}
	generate := countingGenerator(nil, run.OpSettle, run.OpSettle, run.OpSettle)

	code, stdout, stderr := invokeCtx(t, ctx, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("botbox run exited %d, want %d.", code, exitViolation)
	}
	written, err := run.ReadSequence(filepath.Join(session.dirs[0], sequenceFile))
	if err != nil {
		t.Fatalf("The run directory holds no sequence: %v", err)
	}
	if len(written.Ops) != len(session.sequences[0].Ops) {
		t.Errorf("The run directory holds %d ops, want the %d of the sequence its recordings are of.",
			len(written.Ops), len(session.sequences[0].Ops))
	}
	if !strings.Contains(stderr, "deadline") {
		t.Errorf("botbox run printed %q on stderr, want the deadline that ended the pass.", stderr)
	}
	if !strings.Contains(stdout, ops(written)) {
		t.Errorf("botbox run printed %q, want the %s the directory holds.", stdout, ops(written))
	}
	// §11: the shrinker reports the smallest failing sequence it found, which
	// no recording here is of, so it keeps its own file.
	smaller, err := run.ReadSequence(filepath.Join(session.dirs[0], shrunkFile))
	if err != nil {
		t.Fatalf("The pass found a smaller sequence and left no %s: %v", shrunkFile, err)
	}
	if len(smaller.Ops) >= len(written.Ops) {
		t.Errorf("%s holds %d ops, want fewer than the drawn sequence's %d.",
			shrunkFile, len(smaller.Ops), len(written.Ops))
	}
}

// DESIGN.md §11: each failing run writes report.json and report.md beside the
// recordings they describe (§5.7).
func TestAFailingRunWritesItsReport(t *testing.T) {
	violation := run.Violation{ID: "G4", Statement: "the target converges", Evidence: "the settle wait expired"}
	note := "G3 is not evaluated for the deletion of widget"
	session := &fakeSession{results: []run.Result{{Violation: &violation, Notes: []string{note}}}}

	code, _, stderr := invokeWith(t, session, countingGenerator(nil),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("botbox run exited %d: %s", code, stderr)
	}
	for _, name := range []string{report.JSONFile, report.MarkdownFile} {
		if _, err := os.Stat(filepath.Join(session.dirs[0], name)); err != nil {
			t.Fatalf("The run directory holds no %s: %v", name, err)
		}
	}
	written, err := os.ReadFile(filepath.Join(session.dirs[0], report.MarkdownFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{violation.ID, violation.Statement, note, "botbox replay --target " + toyTargetYAML} {
		if !strings.Contains(string(written), want) {
			t.Errorf("The report is\n%s\nwant it to carry %q.", written, want)
		}
	}
}

// The run directory holds the minimized sequence's own run, so a run of it
// that reproduces nothing is worth saying out loud.
func TestAMinimizedSequenceThatPassesOnItsOwnRunIsReported(t *testing.T) {
	violation := run.Violation{ID: "G4"}
	session := &fakeSession{
		results: []run.Result{{Violation: &violation}},
		fails: func(candidate run.Sequence, dir string) *run.Violation {
			if filepath.Base(dir) == shrinkDir && len(candidate.Ops) > 0 {
				return &violation
			}
			return nil
		},
	}
	generate := countingGenerator(nil, run.OpSettle, run.OpRestart)

	code, stdout, stderr := invokeWith(t, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("botbox run exited %d, want %d: the run found a violation.", code, exitViolation)
	}
	if !strings.Contains(stderr, "passed when it ran again") {
		t.Errorf("botbox run reported %q, want the minimized sequence's own run named.", stderr)
	}
	if !strings.Contains(stdout, "G4") {
		t.Errorf("botbox run printed %q, want the violation it found.", stdout)
	}
}

// DESIGN.md §11: the deadline is what the invocation has.
func TestTheDeadlineStopsTheInvocationBetweenRuns(t *testing.T) {
	session := &fakeSession{}

	code, stdout, stderr := invokeWith(t, session, countingGenerator(nil),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3", "--deadline", "1ns")

	if code != exitOK {
		t.Fatalf("botbox run exited %d: %s", code, stderr)
	}
	if len(session.sequences) != 1 {
		t.Errorf("The session executed %d sequences, want the first alone: the deadline had passed.", len(session.sequences))
	}
	if !strings.Contains(stdout, "deadline") {
		t.Errorf("botbox run printed %q, want the deadline named.", stdout)
	}
}

func TestAGeneratorThatFailsExitsTwo(t *testing.T) {
	broken := errors.New("the CRD declares no schema to draw from")
	for _, test := range []struct {
		name         string
		newGenerator func(*target.Target) (Generator, error)
	}{
		{
			name:         "building it",
			newGenerator: func(*target.Target) (Generator, error) { return nil, broken },
		},
		{
			name: "drawing a sequence",
			newGenerator: func(*target.Target) (Generator, error) {
				return func(int64) (run.Sequence, error) { return run.Sequence{}, broken }, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, _, stderr := invokeWith(t, &fakeSession{}, test.newGenerator,
				"run", "--target", toyTargetYAML, "--out", t.TempDir())

			if code != exitError {
				t.Errorf("A generator that failed exited %d, want %d.", code, exitError)
			}
			if !strings.Contains(stderr, "no schema") {
				t.Errorf("botbox run reported %q, want the generator's error.", stderr)
			}
		})
	}
}

func TestReplayExecutesTheNamedSequenceAndPrintsItsSeed(t *testing.T) {
	session := &fakeSession{}
	path := writeSequence(t, 8675309)
	out := t.TempDir()

	code, stdout, stderr := invoke(t, session, "replay", "--target", toyTargetYAML, "--out", out, path)

	if code != exitOK {
		t.Fatalf("botbox replay exited %d: %s", code, stderr)
	}
	if len(session.sequences) != 1 || session.sequences[0].Seed != 8675309 {
		t.Errorf("The session executed %v, want the named sequence.", session.sequences)
	}
	if !strings.Contains(stdout, "8675309") {
		t.Errorf("botbox replay printed %q, want the seed printed.", stdout)
	}
	if !session.closed {
		t.Errorf("botbox replay left the test cluster running.")
	}
}

// A skipped check reads as a passing one from outside, so the run prints what
// it could not judge (DESIGN.md §6).
func TestARunPrintsWhatTheChecksCouldNotJudge(t *testing.T) {
	note := "G3 is not evaluated for the deletion of widget: the run ended before its 10s deadline"
	session := &fakeSession{results: []run.Result{{Notes: []string{note}}}}

	code, stdout, stderr := invoke(t, session, "replay", "--target", toyTargetYAML, "--out", t.TempDir(), writeSequence(t, 1))

	if code != exitOK {
		t.Fatalf("botbox replay exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, note) {
		t.Errorf("botbox replay printed %q, want the note %q.", stdout, note)
	}
}

func TestPassingRunsAreNotPersisted(t *testing.T) {
	session := &fakeSession{}
	out := t.TempDir()

	code, _, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", out, writeSequence(t, 1))

	if code != exitOK {
		t.Fatalf("botbox run exited %d: %s", code, stderr)
	}
	if _, err := os.Stat(session.dirs[0]); !os.IsNotExist(err) {
		t.Errorf("The passing run left %s behind: %v", session.dirs[0], err)
	}
}

func TestAViolationExitsOneAndKeepsTheRunDirectory(t *testing.T) {
	violation := run.Violation{ID: "G3", Statement: "the target deletes what it manages"}
	session := &fakeSession{results: []run.Result{{Violation: &violation}}}
	out := t.TempDir()

	code, stdout, _ := invoke(t, session, "run", "--target", toyTargetYAML, "--out", out, writeSequence(t, 42))

	if code != exitViolation {
		t.Errorf("A run that found a violation exited %d, want %d.", code, exitViolation)
	}
	if !strings.Contains(stdout, "G3") || !strings.Contains(stdout, "42") {
		t.Errorf("botbox run printed %q, want the violation and the seed.", stdout)
	}
	if _, err := os.Stat(session.dirs[0]); err != nil {
		t.Errorf("The failing run's directory is missing: %v", err)
	}
	if !strings.HasSuffix(session.dirs[0], filepath.Join("run-1")) {
		t.Errorf("The failing run wrote to %s, want run-1 under the invocation's directory.", session.dirs[0])
	}
}

func TestAHarnessErrorExitsTwo(t *testing.T) {
	session := &fakeSession{failures: []error{errors.New("the control plane did not start")}}

	code, _, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", t.TempDir(), writeSequence(t, 1))

	if code != exitError {
		t.Errorf("A run that failed exited %d, want %d.", code, exitError)
	}
	if !strings.Contains(stderr, "the control plane did not start") {
		t.Errorf("botbox run reported %q, want the harness error.", stderr)
	}
}

func TestRunStopsAtTheFirstFailingSequence(t *testing.T) {
	violation := run.Violation{ID: "G1"}
	session := &fakeSession{results: []run.Result{{}, {Violation: &violation}}}
	out := t.TempDir()
	first, second, third := writeSequence(t, 1), writeSequence(t, 2), writeSequence(t, 3)

	code, _, _ := invoke(t, session, "run", "--target", toyTargetYAML, "--out", out, first, second, third)

	if code != exitViolation {
		t.Errorf("botbox run exited %d, want %d.", code, exitViolation)
	}
	if len(session.sequences) != 2 {
		t.Errorf("The session executed %d sequences, want it to stop at the failing one and run it as written.",
			len(session.sequences))
	}
}

func TestLaunchArgsAppendToTheTargetsInOrder(t *testing.T) {
	session := &fakeSession{}
	declared := loadToyArgs(t)

	code, _, stderr := invoke(t, session, "replay", "--target", toyTargetYAML, "--out", t.TempDir(),
		"--launch-arg", "--bug=3", "--launch-arg", "--bug=7", writeSequence(t, 1))

	if code != exitOK {
		t.Fatalf("botbox replay exited %d: %s", code, stderr)
	}
	want := append(slices.Clone(declared), "--bug=3", "--bug=7")
	if len(session.args) != 1 || !slices.Equal(session.args[0], want) {
		t.Errorf("The target was launched with %v, want %v: a later flag wins.", session.args, want)
	}
}

func TestExitCodeMapsTheOutcome(t *testing.T) {
	violation := run.Violation{ID: "G1"}
	for _, test := range []struct {
		name   string
		result run.Result
		err    error
		want   int
	}{
		{name: "the run passed", want: exitOK},
		{name: "an invariant failed", result: run.Result{Violation: &violation}, want: exitViolation},
		{name: "the harness failed", err: errors.New("the proxy died"), want: exitError},
		{name: "both", result: run.Result{Violation: &violation}, err: errors.New("the proxy died"), want: exitError},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := exitCode(test.result, test.err); got != test.want {
				t.Errorf("exitCode returned %d, want %d.", got, test.want)
			}
		})
	}
}

func TestParseReadsTheFlagsOfSection11(t *testing.T) {
	opts, _, err := parse([]string{
		"run", "--target", "t.yaml", "--runs", "3", "--seed", "42", "--out", "elsewhere",
		"--deadline", "90s", "--kubeconfig", "kubeconfig", "--launch-arg", "--bug=1",
	})

	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if opts.target != "t.yaml" || opts.runs != 3 || opts.seed != 42 || opts.out != "elsewhere" {
		t.Errorf("parse read %+v.", opts)
	}
	if opts.deadline.String() != "1m30s" || opts.kubeconfig != "kubeconfig" {
		t.Errorf("parse read the deadline %v and the kubeconfig %q.", opts.deadline, opts.kubeconfig)
	}
	if !slices.Equal(opts.launchArgs, []string{"--bug=1"}) {
		t.Errorf("parse read the launch args %v.", opts.launchArgs)
	}
}

func TestParseReadsTheSequenceFiles(t *testing.T) {
	_, sequences, err := parse([]string{"run", "--target", "t.yaml", "a.json", "b.json"})

	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if !slices.Equal(sequences, []string{"a.json", "b.json"}) {
		t.Errorf("parse read the sequences %v.", sequences)
	}
}

func TestParseDefaultsTheDeadlineAndTheOutputDirectory(t *testing.T) {
	opts, _, err := parse([]string{"replay", "--target", "t.yaml", "a.json"})

	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if opts.deadline != defaultDeadline || opts.out != defaultOut {
		t.Errorf("parse defaulted to the deadline %v and the directory %q, want %v and %q.",
			opts.deadline, opts.out, defaultDeadline, defaultOut)
	}
}

func TestTheInvocationDirectoryTakesTheSequenceSeedUnlessOneIsGiven(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "no seed flag", want: "-8675309"},
		{name: "a seed flag", args: []string{"--seed", "7"}, want: "-7"},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := &fakeSession{}
			out := t.TempDir()
			args := append([]string{"run", "--target", toyTargetYAML, "--out", out}, test.args...)

			code, _, stderr := invoke(t, session, append(args, writeSequence(t, 8675309))...)

			if code != exitOK {
				t.Fatalf("botbox run exited %d: %s", code, stderr)
			}
			if invocation := filepath.Base(filepath.Dir(session.dirs[0])); !strings.HasSuffix(invocation, test.want) {
				t.Errorf("The invocation wrote to %s, want a directory ending in %q.", invocation, test.want)
			}
		})
	}
}

func loadToyArgs(t *testing.T) []string {
	t.Helper()
	toy, err := target.Load(toyTargetYAML)
	if err != nil {
		t.Fatalf("Loading the toy target failed: %v", err)
	}
	return toy.Launch.Args
}

func TestARunEvaluatesTheInvariants(t *testing.T) {
	session := &fakeSession{}

	code, _, stderr := invoke(t, session, "replay", "--target", toyTargetYAML, "--out", t.TempDir(), writeSequence(t, 1))

	if code != exitOK {
		t.Fatalf("botbox replay exited %d: %s", code, stderr)
	}
	if len(session.checks) != 1 || session.checks[0] != (run.Engine{}) {
		t.Errorf("The run checked with %v, want the invariant engine.", session.checks)
	}
}
