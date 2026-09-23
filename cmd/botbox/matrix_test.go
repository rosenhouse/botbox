package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/proxy"
	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
)

// bugSequences writes one sequence per bug id into a new directory.
func bugSequences(t *testing.T, bugs ...int) string {
	t.Helper()
	dir := t.TempDir()
	for _, bug := range bugs {
		sequence := run.Sequence{Seed: int64(bug), Target: "toy-widget", Ops: []run.Op{{Type: run.OpSettle}}}
		if err := run.WriteSequence(filepath.Join(dir, "b"+strconv.Itoa(bug)+".json"), sequence); err != nil {
			t.Fatalf("Writing the test sequence failed: %v", err)
		}
	}
	return dir
}

// recorded is a run the engine reads: its settle wait converged, or expired
// with no fault active while the target repeated a failing request, which are
// a G4 and a G6 violation.
func recorded(t *testing.T, converged bool) run.Result {
	t.Helper()
	toy, err := target.Load(toyTargetYAML)
	if err != nil {
		t.Fatalf("Loading the toy target failed: %v", err)
	}
	start := time.Now()
	result := run.Result{Recorded: run.Input{
		Target: toy,
		Timeline: run.Timeline{
			Ops:         []run.AppliedOp{{Op: run.Op{Index: 0, Type: run.OpSettle}, At: start}},
			Checkpoints: []run.Checkpoint{{At: start.Add(5 * time.Second), Op: 0, Converged: converged}},
		},
	}}
	if converged {
		return result
	}
	for i := range toy.Thresholds.ErrLoop + 1 {
		result.Recorded.Requests = append(result.Recorded.Requests, proxy.Request{
			Start: start.Add(time.Duration(i) * time.Second), Verb: "get", Resource: "configmaps", Name: "widget-0", Status: 404,
		})
	}
	return result
}

// recordedWithAnUnjudgedRestart is a converged run whose restart has no
// converged snapshot after it, so G5 leaves a note instead of a verdict.
func recordedWithAnUnjudgedRestart(t *testing.T) run.Result {
	t.Helper()
	result := recorded(t, true)
	at := result.Recorded.Timeline.Checkpoints[0].At.Add(time.Second)
	result.Recorded.Timeline.Ops = append(result.Recorded.Timeline.Ops,
		run.AppliedOp{Op: run.Op{Index: 1, Type: run.OpRestart}, At: at})
	return result
}

func matrixFile(t *testing.T) string { return filepath.Join(t.TempDir(), "bug-matrix.md") }

func readMatrix(t *testing.T, path string) string {
	t.Helper()
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Reading the matrix failed: %v", err)
	}
	return string(written)
}

func TestMatrixRunsEachSequenceUnderItsBug(t *testing.T) {
	session := &fakeSession{results: []run.Result{recorded(t, true), recorded(t, false), recorded(t, false)}}
	sequences := bugSequences(t, 0, 1, 2)
	out := matrixFile(t)

	code, stdout, stderr := invoke(t, session, "matrix", "--target", toyTargetYAML, "--sequences", sequences, "--out", out)

	if code != exitOK {
		t.Fatalf("botbox matrix exited %d: %s%s", code, stdout, stderr)
	}
	if len(session.sequences) != 3 {
		t.Fatalf("The session executed %d sequences, want one per bug.", len(session.sequences))
	}
	declared := loadToyArgs(t)
	for bug, args := range session.args {
		want := append(slices.Clone(declared), "--bug="+strconv.Itoa(bug))
		if !slices.Equal(args, want) {
			t.Errorf("Bug %d ran with %v, want %v.", bug, args, want)
		}
	}
	if !session.closed {
		t.Errorf("botbox matrix left the test cluster running.")
	}
}

