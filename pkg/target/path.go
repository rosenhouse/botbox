package target

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Path names fields of an object, as equalIgnore writes them: plain keys joined
// by dots, ["key"] for any other key, and [*] for every item of a list or value
// of a map.
type Path []Step

// Step is one map key or, when Each is set, every item or value.
type Step struct {
	Key  string
	Each bool
}

// ParsePath reads a path in the form String prints.
func ParsePath(text string) (Path, error) {
	var path Path
	for i := 0; ; {
		start, rest := i, text[i:]
		var step Step
		var err error
		switch {
		case path.MapsToStrings() && !strings.HasPrefix(rest, "[") && strings.ContainsAny(rest, "./"):
			return nil, at(text, i, "%s maps keys to strings, so %s is one key; quote it, as in %s"+inBlockStyle,
				path, rest, append(path, Step{Key: rest}))
		case strings.HasPrefix(rest, "[") && !strings.HasSuffix(text[:i], "."):
			step, i, err = bracket(text, i)
		default:
			step, i, err = bare(text, i, path)
		}
		switch {
		case err != nil:
			return nil, err
		case step.Each && len(path) == 0:
			return nil, at(text, start, "a path starts with a key")
		case path.MapsToStrings() && i < len(text):
			return nil, at(text, start, "%s maps keys to strings, so the path ends one step below it", path)
		}
		path = append(path, step)
		switch {
		case i == len(text):
			return path, nil
		case text[i] == '.':
			i++
		case text[i] != '[':
			return nil, at(text, i, "want . or [ after ]")
		}
	}
}

// inBlockStyle ends a message that suggests brackets, which end a one-line
// YAML list.
const inBlockStyle = "; a path with brackets goes in a block-style list"

// MustParsePath is ParsePath for a path known to be valid.
func MustParsePath(text string) Path {
	path, err := ParsePath(text)
	if err != nil {
		panic(err)
	}
	return path
}

// MapsToStrings reports whether p ends at the labels or the annotations of
// some metadata, one step above where a path ends.
func (p Path) MapsToStrings() bool {
	n := len(p)
	return n >= 2 && p[n-2] == Step{Key: "metadata"} &&
		(p[n-1] == Step{Key: "labels"} || p[n-1] == Step{Key: "annotations"})
}

// bare reads the key at text[i], which runs to the next . or [.
func bare(text string, i int, path Path) (Step, int, error) {
	end := strings.IndexAny(text[i:], ".[")
	if end < 0 {
		end = len(text) - i
	}
	key := text[i : i+end]
	switch {
	case key == "":
		return Step{}, 0, at(text, i, "want a key")
	case key == "*":
		return Step{}, 0, at(text, i, "use [*] for every item of a list or value of a map"+inBlockStyle)
	case strings.Contains(key, "/"):
		return Step{}, 0, at(text, i, `%s holds a /, so it likely ends a key that the dots split; quote the whole key, as in ["example.com/name"]`+inBlockStyle, key)
	case !isBare(key):
		return Step{}, 0, at(text, i, "quote the key, as in %s"+inBlockStyle, append(path, Step{Key: key}))
	}
	return Step{Key: key}, i + end, nil
}

// isBare reports whether a key reads back without quotes.
func isBare(key string) bool {
	return key != "" && !strings.ContainsFunc(key, func(r rune) bool {
		return !unicode.IsPrint(r) || strings.ContainsRune(`.[]"*/: `, r)
	})
}

// bracket reads the [*] or ["key"] at text[i], and returns where it ends.
func bracket(text string, i int) (Step, int, error) {
	rest := text[i:]
	if strings.HasPrefix(rest, "[*]") {
		return Step{Each: true}, i + 3, nil
	}
	if strings.HasPrefix(rest, `["`) {
		end := closingQuote(rest)
		if end < 0 {
			return Step{}, 0, at(text, i, "the quoted key is not closed")
		}
		var key string
		if err := json.Unmarshal([]byte(rest[1:end+1]), &key); err != nil {
			return Step{}, 0, at(text, i, "the quoted key is not a JSON string: %v", err)
		}
		if !strings.HasPrefix(rest[end+1:], "]") {
			return Step{}, 0, at(text, i+end+1, "want ] after the quoted key")
		}
		return Step{Key: key}, i + end + 2, nil
	}
	if index, _, closed := strings.Cut(rest[1:], "]"); closed {
		if _, err := strconv.Atoi(index); err == nil {
			return Step{}, 0, at(text, i, "use [*]; an item's position in a list can change across a restart")
		}
	}
	return Step{}, 0, at(text, i, `want [*] or ["key"]`)
}

// closingQuote returns the index of the quote that closes the one at rest[1].
func closingQuote(rest string) int {
	for j := 2; j < len(rest); j++ {
		switch rest[j] {
		case '\\':
			j++
		case '"':
			return j
		}
	}
	return -1
}

// at reports a problem at the character that begins text[i:].
func at(text string, i int, format string, args ...any) error {
	return fmt.Errorf("offset %d: %s", utf8.RuneCountInString(text[:i]), fmt.Sprintf(format, args...))
}

// String prints the path so that ParsePath reads it back.
func (p Path) String() string {
	var b strings.Builder
	for i, step := range p {
		switch {
		case step.Each:
			b.WriteString("[*]")
		case isBare(step.Key):
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(step.Key)
		default:
			fmt.Fprintf(&b, "[%s]", quote(step.Key))
		}
	}
	return b.String()
}

func quote(key string) string {
	var b strings.Builder
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(key) // A string always encodes.
	return strings.TrimSuffix(b.String(), "\n")
}

// Remove deletes the fields p names from object. A map or a list along p that
// is left empty counts as absent, so Remove deletes it too, unless [*] names it.
// A key names nothing in a list, so Remove reports the first list that a key of
// p meets.
func (p Path) Remove(object map[string]any) error {
	r := removal{path: p}
	if len(p) > 0 {
		r.remove(object, p)
	}
	return r.err
}

type removal struct {
	path Path
	err  error
}

// remove removes p, the end of r.path, from node and reports whether node is
// left empty. A list cannot empty in place, so the caller empties it.
func (r *removal) remove(node any, p Path) bool {
	step, rest := p[0], p[1:]
	switch node := node.(type) {
	case map[string]any:
		switch {
		case step.Each && len(rest) == 0:
			clear(node)
		case step.Each:
			for key, value := range node {
				node[key] = r.removeFromItem(value, rest)
			}
		case len(rest) == 0 || r.remove(node[step.Key], rest):
			delete(node, step.Key)
		}
		return len(node) == 0
	case []any:
		switch {
		case step.Each && len(rest) == 0:
			return true
		case step.Each:
			for i, item := range node {
				node[i] = r.removeFromItem(item, rest)
			}
		case r.err == nil:
			list := r.path[:len(r.path)-len(p)]
			r.err = fmt.Errorf("%s is a list; write %s"+inBlockStyle, list, slices.Concat(list, Path{{Each: true}}, p))
		}
		return len(node) == 0
	}
	return false
}

// removeFromItem removes rest from one item of a list or value of a map. The
// item stays even when left empty, so that the items still count.
func (r *removal) removeFromItem(item any, rest Path) any {
	if _, isList := item.([]any); r.remove(item, rest) && isList {
		return []any{}
	}
	return item
}
