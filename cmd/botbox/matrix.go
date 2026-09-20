package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
)

// defaultMatrix is the bug matrix of DESIGN.md §9.1.
const defaultMatrix = "docs/bug-matrix.md"

// control is the run without a seeded bug, whose row proves the matrix is not
// vacuous.
const control = 0

// bugSequence names the sequence of one seeded bug.
var bugSequence = regexp.MustCompile(`^b(\d+)\.json$`)

// bugRow is one bug, the sequence that exposes it, and every check that fired.
type bugRow struct {
	bug      int
	file     string
	sequence run.Sequence
	fired    []string
}

// bugMatrix runs one sequence per seeded bug and writes which checks caught it
// (DESIGN.md §9.1).
func (c *cli) bugMatrix(ctx context.Context, opts options) int {
	exercised, err := target.Load(opts.target)
	if err != nil {
		return c.fail(err)
	}
	exercised.Launch.Args = append(exercised.Launch.Args, opts.launchArgs...)
	rows, err := readBugSequences(opts.sequences)
	if err != nil {
		return c.fail(err)
	}
	checks, err := checkIDs(exercised)
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
	dir, err := os.MkdirTemp("", "botbox-matrix-")
	if err != nil {
		return c.fail(fmt.Errorf("creating the matrix's run directory: %w", err))
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(ctx, opts.deadline)
	defer cancel()
	for i := range rows {
		if err := c.exerciseBug(ctx, s, exercised, &rows[i], dir); err != nil {
			return c.fail(err)
		}
	}

	if err := os.WriteFile(opts.out, []byte(matrix(opts.sequences, checks, rows)), 0o644); err != nil {
		return c.fail(fmt.Errorf("writing the matrix: %w", err))
	}
	fmt.Fprintf(c.stdout, "wrote %s\n", opts.out)
	return c.judge(opts, rows)
}

// exerciseBug runs one bug's sequence and records what the checks found over
// the whole run.
func (c *cli) exerciseBug(ctx context.Context, s session, t *target.Target, row *bugRow, dir string) error {
	bugged := *t
	bugged.Launch.Args = append(slices.Clone(t.Launch.Args), fmt.Sprintf("--bug=%d", row.bug))
	result, err := s.execute(ctx, &bugged, row.sequence, filepath.Join(dir, row.name()), observing{})
	if err != nil {
		return fmt.Errorf("%s: %w", row.file, err)
	}
	results, err := run.Evaluate(result.Recorded)
	if err != nil {
		return fmt.Errorf("%s: %w", row.file, err)
	}
	for _, check := range results {
		if len(check.Violations) > 0 {
			row.fired = append(row.fired, check.ID)
		}
	}
	fmt.Fprintf(c.stdout, "%s: %s fired %s\n", row.name(), row.file, row.summary())
	return nil
}

// judge applies the acceptance of DESIGN.md §10 M3: every bug's row names a
// check and the control's row is empty.
func (c *cli) judge(opts options, rows []bugRow) int {
	code := exitOK
	for _, row := range rows {
		wanted := row.bug != control
		if caught := len(row.fired) > 0; caught != wanted {
			code = exitViolation
			fmt.Fprintf(c.stderr, "botbox: %s (%s) fired %s; reproduce it with\n  botbox replay --target %s --launch-arg --bug=%d %s\n",
				row.name(), row.file, row.summary(), opts.target, row.bug, filepath.Join(opts.sequences, row.file))
		}
	}
	return code
}

func (r bugRow) name() string { return "B" + strconv.Itoa(r.bug) }

func (r bugRow) summary() string {
	if len(r.fired) == 0 {
		return "nothing"
	}
	return strings.Join(r.fired, ", ")
}

// observing evaluates nothing, so that a matrix run executes its whole
// sequence instead of ending at the first violation (DESIGN.md §5.5). The
// checks then read the whole recording.
type observing struct{}

func (observing) Check(run.Input) ([]run.Violation, error) { return nil, nil }

// readBugSequences reads one b<id>.json per seeded bug, in bug order.
func readBugSequences(dir string) ([]bugRow, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading the sequences: %w", err)
	}
	var rows []bugRow
	for _, entry := range entries {
		id := bugSequence.FindStringSubmatch(entry.Name())
		if id == nil {
			continue
		}
		bug, err := strconv.Atoi(id[1])
		if err != nil {
			return nil, fmt.Errorf("reading the sequences: %s: %w", entry.Name(), err)
		}
		sequence, err := run.ReadSequence(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		rows = append(rows, bugRow{bug: bug, file: entry.Name(), sequence: sequence})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s holds no b<id>.json sequence", dir)
	}
	slices.SortFunc(rows, func(a, b bugRow) int { return a.bug - b.bug })
	if rows[0].bug != control {
		return nil, fmt.Errorf("%s holds no b0.json: the control row proves the matrix is not vacuous", dir)
	}
	return rows, nil
}

// checkIDs names every check the engine runs, in its order: the generic
// invariants and the target's properties. A run of nothing trips nothing, so
// the results name the checks and hold no violation.
func checkIDs(t *target.Target) ([]string, error) {
	results, err := run.Evaluate(run.Input{Target: t})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(results))
	for i, result := range results {
		ids[i] = result.ID
	}
	return ids, nil
}

// matrix renders the bug catalog against the checks (DESIGN.md §9.1).
func matrix(sequences string, checks []string, rows []bugRow) string {
	var out strings.Builder
	fmt.Fprintf(&out, `# Bug matrix

Which check catches each seeded bug of DESIGN.md §9.1. Generated by
`+"`make bug-matrix`"+` from %s; CI fails if it is stale.

A run ends at its first violation, and the matrix evaluates every check over
the whole recording, so a row lists every check that fired rather than one per
bug: several bugs trip more than one. B0 is the control, the toy with no bug,
and its row is empty.

`, sequences)
	fmt.Fprintf(&out, "| Bug | %s |\n", strings.Join(checks, " | "))
	out.WriteString("|---" + strings.Repeat("|---", len(checks)) + "|\n")
	for _, row := range rows {
		fmt.Fprintf(&out, "| %s |", row.name())
		for _, check := range checks {
			fmt.Fprintf(&out, " %s |", fired(row, check))
		}
		out.WriteString("\n")
	}
	return out.String()
}

func fired(row bugRow, check string) string {
	if slices.Contains(row.fired, check) {
		return "✓"
	}
	return ""
}