func TestMatrixWritesBugAgainstCheck(t *testing.T) {
	session := &fakeSession{results: []run.Result{recorded(t, true), recorded(t, false)}}
	out := matrixFile(t)

	code, _, stderr := invoke(t, session, "matrix",
		"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 1), "--out", out)

	if code != exitOK {
		t.Fatalf("botbox matrix exited %d: %s", code, stderr)
	}
	written := readMatrix(t, out)
	want := []string{
		"| Bug | G1 | G2 | G3 | G4 | G5 | G6 | P1 |",
		"| B0 |  |  |  |  |  |  |  |",
		"| B1 |  |  |  | ✓ |  | ✓ |  |",
	}
	for _, line := range want {
		if !strings.Contains(written, line) {
			t.Errorf("The matrix is\n%s\nwant the line %q.", written, line)
		}
	}
	if !strings.Contains(written, "every check that fired") {
		t.Errorf("The matrix is\n%s\nwant it to say a row lists every check that fired.", written)
	}
}

// DESIGN.md §6 and D31: a check that could not judge reads like a passing one
// from outside, so the matrix says which cell is which.
func TestMatrixMarksACheckThatLeftSomethingUnjudged(t *testing.T) {
	// The control is the row D31 is about: it reads as empty either way.
	session := &fakeSession{results: []run.Result{recordedWithAnUnjudgedRestart(t), recorded(t, false)}}
	out := matrixFile(t)

	code, stdout, stderr := invoke(t, session, "matrix",
		"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 1), "--out", out)

	if code != exitOK {
		t.Fatalf("botbox matrix exited %d: %s", code, stderr)
	}
	written := readMatrix(t, out)
	if !strings.Contains(written, "| B0 |  |  |  |  | ? |  |  |") {
		t.Errorf("The matrix is\n%s\nwant B0's G5 cell to say it judged nothing.", written)
	}
	if !strings.Contains(written, "left something unjudged") {
		t.Errorf("The matrix is\n%s\nwant it to say what the mark means.", written)
	}
	if !strings.Contains(stdout, "G5 is not evaluated") {
		t.Errorf("botbox matrix printed %q, want the note G5 left.", stdout)
	}
}

// A run note says why a row reads as it does, such as why G3 fired.
func TestMatrixPrintsTheRunsNotes(t *testing.T) {
	control := recorded(t, true)
	note := "botbox's garbage collector never deletes v1/ConfigMap widget-0, because it does not watch v1/Secret, the kind of its owner tls"
	control.Notes = []string{note}
	session := &fakeSession{results: []run.Result{control, recorded(t, false)}}

	code, stdout, stderr := invoke(t, session, "matrix",
		"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 1), "--out", matrixFile(t))

	if code != exitOK {
		t.Fatalf("botbox matrix exited %d: %s", code, stderr)
	}
	if want := "B0: b0.json fired nothing\n  " + note + "\n"; !strings.Contains(stdout, want) {
		t.Errorf("botbox matrix printed %q, want %q.", stdout, want)
	}
}

func TestMatrixListsTheBugsInCatalogOrder(t *testing.T) {
	session := &fakeSession{results: []run.Result{recorded(t, true), recorded(t, false), recorded(t, false)}}
	out := matrixFile(t)

	code, _, stderr := invoke(t, session, "matrix",
		"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 2, 10), "--out", out)

	if code != exitOK {
		t.Fatalf("botbox matrix exited %d: %s", code, stderr)
	}
	var listed []string
	for _, row := range regexp.MustCompile(`(?m)^\| (B\d+) \|`).FindAllStringSubmatch(readMatrix(t, out), -1) {
		listed = append(listed, row[1])
	}
	if want := []string{"B0", "B2", "B10"}; !slices.Equal(listed, want) {
		t.Errorf("The matrix lists %v, want %v: the bugs come in catalog order, not in the order the files are named.",
			listed, want)
	}
}

// The acceptance of DESIGN.md §10 M3: no bug's row is empty, and the control's
// row is.
func TestMatrixFailsOnARowThatBreaksTheAcceptance(t *testing.T) {
	for _, test := range []struct {
		name    string
		results []run.Result
		want    string
	}{
		{
			name:    "a bug nothing caught",
			results: []run.Result{recorded(t, true), recorded(t, true)},
			want:    "B1",
		},
		{
			name:    "a control something caught",
			results: []run.Result{recorded(t, false), recorded(t, false)},
			want:    "B0",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := &fakeSession{results: test.results}
			out := matrixFile(t)

			code, _, stderr := invoke(t, session, "matrix",
				"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 1), "--out", out)

			if code != exitViolation {
				t.Errorf("botbox matrix exited %d, want %d.", code, exitViolation)
			}
			if !strings.Contains(stderr, test.want) {
				t.Errorf("botbox matrix reported %q, want it to name %s.", stderr, test.want)
			}
			if readMatrix(t, out) == "" {
				t.Errorf("botbox matrix wrote no matrix, want the one it judged.")
			}
		})
	}
}

func TestMatrixConfigurationErrorsExitTwo(t *testing.T) {
	for _, test := range []struct {
		name      string
		sequences string
		extra     []string
		want      string
	}{
		{name: "no --sequences", want: "--sequences"},
		{name: "a sequence file as well", sequences: bugSequences(t, 0, 1), extra: []string{"b1.json"}, want: "no sequence file"},
		{name: "a directory that does not exist", sequences: "absent", want: "absent"},
		{name: "a directory holding no sequence", sequences: t.TempDir(), want: "b<id>.json"},
		{name: "no control sequence", sequences: bugSequences(t, 1, 2), want: "b0.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"matrix", "--target", toyTargetYAML, "--out", matrixFile(t)}
			if test.sequences != "" {
				args = append(args, "--sequences", test.sequences)
			}
			args = append(args, test.extra...)

			code, _, stderr := invoke(t, &fakeSession{}, args...)

			if code != exitError {
				t.Errorf("botbox matrix exited %d, want %d.", code, exitError)
			}
			if !strings.Contains(stderr, test.want) {
				t.Errorf("botbox matrix reported %q, want it to name %q.", stderr, test.want)
			}
		})
	}
}

// The sequences the matrix runs: the shortest that exposes each bug, in the
// canonical form of DESIGN.md §7, and needing no fault (§10, M3).
func TestTheSeededBugSequencesAreShortAndFaultless(t *testing.T) {
	const dir = "../../targets/toy-widget/sequences"
	rows, err := readBugSequences(dir)
	if err != nil {
		t.Fatalf("Reading the toy's sequences failed: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("The toy holds %d sequences, want one per seeded bug and a control.", len(rows))
	}
	for _, row := range rows {
		t.Run(row.name(), func(t *testing.T) {
			if row.sequence.Target != "toy-widget" {
				t.Errorf("%s runs the target %q.", row.file, row.sequence.Target)
			}
			if len(row.sequence.Ops) > 6 {
				t.Errorf("%s holds %d ops, want the shortest sequence that exposes the bug.", row.file, len(row.sequence.Ops))
			}
			for _, op := range row.sequence.Ops {
				if op.Type == run.OpFault {
					t.Errorf("%s injects a fault at op %d, and faults arrive in M6.", row.file, op.Index)
				}
			}
			canonical, err := row.sequence.Marshal()
			if err != nil {
				t.Fatalf("Marshalling %s failed: %v", row.file, err)
			}
			written, err := os.ReadFile(filepath.Join(dir, row.file))
			if err != nil {
				t.Fatalf("Reading %s failed: %v", row.file, err)
			}
			if string(written) != string(canonical) {
				t.Errorf("%s is not in the canonical form of §7:\n%s\nwant\n%s", row.file, written, canonical)
			}
		})
	}
}

// A matrix run ends at no violation, so that the whole sequence executes and
// every check the run trips is recorded.
func TestMatrixChecksWithoutEndingTheRun(t *testing.T) {
	session := &fakeSession{results: []run.Result{recorded(t, true), recorded(t, false)}}

	code, _, stderr := invoke(t, session, "matrix",
		"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 1), "--out", matrixFile(t))

	if code != exitOK {
		t.Fatalf("botbox matrix exited %d: %s", code, stderr)
	}
	if len(session.checks) == 0 {
		t.Fatalf("The matrix ran no sequence.")
	}
	for _, check := range session.checks {
		found, err := check.Check(run.Input{})
		if len(found.Violations) > 0 || err != nil {
			t.Errorf("The matrix checked with %v, which reported %v, %v: a matrix run ends at no violation.",
				check, found.Violations, err)
		}
	}
}
