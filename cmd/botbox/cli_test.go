package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/proxy"
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
	// Asking for the usage is not an error, and exit 2 is what tells CI the
	// target or the invocation is broken (DESIGN.md §11).
	for _, asked := range [][]string{{"run", "--help"}, {"--help"}, {"-h"}, {"help"}} {
		code, stdout, stderr := invoke(t, &fakeSession{}, asked...)

		if code != exitOK {
			t.Errorf("botbox %s exited %d, want %d: %s", strings.Join(asked, " "), code, exitOK, stderr)
		}
		if !strings.Contains(stdout, "botbox replay") {
			t.Errorf("botbox %s printed %q, want the usage.", strings.Join(asked, " "), stdout)
		}
	}
}

func TestTheUsageNamesEveryFlag(t *testing.T) {
	for _, command := range []string{"run", "replay", "matrix"} {
		var synopsis string
		for _, line := range strings.Split(usage, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "botbox "+command+" ") {
				synopsis = line
			}
		}
		(&options{command: command}).flags().VisitAll(func(f *flag.Flag) {
			if !strings.Contains(synopsis, "--"+f.Name+" ") {
				t.Errorf("The usage of botbox %s is %q, want it to name --%s.", command, synopsis, f.Name)
			}
		})
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

// A report says the sequence it carries is the one botbox drew, because §5.7
// says a report carries the minimized sequence and this one is not it (D31).
func TestTheReportSaysWhenTheDeadlineLeftTheSequenceUnminimized(t *testing.T) {
	ctx, expire := context.WithCancel(t.Context())
	violation := run.Violation{ID: "G4", Statement: "the target converges"}
	session := &fakeSession{fails: func(run.Sequence, string) *run.Violation { return &violation }}
	// The deadline passes before the first candidate is replayed, so the pass
	// never finds anything smaller.
	session.after = expire
	generate := countingGenerator(nil, run.OpSettle, run.OpSettle, run.OpSettle)

	code, _, stderr := invokeCtx(t, ctx, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("botbox run exited %d, want %d: %s", code, exitViolation, stderr)
	}
	report, err := os.ReadFile(filepath.Join(session.dirs[0], report.MarkdownFile))
	if err != nil {
		t.Fatal(err)
	}
	if want := "the deadline ended minimization"; !strings.Contains(string(report), want) {
		t.Errorf("The report is\n%s\nwant it to say %q.", report, want)
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
	violation := run.Violation{
		ID: "G4", Statement: "the target converges", Evidence: "the settle wait expired",
		At:       time.Date(2026, 9, 21, 5, 59, 8, 980624165, time.UTC),
		Requests: []proxy.Request{{Verb: "get", Path: "/api/v1/namespaces/ns/configmaps/w-0", Status: 404}},
		Versions: []observe.Version{{Key: observe.Key{
			GVK:  schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
			Name: "left-behind",
		}, ResourceVersion: "12"}},
	}
	note := "G3 is not evaluated for the deletion of widget"
	session := &fakeSession{results: []run.Result{{Violation: &violation, Notes: []string{note}}}}

	code, stdout, stderr := invokeWith(t, session, countingGenerator(nil),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42",
		"--kubeconfig", "kind.kubeconfig", "--launch-arg", "--bug=7")

	if code != exitViolation {
		t.Fatalf("botbox run exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "at 2026-09-21T05:59:08.980624165Z") {
		t.Errorf("botbox printed\n%s\nwant the instant the violation carries.", stdout)
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
	want := []string{violation.ID, violation.Statement, note}
	// The command has to carry what selected this run, or it replays something
	// else (DESIGN.md §5.7).
	want = append(want, "botbox replay --target "+toyTargetYAML+" --kubeconfig kind.kubeconfig --launch-arg --bug=7 "+
		filepath.Join(session.dirs[0], sequenceFile))
	// §5.7 also asks for what ran, the seed, the instant it judged, and the
	// evidence the check named.
	want = append(want, version(), "seed 42", "2026-09-21T05:59:08.980624165Z",
		violation.Requests[0].Path, violation.Versions[0].Name)
	for _, want := range want {
		if !strings.Contains(string(written), want) {
			t.Errorf("The report is\n%s\nwant it to carry %q.", written, want)
		}
	}
}

// A run ends at its first violation, so its report says how far it got. The
// ops after it never ran, and a reader who counts them all has been told the
// wrong thing (DESIGN.md §5.7).
func TestTheReportSaysHowFarTheRunGot(t *testing.T) {
	violation := run.Violation{ID: "G4", Statement: "the target converges"}
	session := &fakeSession{results: []run.Result{{
		Violation: &violation,
		Timeline:  run.Timeline{Ops: []run.AppliedOp{{Op: run.Op{Index: 0, Type: run.OpCreate}}}},
	}}}
	generate := countingGenerator(nil, run.OpSettle, run.OpSettle, run.OpSettle)

	code, _, stderr := invokeWith(t, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("botbox run exited %d: %s", code, stderr)
	}
	written, err := os.ReadFile(filepath.Join(session.dirs[0], report.MarkdownFile))
	if err != nil {
		t.Fatal(err)
	}
	if want := "applied 1 of the sequence's 3 ops"; !strings.Contains(string(written), want) {
		t.Errorf("The report is\n%s\nwant it to say %q.", written, want)
	}
}

// A sequence the caller named is never minimized, and it still gets a report.
// This is the path `botbox replay` takes, which is how M6's own fault sequence
// is run.
func TestReplayWritesAReportForTheSequenceItWasGiven(t *testing.T) {
	violation := run.Violation{ID: "G1", Statement: "the target falls quiet"}
	session := &fakeSession{results: []run.Result{{Violation: &violation}}}
	path := writeSequence(t, 8675309)

	code, _, stderr := invoke(t, session, "replay", "--target", toyTargetYAML, "--out", t.TempDir(), path)

	if code != exitViolation {
		t.Fatalf("botbox replay exited %d: %s", code, stderr)
	}
	written, err := os.ReadFile(filepath.Join(session.dirs[0], report.MarkdownFile))
	if err != nil {
		t.Fatalf("The run directory holds no report: %v", err)
	}
	// The command replays the caller's file, which is a better thing to run
	// than a copy botbox made of it.
	for _, want := range []string{violation.ID, "botbox replay --target " + toyTargetYAML + " " + path} {
		if !strings.Contains(string(written), want) {
			t.Errorf("The report is\n%s\nwant it to carry %q.", written, want)
		}
	}
}

// §10 M6's acceptance says the report names the MINIMIZED sequence. Nothing
// else ties the two together: a report carrying the sequence botbox drew would
// send a reader to reproduce the long way round.
func TestTheReportCarriesTheMinimizedSequence(t *testing.T) {
	violation := run.Violation{ID: "G4", Statement: "the target converges"}
	session := &fakeSession{fails: func(candidate run.Sequence, _ string) *run.Violation {
		// Only the restart is needed, so the pass cuts the rest away.
		if slices.ContainsFunc(candidate.Ops, func(op run.Op) bool { return op.Type == run.OpRestart }) {
			return &violation
		}
		return nil
	}}
	generate := countingGenerator(nil, run.OpSettle, run.OpRestart, run.OpSettle)

	code, _, stderr := invokeWith(t, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("botbox run exited %d: %s", code, stderr)
	}
	reported := reportedSequence(t, session.dirs[0])
	if want := []run.OpType{run.OpRestart, run.OpSettle}; !slices.Equal(opTypesOf(reported), want) {
		t.Errorf("The report carries %v, want the minimized %v.", opTypesOf(reported), want)
	}
	written, err := run.ReadSequence(filepath.Join(session.dirs[0], sequenceFile))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(opTypesOf(reported), opTypesOf(written)) {
		t.Errorf("The report carries %v and the directory holds %v; they are one sequence.",
			opTypesOf(reported), opTypesOf(written))
	}
}

// reportedSequence is the sequence report.json embedded.
func reportedSequence(t *testing.T, dir string) run.Sequence {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, report.JSONFile))
	if err != nil {
		t.Fatalf("The run directory holds no report: %v", err)
	}
	var written struct {
		Sequence json.RawMessage `json:"sequence"`
	}
	if err := json.Unmarshal(body, &written); err != nil {
		t.Fatalf("The report does not parse: %v", err)
	}
	sequence, err := run.UnmarshalSequence(written.Sequence)
	if err != nil {
		t.Fatalf("The report's sequence does not parse: %v", err)
	}
	return sequence
}

// A report is the artefact a reader trusts, so it says when the recordings
// beside it are of a run that found nothing (D31, D35).
func TestTheReportSaysWhenTheMinimizedSequenceDidNotReproduce(t *testing.T) {
	violation := run.Violation{ID: "G4", Statement: "the target converges"}
	session := &fakeSession{
		results: []run.Result{{Violation: &violation}},
		// Only the first run fails, so the shrink pass minimizes and the run of
		// what it found passes.
		fails: func(candidate run.Sequence, dir string) *run.Violation {
			if strings.Contains(dir, shrinkDir) && len(candidate.Ops) < 3 {
				return &violation
			}
			return nil
		},
	}
	generate := countingGenerator(nil, run.OpSettle, run.OpSettle, run.OpSettle)

	code, _, _ := invokeWith(t, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("botbox run exited %d, want %d.", code, exitViolation)
	}
	written, err := os.ReadFile(filepath.Join(session.dirs[0], report.MarkdownFile))
	if err != nil {
		t.Fatalf("The run directory holds no report: %v", err)
	}
	if !strings.Contains(string(written), "passed when it ran again") {
		t.Errorf("The report is\n%s\nwant it to say the recordings are of a run that found nothing.", written)
	}
	// The counts describe the sequence the directory holds a run of, or they
	// describe two sequences and one of them is a fiction (DESIGN.md §5.7).
	encoded, err := os.ReadFile(filepath.Join(session.dirs[0], report.JSONFile))
	if err != nil {
		t.Fatal(err)
	}
	var carried struct{ Applied, Ops int }
	if err := json.Unmarshal(encoded, &carried); err != nil {
		t.Fatal(err)
	}
	if carried.Applied > carried.Ops {
		t.Errorf("The report says the run applied %d of %d ops.", carried.Applied, carried.Ops)
	}
}

// The shrink pass weakens a fault as well as removing ops, so a sequence it
// changed without shortening is still one the report has to carry.
func TestASequenceWeakenedButNotShortenedCountsAsSimpler(t *testing.T) {
	failing := run.Sequence{Seed: 1, Target: "toy-widget", Ops: []run.Op{
		{Index: 0, Type: run.OpFault, Fault: &run.Fault{Action: run.Action{Error: 500}, Until: run.Trigger{Count: 8}}},
		{Index: 1, Type: run.OpSettle},
	}}
	weakened := failing
	weakened.Ops = slices.Clone(failing.Ops)
	weakened.Ops[0].Fault = &run.Fault{Action: run.Action{Error: 500}, Until: run.Trigger{Count: 4}}

	if !simpler(weakened, failing) {
		t.Error("A fault weakened from a count of 8 to 4 reads as the sequence botbox drew.")
	}
	if simpler(failing, failing) {
		t.Error("The sequence botbox drew reads as simpler than itself.")
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

// A run the deadline cut short says so in the flag's own terms, because Go's
// phrase for it names nothing the caller set (DESIGN.md §11).
func TestADeadlineThatEndsARunNamesTheFlag(t *testing.T) {
	for _, failure := range []struct {
		name     string
		deadline string
		err      error
		names    bool
	}{
		{"the deadline", "1ns", fmt.Errorf("op 0 (create): %w", context.DeadlineExceeded), true},
		{"a target that stopped", "1ns", errors.New("op 0 (create): the target is no longer running"), false},
		// The teardown runs on a budget of its own, which no flag names
		// (DESIGN.md §5.5).
		{"a deadline the flag did not set", "5s", fmt.Errorf("stopping: %w", context.DeadlineExceeded), false},
	} {
		t.Run(failure.name, func(t *testing.T) {
			session := &fakeSession{failures: []error{failure.err}}

			code, _, stderr := invokeWith(t, session, countingGenerator(nil),
				"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--deadline", failure.deadline)

			if code != exitError {
				t.Fatalf("botbox run exited %d, want %d.", code, exitError)
			}
			if named := strings.Contains(stderr, "the --deadline of "+failure.deadline); named != failure.names {
				t.Errorf("botbox run printed %q, and what ended the run was %s.", stderr, failure.name)
			}
		})
	}
}

// DESIGN.md §11: the deadline is what the invocation has. An invocation it
// ended early tested less than asked, so it does not pass.
func TestADeadlineThatStopsTheInvocationBetweenRunsExitsTwo(t *testing.T) {
	session := &fakeSession{}

	code, stdout, stderr := invokeWith(t, session, countingGenerator(nil),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3", "--deadline", "1ns")

	if code != exitError {
		t.Errorf("botbox run exited %d, want %d.", code, exitError)
	}
	if len(session.sequences) != 1 {
		t.Errorf("The session executed %d sequences, want the first alone: the deadline had passed.", len(session.sequences))
	}
	if want := "the --deadline of 1ns stopped the invocation after 1 of 3 runs"; !strings.Contains(stderr, want) {
		t.Errorf("botbox run printed %q on stderr, want %q.", stderr, want)
	}
	if strings.Contains(stdout, "every run passed") {
		t.Errorf("botbox run printed %q, but not every run ran.", stdout)
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

// shellWords splits a command line the way sh does.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	printed, err := exec.Command("sh", "-c", `printf '%s\0' `+line).Output()
	if err != nil {
		t.Fatalf("sh could not split %q: %v", line, err)
	}
	return strings.Split(strings.TrimSuffix(string(printed), "\x00"), "\x00")
}

func TestTheReplayCommandParsesBackToWhatRan(t *testing.T) {
	for _, test := range []struct {
		name string
		ran  options
		path string
	}{
		{name: "the target alone", ran: options{target: "t.yaml"}, path: "s.json"},
		{name: "a kubeconfig", ran: options{target: "t.yaml", kubeconfig: "/home/me/.kube/config"}, path: "s.json"},
		{name: "launch args", ran: options{target: "t.yaml", launchArgs: []string{"--bug=3", "--mode=a=b", "--bug=7"}}, path: "s.json"},
		{name: "a kubeconfig and launch args", ran: options{target: "t.yaml", kubeconfig: "k", launchArgs: []string{"--x=1"}}, path: "s.json"},
		{name: "a path with a space", ran: options{target: "my target.yaml"}, path: "botbox-out/run 1/sequence.json"},
		{name: "what a shell would expand", ran: options{target: "$HOME/t.yaml", launchArgs: []string{"--name=it's", "*"}}, path: "`s`.json"},
		{name: "an empty launch arg", ran: options{target: "t.yaml", launchArgs: []string{""}}, path: "s.json"},
		{name: "a path that reads as a flag", ran: options{target: "t.yaml"}, path: "-seqs/b1.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			line := test.ran.replayCommand(test.path)

			words := shellWords(t, line)
			if words[0] != "botbox" {
				t.Fatalf("The replay command %q runs %q.", line, words[0])
			}
			replay, sequences, err := parse(words[1:])

			if err != nil {
				t.Fatalf("botbox cannot parse the replay command %q: %v", line, err)
			}
			if replay.command != "replay" || replay.target != test.ran.target || replay.kubeconfig != test.ran.kubeconfig ||
				!slices.Equal(replay.launchArgs, test.ran.launchArgs) || len(sequences) != 1 || filepath.Clean(sequences[0]) != test.path {
				t.Errorf("The replay command %q parses to %s %q, kubeconfig %q, launch args %q, sequences %q; want replay %q, %q, %q, [%q].",
					line, replay.command, replay.target, replay.kubeconfig, replay.launchArgs, sequences,
					test.ran.target, test.ran.kubeconfig, test.ran.launchArgs, test.path)
			}
		})
	}
}

// zsh expands a word that begins with =, and sh does not.
func TestTheReplayCommandQuotesAWordZshWouldExpand(t *testing.T) {
	line := options{target: "t.yaml", launchArgs: []string{"=x"}}.replayCommand("s.json")

	if !strings.Contains(line, "'=x'") {
		t.Errorf("The replay command is %q, want =x quoted.", line)
	}
}

// literalBytes are the bytes sh and zsh read as themselves, but for a leading =.
const literalBytes = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_./=:@%+,-"

func TestTheReplayCommandQuotesEveryByteAShellInterprets(t *testing.T) {
	for c := byte(0); c < 0x7f; c++ {
		if (c < ' ' && c != '\t' && c != '\n') || strings.IndexByte(literalBytes, c) >= 0 {
			continue
		}
		for _, word := range []string{string(c), "x" + string(c)} {
			if quoted := shellQuote(word); !strings.HasPrefix(quoted, "'") {
				t.Errorf("shellQuote(%q) is %q, want it single-quoted.", word, quoted)
			}
		}
	}
}

// A new flag either changes what a run executes, and the replay command
// carries it, or it does not. This test makes its author say which.
func TestEveryFlagIsReplayedOrSelectsNothing(t *testing.T) {
	replayed := []string{"target", "kubeconfig", "launch-arg"}
	inert := []string{"runs", "seed", "out", "deadline", "sequences"}
	for _, command := range []string{"run", "replay", "matrix"} {
		(&options{command: command}).flags().VisitAll(func(f *flag.Flag) {
			if !slices.Contains(replayed, f.Name) && !slices.Contains(inert, f.Name) {
				t.Errorf("botbox %s --%s is neither replayed nor inert: replayCommand must carry it, or this test must say it selects nothing.",
					command, f.Name)
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

// The state and its count reach report.json, and not only the line the CLI
// prints (#13, #24).
func TestTheReportQuotesTheStateAtTheVerdict(t *testing.T) {
	for _, c := range []struct {
		name  string
		state []observe.Version
	}{
		{name: "the state the verdict quoted", state: []observe.Version{{Key: observe.Key{
			GVK:  schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
			Name: "widget-0",
		}, ResourceVersion: "12"}}},
		{name: "a target that managed nothing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			managed := len(c.state)
			violation := run.Violation{ID: "G4", Statement: "the target converges",
				Managed: c.state, ManagedTotal: &managed}
			session := &fakeSession{results: []run.Result{{Violation: &violation}}}

			code, _, stderr := invokeWith(t, session, countingGenerator(nil, run.OpSettle),
				"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

			if code != exitViolation {
				t.Fatalf("botbox run exited %d, want %d: %s", code, exitViolation, stderr)
			}
			encoded, err := os.ReadFile(filepath.Join(session.dirs[0], report.JSONFile))
			if err != nil {
				t.Fatal(err)
			}
			var carried struct {
				Managed      []observe.Version
				ManagedTotal *int
			}
			if err := json.Unmarshal(encoded, &carried); err != nil {
				t.Fatal(err)
			}
			if len(carried.Managed) != managed {
				t.Errorf("report.json is\n%s\nwant the %d objects the violation quoted.", encoded, managed)
			}
			if carried.ManagedTotal == nil || *carried.ManagedTotal != managed {
				t.Errorf("report.json is\n%s\nwant the count of the objects the target managed.", encoded)
			}
		})
	}
}

// The report says what the check's own bound left out, which only the check
// knows (#22).
func TestTheReportSaysWhatTheChecksBoundLeftOut(t *testing.T) {
	violation := run.Violation{
		ID: "G4", Statement: "the target converges",
		Requests:      []proxy.Request{{Verb: "get", Path: "/api/v1/widgets"}},
		RequestsTotal: 133,
		Versions:      []observe.Version{{Key: observe.Key{Name: "widget"}, ResourceVersion: "11"}},
		VersionsTotal: 41,
		VersionsOf:    "toy.botbox/v1/Widget widget",
	}
	session := &fakeSession{results: []run.Result{{Violation: &violation}}}

	code, _, stderr := invokeWith(t, session, countingGenerator(nil, run.OpSettle),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("botbox run exited %d, want %d: %s", code, exitViolation, stderr)
	}
	encoded, err := os.ReadFile(filepath.Join(session.dirs[0], report.JSONFile))
	if err != nil {
		t.Fatal(err)
	}
	var carried struct {
		RequestsTotal, VersionsTotal int
		VersionsOf                   string
	}
	if err := json.Unmarshal(encoded, &carried); err != nil {
		t.Fatal(err)
	}
	if carried.VersionsTotal != 41 || carried.RequestsTotal != 133 {
		t.Errorf("report.json says it chose from %d requests and %d versions, want the 133 and 41 the check did.",
			carried.RequestsTotal, carried.VersionsTotal)
	}
	if carried.VersionsOf != violation.VersionsOf {
		t.Errorf("report.json says the timeline is of %q, want %q.", carried.VersionsOf, violation.VersionsOf)
	}
}
