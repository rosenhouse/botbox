// Package report writes what a failing run found, as report.json and
// report.md beside the run's recordings (DESIGN.md §5.7). The caller fills in
// a Report, so that nothing here depends on the Runner.
package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/proxy"
)

// A failing run writes these two files (DESIGN.md §11).
const (
	JSONFile     = "report.json"
	MarkdownFile = "report.md"
)

// maxEvidence bounds each excerpt. A run makes thousands of requests, and the
// first of them are where the failure starts. The report says how many the
// check quoted, and the run's recordings hold every one.
const maxEvidence = 20

// Report is one failing run.
type Report struct {
	Check  Check
	Target Target
	// Seed is the sequence's seed, which the run is reproducible from
	// (DESIGN.md §11).
	Seed int64
	// Replay is the one-line command that re-executes the sequence.
	Replay string
	// Sequence is the minimized sequence in the canonical form of
	// DESIGN.md §7, as run.Sequence.Marshal writes it.
	Sequence json.RawMessage
	// Requests and Versions are the evidence the check quoted. The report
	// bounds each excerpt and says how many there were.
	Requests []proxy.Request
	Versions []observe.Version
}

// Check is the invariant or property the run broke (DESIGN.md §6).
type Check struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
	Evidence  string `json:"evidence,omitempty"`
}

// Target is the controller the run exercised (DESIGN.md §8.1).
type Target struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// Write writes report.json and report.md into dir, creating it if it is not
// there. A report that does not encode writes neither file.
func Write(dir string, r Report) error {
	doc := r.document()
	encoded, err := doc.marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating the report's directory: %w", err)
	}
	for _, file := range []struct {
		name    string
		content []byte
	}{{JSONFile, encoded}, {MarkdownFile, doc.markdown()}} {
		if err := os.WriteFile(filepath.Join(dir, file.name), file.content, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", file.name, err)
		}
	}
	return nil
}

// document is the report as both files render it: the evidence bounded, and
// the totals that say what the bound left out. Field order is report.json's.
type document struct {
	Check         Check             `json:"check"`
	Target        Target            `json:"target"`
	Seed          int64             `json:"seed"`
	Replay        string            `json:"replay"`
	Sequence      json.RawMessage   `json:"sequence"`
	Requests      []proxy.Request   `json:"requests,omitempty"`
	RequestsTotal int               `json:"requestsTotal,omitempty"`
	Versions      []observe.Version `json:"versions,omitempty"`
	VersionsTotal int               `json:"versionsTotal,omitempty"`
}

func (r Report) document() document {
	return document{
		Check:         r.Check,
		Target:        r.Target,
		Seed:          r.Seed,
		Replay:        r.Replay,
		Sequence:      r.Sequence,
		Requests:      r.Requests[:min(len(r.Requests), maxEvidence)],
		RequestsTotal: len(r.Requests),
		Versions:      timeline(r.Versions),
		VersionsTotal: len(r.Versions),
	}
}

// timeline quotes when each version appeared and what it carried, and drops
// the object bodies that objects.jsonl holds in full.
func timeline(versions []observe.Version) []observe.Version {
	excerpt := slices.Clone(versions[:min(len(versions), maxEvidence)])
	for i := range excerpt {
		excerpt[i].Object = nil
	}
	return excerpt
}

// marshal renders report.json: two spaces of indentation and a trailing
// newline, as a sequence file has. HTML escaping stays off, so that a CEL
// statement's < and & read as themselves (DESIGN.md §8.4).
func (d document) marshal() ([]byte, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(d); err != nil {
		return nil, fmt.Errorf("encoding the report: %w", err)
	}
	return encoded.Bytes(), nil
}
