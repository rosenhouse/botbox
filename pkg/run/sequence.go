package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/target"
)

// Sequence is one test sequence in the form of DESIGN.md §7. It is the unit of
// generation, replay and shrinking.
type Sequence struct {
	Seed   int64  `json:"seed"`
	Target string `json:"target"`
	Ops    []Op   `json:"ops"`
}

// Op is one step of a sequence. Marshalling follows the field order below, so
// a sequence read from disk and written back is byte-identical.
type Op struct {
	// Index is the op's position in the sequence.
	Index int    `json:"i"`
	Type  OpType `json:"t"`
	// Obj is what create and recreate create.
	Obj *unstructured.Unstructured `json:"obj,omitempty"`
	// Patch is the JSON merge patch (RFC 7386) update applies.
	Patch map[string]any `json:"patch,omitempty"`
	// Fault is the fault a fault op injects.
	Fault *Fault `json:"spec,omitempty"`
	// Kind and Nth select deleteManaged's object: the Nth managed object of
	// Kind, ordered by creationTimestamp then name.
	Kind string `json:"kind,omitempty"`
	Nth  *int   `json:"index,omitempty"`
	// NoSettle skips the Runner's implicit settle wait (DESIGN.md §5.5).
	NoSettle bool `json:"noSettle,omitempty"`
}

// OpType is an op's "t" (DESIGN.md §5.4).
type OpType string

const (
	OpCreate        OpType = "create"
	OpUpdate        OpType = "update"
	OpDelete        OpType = "delete"
	OpRecreate      OpType = "recreate"
	OpSettle        OpType = "settle"
	OpRestart       OpType = "restart"
	OpFault         OpType = "fault"
	OpDeleteManaged OpType = "deleteManaged"
)

// opTypes are every op type the format defines.
var opTypes = []OpType{OpCreate, OpUpdate, OpDelete, OpRecreate, OpSettle, OpRestart, OpFault, OpDeleteManaged}

// crOps act on the primary CR and may carry noSettle (DESIGN.md §4).
var crOps = []OpType{OpCreate, OpUpdate, OpDelete, OpRecreate}

// OnCR reports whether the op type acts on the primary CR.
func (t OpType) OnCR() bool { return slices.Contains(crOps, t) }

// Fault is what a fault op injects (DESIGN.md §5.2).
type Fault struct {
	Match  Match   `json:"match,omitzero"`
	Action Action  `json:"action"`
	Until  Trigger `json:"until,omitzero"`
}

// Match selects the requests a fault applies to. An unset field matches every
// request.
type Match struct {
	Verb     string  `json:"verb,omitempty"`
	Resource string  `json:"resource,omitempty"`
	Name     string  `json:"name,omitempty"`
	Fraction float64 `json:"fraction,omitempty"`
}

// Action is what the proxy does to a matched request. Exactly one field is set.
type Action struct {
	Error int      `json:"error,omitempty"`
	Delay Duration `json:"delay,omitempty"`
	Drop  bool     `json:"drop,omitempty"`
}

// Trigger ends a fault at an op index, after a count of matched requests, or
// after a duration (DESIGN.md §5.2).
type Trigger struct {
	Op    *int     `json:"op,omitempty"`
	Count int      `json:"count,omitempty"`
	For   Duration `json:"for,omitempty"`
}

// Duration marshals as a Go duration string, as the rest of the harness's
// configuration does.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// Settles reports whether the Runner waits for convergence after the op
// (DESIGN.md §5.5). A settle op is the wait itself.
func (o Op) Settles() bool {
	return o.Type == OpSettle || (!o.NoSettle && slices.Contains(mutatingOps, o.Type))
}

// mutatingOps change the CR or a managed object, so the Runner settles after
// them.
var mutatingOps = append(slices.Clone(crOps), OpDeleteManaged)

// ReadSequence reads and validates a sequence file.
func ReadSequence(path string) (Sequence, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Sequence{}, fmt.Errorf("reading the sequence: %w", err)
	}
	sequence, err := UnmarshalSequence(data)
	if err != nil {
		return Sequence{}, fmt.Errorf("reading the sequence %s: %w", path, err)
	}
	return sequence, nil
}

// WriteSequence writes the sequence in its canonical form.
func WriteSequence(path string, s Sequence) error {
	data, err := s.Marshal()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing the sequence: %w", err)
	}
	return nil
}

