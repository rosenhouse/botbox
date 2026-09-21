package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
)

const toyTargetYAML = "../../targets/toy-widget/target.yaml"

// fakeSession executes nothing: it records what the CLI asked for and answers
// from results.
type fakeSession struct {
	results   []run.Result
	failures  []error
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
	n := len(s.sequences) - 1
	var failure error
	if n < len(s.failures) {
		failure = s.failures[n]
	}
	if n < len(s.results) {
		return s.results[n], failure
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
	var stdout, stderr bytes.Buffer
	c := &cli{
		stdout: &stdout,
		stderr: &stderr,
		open:   func(options, *target.Target) (session, error) { return fake, nil },
	}
	return c.main(t.Context(), args), stdout.String(), stderr.String()
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

func TestRunWithoutASequenceSaysGenerationArrivesInM5(t *testing.T) {
	code, _, stderr := invoke(t, &fakeSession{}, "run", "--target", toyTargetYAML)

	if code != exitError {
		t.Errorf("botbox run without a sequence exited %d, want %d.", code, exitError)
	}
	if !strings.Contains(stderr, "M5") {
		t.Errorf("botbox run without a sequence reported %q, want it to name the milestone that generates one.", stderr)
	}
}

func TestRunsFlagSaysGenerationArrivesInM5(t *testing.T) {
	sequence := writeSequence(t, 1)

	code, _, stderr := invoke(t, &fakeSession{}, "run", "--target", toyTargetYAML, "--runs", "5", sequence)

	if code != exitError || !strings.Contains(stderr, "M5") {
		t.Errorf("botbox run --runs 5 exited %d reporting %q, want a configuration error naming the milestone.", code, stderr)
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
		t.Errorf("The session executed %d sequences, want it to stop at the failing one.", len(session.sequences))
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
	opts, sequences, err := parse([]string{
		"run", "--target", "t.yaml", "--runs", "3", "--seed", "42", "--out", "elsewhere",
		"--deadline", "90s", "--kubeconfig", "kubeconfig", "--launch-arg", "--bug=1", "a.json", "b.json",
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
