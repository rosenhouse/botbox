package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/rosenhouse/botbox/pkg/run"
)

// summaryMarkdownFile is summary.json for a person, such as a CI job's page.
const summaryMarkdownFile = "summary.md"

func (s *summary) markdown() []byte {
	var md strings.Builder
	name := s.Target.Name
	if s.Target.Version != "" {
		name += " " + s.Target.Version
	}
	fmt.Fprintf(&md, "# botbox %s on %s: %s\n\n%s\n", s.Command, name, s.Outcome, s.provenance())
	if s.Error != "" {
		fmt.Fprintf(&md, "\nbotbox: %s\n", s.Error)
	}
	md.WriteString("\n| run | seed | sequence | outcome | ops applied | faults applied | exits | took |\n")
	md.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, r := range s.Runs {
		fmt.Fprintf(&md, "| %d | %d | %s | %s | %d of %d | %s | %d | %s |\n",
			r.Run, r.Seed, r.source(), r.verdict(), r.Applied, r.Ops, r.faults(), len(r.Exits), r.took())
	}
	for _, r := range s.Runs {
		r.details(&md)
	}
	return []byte(md.String())
}

func (s *summary) provenance() string {
	ran := 0
	for _, r := range s.Runs {
		if r.Outcome != outcomeNotRun {
			ran++
		}
	}
	said := fmt.Sprintf("botbox %s ran %d of %d runs from seed %d on %s", s.Botbox, ran, len(s.Runs), s.Seed, s.Cluster)
	if len(s.LaunchArgs) > 0 {
		args := make([]string, len(s.LaunchArgs))
		for i, arg := range s.LaunchArgs {
			args[i] = "--launch-arg " + shellQuote(arg)
		}
		said += ", with `" + strings.Join(args, " ") + "`"
	}
	said += fmt.Sprintf(". It took %s", s.Finish.Sub(s.Start).Round(time.Millisecond))
	switch {
	case s.Deadline == 0:
	case s.DeadlineDerived:
		said += fmt.Sprintf(" of a derived deadline of %s", time.Duration(s.Deadline))
	default:
		said += fmt.Sprintf(" of the --deadline of %s", time.Duration(s.Deadline))
	}
	return said + fmt.Sprintf(", and exited %d.", s.ExitCode)
}

func (r runSummary) source() string {
	if r.File == "" {
		return "drawn"
	}
	return "`" + strings.ReplaceAll(r.File, "|", `\|`) + "`"
}

// verdict is the check a run failed, or else its outcome.
func (r runSummary) verdict() string {
	if r.Violation != nil {
		return r.Violation.ID
	}
	return string(r.Outcome)
}

func (r runSummary) faults() string {
	planned := r.OpTypes[run.OpFault]
	if planned == 0 {
		return "none"
	}
	applied := fmt.Sprintf("%d of %d", r.FaultsApplied, planned)
	if r.FaultedRequests > 0 {
		applied += fmt.Sprintf(", to %d requests", r.FaultedRequests)
	}
	return applied
}

func (r runSummary) took() string {
	if r.Duration == nil {
		return ""
	}
	return time.Duration(*r.Duration).String()
}

// details says what a run found, why it stopped, and what its checks could
// not judge.
func (r runSummary) details(md *strings.Builder) {
	if r.Violation == nil && r.Error == "" && len(r.Notes) == 0 {
		return
	}
	fmt.Fprintf(md, "\n## Run %d: %s\n", r.Run, r.verdict())
	switch {
	case r.Violation != nil:
		fmt.Fprintf(md, "\n%s\n", r.Violation.Statement)
		if r.Violation.Evidence != "" {
			fmt.Fprintf(md, "\n%s\n", r.Violation.Evidence)
		}
		fmt.Fprintf(md, "\n`%s/` holds the report and the evidence.\n", r.Dir)
	case r.Error != "":
		fmt.Fprintf(md, "\n%s\n", r.Error)
		if r.Dir != "" {
			fmt.Fprintf(md, "\n`%s/` holds the run's files.\n", r.Dir)
		}
	}
	if len(r.Notes) > 0 {
		md.WriteString("\n")
		for _, note := range r.Notes {
			fmt.Fprintf(md, "- %s\n", note)
		}
	}
}
