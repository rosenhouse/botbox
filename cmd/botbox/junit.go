package main

import (
	"encoding/xml"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rosenhouse/botbox/pkg/run"
)

// junitTimestamp is the form JUnit's schema gives a timestamp. botbox writes
// UTC.
const junitTimestamp = "2006-01-02T15:04:05"

type junitSuites struct {
	XMLName  xml.Name   `xml:"testsuites"`
	Name     string     `xml:"name,attr"`
	Tests    int        `xml:"tests,attr"`
	Failures int        `xml:"failures,attr"`
	Errors   int        `xml:"errors,attr"`
	Time     string     `xml:"time,attr"`
	Suite    junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name       string          `xml:"name,attr"`
	Tests      int             `xml:"tests,attr"`
	Failures   int             `xml:"failures,attr"`
	Errors     int             `xml:"errors,attr"`
	Skipped    int             `xml:"skipped,attr"`
	Time       string          `xml:"time,attr"`
	Timestamp  string          `xml:"timestamp,attr"`
	Properties []junitProperty `xml:"properties>property"`
	Cases      []junitCase     `xml:"testcase"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr,omitempty"`
	Failure   *junitProblem `xml:"failure"`
	Error     *junitProblem `xml:"error"`
	Skipped   *junitProblem `xml:"skipped"`
	SystemOut string        `xml:"system-out,omitempty"`
}

type junitProblem struct {
	Type    string `xml:"type,attr,omitempty"`
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

// junit renders the summary as JUnit XML: a testcase per planned run, and one
// named botbox for what stopped the invocation where no run did. dir is the
// invocation's directory.
func (s *summary) junit(dir string) ([]byte, error) {
	suite := junitSuite{
		Name:      s.Target.Name,
		Time:      seconds(s.Finish.Sub(s.Start)),
		Timestamp: s.Start.UTC().Format(junitTimestamp),
		Properties: []junitProperty{
			{Name: "botbox", Value: s.Botbox},
			{Name: "seed", Value: strconv.FormatInt(s.Seed, 10)},
			{Name: "summary", Value: filepath.Join(dir, summaryJSONFile)},
		},
	}
	for _, r := range s.Runs {
		suite.Cases = append(suite.Cases, r.junit(s.Target.Name, dir))
	}
	if s.Error != "" {
		kind := outcomeError
		if s.Outcome == outcomeInterrupted {
			kind = outcomeInterrupted
		}
		suite.Cases = append(suite.Cases, junitCase{Name: "botbox", Classname: s.Target.Name,
			Error: &junitProblem{Type: string(kind), Message: s.Error}})
	}
	for _, c := range suite.Cases {
		suite.Tests++
		switch {
		case c.Failure != nil:
			suite.Failures++
		case c.Error != nil:
			suite.Errors++
		case c.Skipped != nil:
			suite.Skipped++
		}
	}
	encoded, err := xml.MarshalIndent(junitSuites{Name: "botbox", Tests: suite.Tests, Failures: suite.Failures,
		Errors: suite.Errors, Time: suite.Time, Suite: suite}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding the JUnit XML: %w", err)
	}
	return append(append([]byte(xml.Header), encoded...), '\n'), nil
}

func (r runSummary) junit(class, dir string) junitCase {
	name := "seed " + strconv.FormatInt(r.Seed, 10)
	if r.File != "" {
		name = r.File
	}
	c := junitCase{Name: fmt.Sprintf("run %d: %s", r.Run, name), Classname: class, SystemOut: strings.Join(r.Notes, "\n")}
	if r.Duration != nil {
		c.Time = seconds(time.Duration(*r.Duration))
	}
	files := filepath.Join(dir, r.Dir)
	switch r.Outcome {
	case outcomeViolation:
		c.Failure = &junitProblem{Type: r.Violation.ID, Message: r.Violation.Statement,
			Body: fmt.Sprintf("%s\n%s holds the report and the evidence.", r.Violation.Evidence, files)}
	case outcomeError, outcomeInterrupted:
		c.Error = &junitProblem{Type: string(r.Outcome), Message: r.Error}
		if r.Dir != "" {
			c.Error.Body = files + " holds the run's files."
		}
	case outcomeNotRun:
		c.Skipped = &junitProblem{Message: "botbox stopped before this run"}
	}
	return c
}

func seconds(d time.Duration) string { return fmt.Sprintf("%.3f", d.Seconds()) }

// writeJUnit writes the JUnit XML to path. dir is the invocation's directory.
func (s *summary) writeJUnit(path, dir string) error {
	encoded, err := s.junit(dir)
	if err != nil {
		return err
	}
	return run.WriteAtomic(path, encoded)
}
