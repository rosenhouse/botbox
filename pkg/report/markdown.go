package report

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/observe"
	"github.com/rosenhouse/botbox/pkg/proxy"
)

// markdown renders report.md. It opens with what failed and the command that
// reproduces it, so that the first lines are enough to act on.
func (d document) markdown() []byte {
	var md strings.Builder
	fmt.Fprintf(&md, "# %s failed on %s\n", d.Check.ID, d.Target.describe())
	fmt.Fprintf(&md, "\n%s\n", d.Check.Statement)
	if d.Check.Evidence != "" {
		fmt.Fprintf(&md, "\n%s\n", d.Check.Evidence)
	}
	fmt.Fprintf(&md, "\n```sh\n%s\n```\n", d.Replay)
	fmt.Fprintf(&md, "\n%s The run directory holds `requests.jsonl`, `objects.jsonl` and `target.log`.\n", d.provenance())
	if len(d.Notes) > 0 {
		md.WriteString("\n## Notes\n\n")
		for _, note := range d.Notes {
			fmt.Fprintf(&md, "- %s\n", note)
		}
	}
	fmt.Fprintf(&md, "\n## Sequence\n\n```json\n%s\n```\n", strings.TrimRight(string(d.Sequence), "\n"))
	if len(d.Requests) > 0 {
		md.WriteString("\n## Requests\n\n")
		md.WriteString(quotedLine(d.RequestsTotal, len(d.Requests), "request", "requests.jsonl"))
		table(&md, []string{"start", "verb", "path", "status", "fault"}, requestRows(d.Requests))
	}
	if len(d.Versions) > 0 {
		md.WriteString("\n## Object versions\n\n")
		md.WriteString(quotedLine(d.VersionsTotal, len(d.Versions), "version", "objects.jsonl"))
		table(&md, []string{"time", "kind", "name", "resourceVersion", "finalizers", "deleted"}, versionRows(d.Versions))
	}
	return []byte(md.String())
}

// quotedLine says how much evidence the check found, and names what the bound
// of maxEvidence left out.
func quotedLine(total, shown int, noun, recording string) string {
	line := fmt.Sprintf("The check quoted %s.", count(total, noun))
	if shown < total {
		line += fmt.Sprintf(" The first %d are below.", shown)
	}
	return line + fmt.Sprintf(" `%s` holds them all.\n\n", recording)
}

// count writes a number of things, in the singular where there is one.
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func requestRows(requests []proxy.Request) [][]string {
	rows := make([][]string, len(requests))
	for i, request := range requests {
		rows[i] = []string{
			stamp(request.Start), request.Verb, request.Path,
			strconv.Itoa(request.Status), request.Fault,
		}
	}
	return rows
}

func versionRows(versions []observe.Version) [][]string {
	rows := make([][]string, len(versions))
	for i, version := range versions {
		rows[i] = []string{
			stamp(version.Time), kindName(version.GVK), version.Name, version.ResourceVersion,
			strings.Join(version.Finalizers, ", "), yes(version.Deleted),
		}
	}
	return rows
}

func table(md *strings.Builder, header []string, rows [][]string) {
	fmt.Fprintf(md, "| %s |\n", strings.Join(header, " | "))
	fmt.Fprintf(md, "|%s\n", strings.Repeat(" --- |", len(header)))
	for _, row := range rows {
		fmt.Fprintf(md, "| %s |\n", strings.Join(row, " | "))
	}
}

// provenance says what ran the sequence, in the versions each declares.
func (d document) provenance() string {
	if d.Botbox == "" {
		return fmt.Sprintf("The run exercised %s on seed %d.", d.Target.describe(), d.Seed)
	}
	return fmt.Sprintf("botbox %s exercised %s on seed %d.", d.Botbox, d.Target.describe(), d.Seed)
}

// describe names the target and the version it declares (DESIGN.md §8.1).
func (t Target) describe() string {
	if t.Version == "" {
		return t.Name
	}
	return t.Name + " " + t.Version
}

// stamp writes a time as the request log and the object history do.
func stamp(at time.Time) string { return at.Format(time.RFC3339Nano) }

// kindName writes a kind as DESIGN.md §8.1 declares it.
func kindName(gvk schema.GroupVersionKind) string {
	if gvk.Group == "" {
		return gvk.Version + "/" + gvk.Kind
	}
	return gvk.Group + "/" + gvk.Version + "/" + gvk.Kind
}

func yes(set bool) string {
	if set {
		return "yes"
	}
	return ""
}
