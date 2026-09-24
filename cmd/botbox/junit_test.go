package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/rosenhouse/botbox/pkg/run"
)

// parsedSuites is a JUnit XML file as a CI server reads it.
type parsedSuites struct {
	XMLName xml.Name      `xml:"testsuites"`
	Suites  []parsedSuite `xml:"testsuite"`
}

type parsedSuite struct {
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Errors   int          `xml:"errors,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Cases    []parsedCase `xml:"testcase"`
}

type parsedCase struct {
	Name      string         `xml:"name,attr"`
	Classname string         `xml:"classname,attr"`
	Failure   *parsedProblem `xml:"failure"`
	Error     *parsedProblem `xml:"error"`
	Skipped   *parsedProblem `xml:"skipped"`
	SystemOut string         `xml:"system-out"`
}

type parsedProblem struct {
	Type    string `xml:"type,attr"`
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

// readJUnit reads the file's one testsuite and the counts it claims.
func readJUnit(t *testing.T, path string) (parsedSuite, map[string]int) {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("botbox wrote no JUnit file: %v", err)
	}
	var written parsedSuites
	if err := xml.Unmarshal(encoded, &written); err != nil {
		t.Fatalf("The JUnit file does not parse: %v\n%s", err, encoded)
	}
	if len(written.Suites) != 1 {
		t.Fatalf("The JUnit file holds %d testsuites, want one:\n%s", len(written.Suites), encoded)
	}
	suite := written.Suites[0]
	return suite, map[string]int{"tests": suite.Tests, "failures": suite.Failures, "errors": suite.Errors, "skipped": suite.Skipped}
}

// counted is what the testcases hold, which the testsuite's counts must say.
func counted(suite parsedSuite) map[string]int {
	counts := map[string]int{"tests": len(suite.Cases), "failures": 0, "errors": 0, "skipped": 0}
	for _, c := range suite.Cases {
		switch {
		case c.Failure != nil:
			counts["failures"]++
		case c.Error != nil:
			counts["errors"]++
		case c.Skipped != nil:
			counts["skipped"]++
		}
	}
	return counts
}

func TestJUnitHasATestcasePerPlannedRun(t *testing.T) {
	g3 := run.Violation{ID: "G3", Statement: "the target deletes what it manages", Evidence: "the v1/ConfigMap widget-0 was still there"}
	note := "G3 is not evaluated for the deletion of widget"
	// XML 1.0 has no escape for a terminal's color codes.
	colored := "the target exited with exit status 2 after writing \"\x1b[31mpanic\x1b[0m\""
	session := &fakeSession{results: []run.Result{{Notes: []string{note, colored}}, {Violation: &g3}}}
	junit := filepath.Join(t.TempDir(), "junit.xml")

	code, _, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3", "--seed", "7", "--junit", junit)

	if code != exitViolation {
		t.Fatalf("botbox run exited %d, want %d: %s", code, exitViolation, stderr)
	}
	suite, claimed := readJUnit(t, junit)
	if want := map[string]int{"tests": 3, "failures": 1, "errors": 0, "skipped": 1}; !maps.Equal(claimed, want) || !maps.Equal(counted(suite), want) {
		t.Errorf("The testsuite claims %v and holds %v, want %v.", claimed, counted(suite), want)
	}
	var names []string
	for _, c := range suite.Cases {
		names = append(names, c.Name)
		if c.Classname != "toy-widget" {
			t.Errorf("The testcase %q is of the class %q, want toy-widget.", c.Name, c.Classname)
		}
	}
	if want := []string{"run 1: seed 7", "run 2: seed 8", "run 3: seed 9"}; !slices.Equal(names, want) {
		t.Fatalf("The testcases are %q, want %q.", names, want)
	}
	if suite.Name != "toy-widget" {
		t.Errorf("The testsuite is %q, want toy-widget.", suite.Name)
	}
	passed, failed, unrun := suite.Cases[0], suite.Cases[1], suite.Cases[2]
	if passed.Failure != nil || passed.Error != nil || passed.Skipped != nil || !strings.Contains(passed.SystemOut, note) {
		t.Errorf("Run 1 reads as %+v, want a pass whose output carries the note %q.", passed, note)
	}
	if f := failed.Failure; f == nil || f.Type != "G3" || f.Message != g3.Statement ||
		!strings.Contains(f.Body, g3.Evidence) || !strings.Contains(f.Body, session.dirs[1]) {
		t.Errorf("Run 2 failed with %+v, want G3, its statement, its evidence and %s.", f, session.dirs[1])
	}
	if unrun.Skipped == nil {
		t.Errorf("Run 3 reads as %+v, want it skipped.", unrun)
	}
}

func TestJUnitCountsWhatStoppedTheInvocationAsAnError(t *testing.T) {
	for _, test := range []struct {
		name    string
		session func(cancel context.CancelCauseFunc) *fakeSession
		args    []string
		// errored is the testcase that errs, and says so in its message.
		errored, kind, message string
		counts                 map[string]int
	}{
		{name: "a run that did not finish",
			session: func(context.CancelCauseFunc) *fakeSession {
				return &fakeSession{failures: []error{nil, errors.New("the control plane did not start")}}
			},
			errored: "run 2: seed 8", kind: "error", message: "the control plane did not start",
			counts: map[string]int{"tests": 3, "failures": 0, "errors": 1, "skipped": 1}},
		{name: "an interrupted run",
			session: func(cancel context.CancelCauseFunc) *fakeSession {
				s := &fakeSession{failures: []error{fmt.Errorf("op 0 (settle): %w", context.Canceled)}}
				s.after = func() { cancel(interrupt{syscall.SIGTERM}) }
				return s
			},
			errored: "run 1: seed 7", kind: "interrupted", message: "an interrupt stopped the run",
			counts: map[string]int{"tests": 3, "failures": 0, "errors": 1, "skipped": 2}},
		{name: "the deadline between runs",
			session: func(context.CancelCauseFunc) *fakeSession { return &fakeSession{} },
			args:    []string{"--deadline", "1ns"},
			errored: "botbox", kind: "error", message: "the --deadline of 1ns stopped the invocation after 1 of 3 runs",
			counts: map[string]int{"tests": 4, "failures": 0, "errors": 1, "skipped": 2}},
		{name: "an interrupt between runs",
			session: func(cancel context.CancelCauseFunc) *fakeSession {
				s := &fakeSession{}
				s.after = func() { cancel(interrupt{syscall.SIGINT}) }
				return s
			},
			errored: "botbox", kind: "interrupted", message: "an interrupt stopped the invocation after 1 of 3 runs",
			counts: map[string]int{"tests": 4, "failures": 0, "errors": 1, "skipped": 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			session := test.session(cancel)
			junit := filepath.Join(t.TempDir(), "junit.xml")
			args := slices.Concat([]string{"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3", "--seed", "7", "--junit", junit}, test.args)

			invokeCtx(t, ctx, session, countingGenerator(nil), args...)

			suite, claimed := readJUnit(t, junit)
			if !maps.Equal(claimed, test.counts) || !maps.Equal(counted(suite), test.counts) {
				t.Errorf("The testsuite claims %v and holds %v, want %v.", claimed, counted(suite), test.counts)
			}
			i := slices.IndexFunc(suite.Cases, func(c parsedCase) bool { return c.Name == test.errored })
			if i < 0 {
				t.Fatalf("The testcases are %+v, want one named %q.", suite.Cases, test.errored)
			}
			if e := suite.Cases[i].Error; e == nil || e.Type != test.kind || !strings.Contains(e.Message, test.message) {
				t.Errorf("%s errs with %+v, want a %s saying %q.", test.errored, e, test.kind, test.message)
			}
			if len(session.dirs) > 0 && strings.HasPrefix(test.errored, "run") &&
				!strings.Contains(suite.Cases[i].Error.Body, session.dirs[len(session.dirs)-1]) {
				t.Errorf("%s errs with %+v, which does not name its directory.", test.errored, suite.Cases[i].Error)
			}
		})
	}
}

func TestAJUnitFileThatCannotBeWrittenOnlyWarns(t *testing.T) {
	junit := filepath.Join(t.TempDir(), "absent", "junit.xml")
	out := t.TempDir()

	code, _, stderr := invoke(t, &fakeSession{}, "replay", "--target", toyTargetYAML, "--out", out, "--junit", junit, writeSequence(t, 1))

	if code != exitOK {
		t.Errorf("botbox replay exited %d, want %d: the run passed.", code, exitOK)
	}
	if !strings.Contains(stderr, "botbox: writing "+junit) {
		t.Errorf("botbox replay printed %q on stderr, want it to say it could not write %s.", stderr, junit)
	}
	if readSummary(t, out).Outcome != "passed" {
		t.Error("The summary does not say the run passed.")
	}
}

func TestJUnitKeepsItsForm(t *testing.T) {
	encoded, err := sampleSummary(t).junit(filepath.Join("botbox-out", "20260924T010203Z-7"))
	if err != nil {
		t.Fatal(err)
	}

	checkGolden(t, "testdata/junit.golden.xml", encoded)
}
