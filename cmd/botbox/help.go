package main

import (
	"flag"
	"fmt"
	"slices"
	"strings"
)

// command is one botbox command. Its flags are what options.flags defines for
// it.
type command struct {
	name     string
	summary  string
	about    string
	operands string
	required []string
	exits    string
	// internal leaves the command out of the top-level help.
	internal bool
}

const runExits = `
Exit codes:
  0      Every run passed.
  1      A check failed. The run's directory holds its report.
  2      botbox could not test the controller. A configuration or harness error
         stopped it, or the deadline did.
  128+N  Signal N stopped botbox. Ctrl-C exits 130.
`

const environment = `
Environment:
  KUBEBUILDER_ASSETS names the directory that holds etcd and kube-apiserver.
  botbox starts them as the test API server, unless --kubeconfig names a
  cluster. The README's Install section shows how to set it.
`

var commands = []command{{
	name:    "run",
	summary: "Draw sequences of ops, run each, and minimize the first that fails.",
	about: `botbox run draws --runs sequences of ops on the target's custom resources and
runs each against a fresh namespace. It stops at the first sequence that fails a
check, minimizes it, and writes a report. Sequence files given as arguments run
as written instead, and botbox does not minimize them.`,
	operands: "[sequence.json...]",
	required: []string{"target"},
	exits:    runExits,
}, {
	name:     "replay",
	summary:  "Run one sequence file again.",
	about:    "botbox replay runs one sequence file as written, such as the sequence.json of a\nfailing run, and writes a report if it fails.",
	operands: "sequence.json",
	required: []string{"target"},
	exits:    runExits,
}, {
	name:    "version",
	summary: "Print botbox's version.",
	about:   "botbox version prints botbox's version.",
}, {
	name: "matrix",
	about: `botbox matrix runs each seeded bug's sequence against the toy controller, under
the bug and without it, and writes which checks fired as Markdown. make
bug-matrix runs it.`,
	required: []string{"target", "sequences"},
	exits: `
Exit codes:
  0      Some check caught every bug, and none fired on the toy with no bug.
  1      No check caught a bug, or a check fired on the toy with no bug.
  2      A configuration or harness error stopped botbox, or the deadline did.
  128+N  Signal N stopped botbox. Ctrl-C exits 130.
`,
	internal: true,
}}

func lookup(name string) (command, bool) {
	i := slices.IndexFunc(commands, func(c command) bool { return c.name == name })
	if i < 0 {
		return command{}, false
	}
	return commands[i], true
}

// flags are the command's required flags, then the rest by name.
func (c command) flags() []*flag.Flag {
	set := (&options{command: c.name}).flags()
	var flags []*flag.Flag
	for _, name := range c.required {
		flags = append(flags, set.Lookup(name))
	}
	set.VisitAll(func(f *flag.Flag) {
		if !slices.Contains(c.required, f.Name) {
			flags = append(flags, f)
		}
	})
	return flags
}

func (c command) synopsis() string {
	words := []string{"botbox", c.name}
	for _, f := range c.flags() {
		word := flagWord(f)
		if !slices.Contains(c.required, f.Name) {
			word = "[" + word + "]"
		}
		if _, repeats := f.Value.(*stringList); repeats {
			word += "..."
		}
		words = append(words, word)
	}
	if c.operands != "" {
		words = append(words, c.operands)
	}
	return strings.Join(words, " ")
}

// flagWord is the flag as a command line spells it, with the back-quoted name
// in its usage as the value.
func flagWord(f *flag.Flag) string {
	value, _ := flag.UnquoteUsage(f)
	return "--" + f.Name + " " + value
}

// help is the command's help, or botbox's where name is no command.
func help(name string) string {
	c, found := lookup(name)
	if !found {
		return intro + "\n" + usage("") + runExits + environment +
			"\nThe README at https://github.com/rosenhouse/botbox has a quickstart.\ndocs/reference.md there lists every key of target.yaml and every op.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Usage:\n  %s\n\n%s\n", c.synopsis(), c.about)
	if flags := c.flags(); len(flags) > 0 {
		b.WriteString("\nFlags:\n")
		for _, f := range flags {
			_, says := flag.UnquoteUsage(f)
			fmt.Fprintf(&b, "  %s\n%s\n", flagWord(f), wrap("      ", says+c.annotation(f)))
		}
	}
	if c.exits != "" {
		b.WriteString(c.exits + environment)
	}
	return b.String()
}

const intro = `botbox finds bugs in a Kubernetes controller. It runs your controller against a
test API server, acts on its custom resources, injects faults and restarts, and
checks that it converges, goes quiet and cleans up.
`

// annotation says the flag is required, or what it defaults to.
func (c command) annotation(f *flag.Flag) string {
	if slices.Contains(c.required, f.Name) {
		return " (required)"
	}
	switch f.DefValue {
	case "", "0", "0s":
		return ""
	}
	return " (default " + f.DefValue + ")"
}

// wrap breaks text into lines of at most 80 columns that begin with indent.
func wrap(indent, text string) string {
	lines := []string{indent}
	for _, word := range strings.Fields(text) {
		last := &lines[len(lines)-1]
		switch {
		case *last == indent:
			*last += word
		case len(*last)+1+len(word) > 80:
			lines = append(lines, indent+word)
		default:
			*last += " " + word
		}
	}
	return strings.Join(lines, "\n")
}

// usage is what botbox prints after a usage error: the command's synopsis, or
// botbox's commands where name is no command.
func usage(name string) string {
	if c, found := lookup(name); found {
		text := "Usage:\n  " + c.synopsis() + "\n"
		if len(c.flags()) > 0 {
			text += "Run 'botbox " + c.name + " --help' for its flags.\n"
		}
		return text
	}
	var b strings.Builder
	b.WriteString("Usage:\n  botbox <command> [flags]\n\nCommands:\n")
	for _, c := range commands {
		if !c.internal {
			fmt.Fprintf(&b, "  %-8s %s\n", c.name, c.summary)
		}
	}
	b.WriteString("\nRun 'botbox <command> --help' or 'botbox help <command>' for a command's flags.\n")
	return b.String()
}