// UnmarshalSequence decodes a sequence and validates every op. An unknown op
// type, an unknown field and an op missing what its type needs are all
// configuration errors (DESIGN.md §11).
func UnmarshalSequence(data []byte) (Sequence, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var sequence Sequence
	if err := decoder.Decode(&sequence); err != nil {
		return Sequence{}, fmt.Errorf("the sequence does not parse: %w", err)
	}
	if err := sequence.Validate(); err != nil {
		return Sequence{}, err
	}
	return sequence, nil
}

// Marshal returns the sequence's canonical form: the JSON of DESIGN.md §7,
// indented two spaces and newline-terminated.
func (s Sequence) Marshal() ([]byte, error) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding the sequence: %w", err)
	}
	return append(data, '\n'), nil
}

// Validate reports the first malformed op, or a sequence that ends while the
// target is still working.
func (s Sequence) Validate() error {
	for i, op := range s.Ops {
		if err := op.validate(i); err != nil {
			return fmt.Errorf("op %d: %w", i, err)
		}
	}
	// The teardown's quiet window is the only one such a sequence would be
	// judged on, and the teardown does not wait for convergence before it opens
	// that window (DESIGN.md §5.6).
	if len(s.Ops) == 0 || !s.Ops[len(s.Ops)-1].Settles() {
		return fmt.Errorf("the sequence does not end with an op that settles")
	}
	return nil
}

func (o Op) validate(position int) error {
	if !slices.Contains(opTypes, o.Type) {
		return fmt.Errorf("%q is not an op type; want one of %v", o.Type, opTypes)
	}
	if o.Index != position {
		return fmt.Errorf("i is %d, want the op's position %d", o.Index, position)
	}
	if o.NoSettle && !slices.Contains(crOps, o.Type) {
		return fmt.Errorf("only a CR op carries noSettle; %q is not one", o.Type)
	}
	if err := o.validateFields(); err != nil {
		return err
	}
	if o.Type == OpFault {
		return o.Fault.validate()
	}
	return nil
}

// opFields are the fields an op may carry, in the order errors report them.
var opFields = []string{"obj", "patch", "spec", "kind", "index"}

// validateFields reports a field the op's type does not take, or one it needs
// and does not carry.
func (o Op) validateFields() error {
	carried := map[string]bool{
		"obj":   o.Obj != nil,
		"patch": o.Patch != nil,
		"spec":  o.Fault != nil,
		"kind":  o.Kind != "",
		"index": o.Nth != nil,
	}
	wanted := fieldsOf(o.Type)
	for _, field := range opFields {
		if carried[field] == wanted[field] {
			continue
		}
		if wanted[field] {
			return fmt.Errorf("a %s op needs %s", o.Type, field)
		}
		return fmt.Errorf("a %s op takes no %s", o.Type, field)
	}
	if o.Type == OpDeleteManaged && *o.Nth < 0 {
		return fmt.Errorf("index is %d, want the position of a managed object", *o.Nth)
	}
	return nil
}

// fieldsOf says which fields an op type carries.
func fieldsOf(opType OpType) map[string]bool {
	fields := map[string]bool{}
	switch opType {
	case OpCreate, OpRecreate:
		fields["obj"] = true
	case OpUpdate:
		fields["patch"] = true
	case OpFault:
		fields["spec"] = true
	case OpDeleteManaged:
		fields["kind"], fields["index"] = true, true
	}
	return fields
}

func (f *Fault) validate() error {
	actions := 0
	for _, set := range []bool{f.Action.Error != 0, f.Action.Delay != 0, f.Action.Drop} {
		if set {
			actions++
		}
	}
	if actions != 1 {
		return fmt.Errorf("the fault carries %d actions, want exactly one of error, delay or drop", actions)
	}
	return nil
}

// managedKind resolves a deleteManaged op's kind against what the target
// declares it manages (DESIGN.md §8.1).
func managedKind(t *target.Target, declared string) (schema.GroupVersionKind, error) {
	for _, gvk := range t.Manages {
		if kindName(gvk) == declared {
			return gvk, nil
		}
	}
	return schema.GroupVersionKind{}, fmt.Errorf("the target manages no kind %q", declared)
}

// kindName writes a kind as DESIGN.md §8.1 declares it.
func kindName(gvk schema.GroupVersionKind) string {
	if gvk.Group == "" {
		return gvk.Version + "/" + gvk.Kind
	}
	return gvk.Group + "/" + gvk.Version + "/" + gvk.Kind
}
