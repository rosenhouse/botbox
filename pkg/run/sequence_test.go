package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/target"
)

const goldenSequence = "testdata/sequence.json"

// designExample is the sequence of DESIGN.md §7, as written there.
const designExample = `{
  "seed": 8675309,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.botbox/v1", "kind": "Widget", "spec": {"count": 3}}},
    {"i": 1, "t": "fault", "spec": {"match": {"verb": "create", "resource": "configmaps", "fraction": 0.5}, "action": {"error": 500}, "until": {"op": 3}}},
    {"i": 2, "t": "update", "patch": {"spec": {"count": 5}}, "noSettle": true},
    {"i": 3, "t": "settle"},
    {"i": 4, "t": "restart"},
    {"i": 5, "t": "settle"},
    {"i": 6, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
    {"i": 7, "t": "delete"}
  ]
}`

func TestGoldenSequenceIsByteIdenticalAfterARoundTrip(t *testing.T) {
	want, err := os.ReadFile(goldenSequence)
	if err != nil {
		t.Fatalf("Reading the golden sequence failed: %v", err)
	}

	decoded, err := UnmarshalSequence(want)
	if err != nil {
		t.Fatalf("Decoding the golden sequence failed: %v", err)
	}
	got, err := decoded.Marshal()
	if err != nil {
		t.Fatalf("Encoding the golden sequence failed: %v", err)
	}

	if string(got) != string(want) {
		t.Errorf("The golden sequence came back as\n%s\nwant\n%s", got, want)
	}
}

// The golden sequence holds every op type, so the round trip covers each one.
func TestGoldenSequenceHoldsEveryOpType(t *testing.T) {
	decoded := readGolden(t)

	held := map[OpType]bool{}
	for _, op := range decoded.Ops {
		held[op.Type] = true
	}
	for _, opType := range opTypes {
		if !held[opType] {
			t.Errorf("The golden sequence holds no %q op.", opType)
		}
	}
}

func TestSequenceReadsTheDesignExample(t *testing.T) {
	decoded, err := UnmarshalSequence([]byte(designExample))
	if err != nil {
		t.Fatalf("Decoding the example of DESIGN.md §7 failed: %v", err)
	}

	if decoded.Seed != 8675309 || decoded.Target != "toy-widget" {
		t.Errorf("The example decoded to seed %d and target %q.", decoded.Seed, decoded.Target)
	}
	create := decoded.Ops[0]
	if count, _, _ := unstructured.NestedInt64(create.Obj.Object, "spec", "count"); count != 3 {
		t.Errorf("The create op carries %v, want a Widget of count 3.", create.Obj)
	}
	fault := decoded.Ops[1].Fault
	if fault == nil || fault.Match.Fraction != 0.5 || fault.Action.Error != 500 || fault.Until.Op == nil || *fault.Until.Op != 3 {
		t.Errorf("The fault op decoded to %+v.", fault)
	}
	update := decoded.Ops[2]
	if !update.NoSettle || update.Patch == nil {
		t.Errorf("The update op decoded to %+v, want a patch it does not settle after.", update)
	}
	deleteManaged := decoded.Ops[6]
	if deleteManaged.Kind != "v1/ConfigMap" || deleteManaged.Nth == nil || *deleteManaged.Nth != 0 {
		t.Errorf("The deleteManaged op decoded to %+v.", deleteManaged)
	}
	if types := opTypesOf(decoded); strings.Join(types, ",") != "create,fault,update,settle,restart,settle,deleteManaged,delete" {
		t.Errorf("The example decoded to the ops %v.", types)
	}
}

func TestSequenceRejectsMalformedOps(t *testing.T) {
	for _, test := range []struct {
		name string
		ops  string
		want string
	}{
		{
			name: "an unknown op type",
			ops:  `{"i": 0, "t": "reboot"}`,
			want: `"reboot"`,
		},
		{
			name: "an index that is not the op's position",
			ops:  `{"i": 1, "t": "settle"}`,
			want: "position",
		},
		{
			name: "an unknown field",
			ops:  `{"i": 0, "t": "settle", "wait": "5s"}`,
			want: "wait",
		},
		{
			name: "a create without an object",
			ops:  `{"i": 0, "t": "create"}`,
			want: "obj",
		},
		{
			name: "a recreate without an object",
			ops:  `{"i": 0, "t": "recreate"}`,
			want: "obj",
		},
		{
			name: "an update without a patch",
			ops:  `{"i": 0, "t": "update"}`,
			want: "patch",
		},
		{
			name: "a delete carrying an object",
			ops:  `{"i": 0, "t": "delete", "obj": {"kind": "Widget"}}`,
			want: "obj",
		},
		{
			name: "a settle carrying a patch",
			ops:  `{"i": 0, "t": "settle", "patch": {"spec": {"count": 1}}}`,
			want: "patch",
		},
		{
			name: "a deleteManaged without a kind",
			ops:  `{"i": 0, "t": "deleteManaged", "index": 0}`,
			want: "kind",
		},
		{
			name: "a deleteManaged without an index",
			ops:  `{"i": 0, "t": "deleteManaged", "kind": "v1/ConfigMap"}`,
			want: "index",
		},
		{
			name: "a deleteManaged with a negative index",
			ops:  `{"i": 0, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": -1}`,
			want: "index",
		},
		{
			name: "a restart carrying a kind",
			ops:  `{"i": 0, "t": "restart", "kind": "v1/ConfigMap"}`,
			want: "kind",
		},
		{
			name: "a fault without a spec",
			ops:  `{"i": 0, "t": "fault"}`,
			want: "spec",
		},
		{
			name: "a fault with no action",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"verb": "create"}}}`,
			want: "action",
		},
		{
			name: "a fault with two actions",
			ops:  `{"i": 0, "t": "fault", "spec": {"action": {"error": 500, "drop": true}}}`,
			want: "action",
		},
		{
			name: "a fault on a verb the proxy never records",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"verb": "post"}, "action": {"drop": true}}}`,
			want: `match.verb "post" is not one of get, list, watch, create, update, patch, delete and deletecollection`,
		},
		{
			name: "a fault on a verb in capitals",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"verb": "Create"}, "action": {"drop": true}}}`,
			want: `"Create"`,
		},
		{
			name: "a fault on a subresource",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"resource": "widgets/status"}, "action": {"drop": true}}}`,
			want: `match.resource "widgets/status" holds a slash; name the plural alone, such as configmaps. A fault on a resource matches its subresources' requests too`,
		},
		{
			name: "a fault on a resource with its version",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"resource": "v1/configmaps"}, "action": {"drop": true}}}`,
			want: `match.resource "v1/configmaps" holds a slash; name the plural alone, such as configmaps. A fault on a resource matches its subresources' requests too`,
		},
		{
			name: "a fault on a resource with a leading slash",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"resource": "/configmaps"}, "action": {"drop": true}}}`,
			want: `match.resource "/configmaps" holds a slash; name the plural alone, such as configmaps. A fault on a resource matches its subresources' requests too`,
		},
		{
			name: "a fault on a resource with a trailing slash",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"resource": "configmaps/"}, "action": {"drop": true}}}`,
			want: `match.resource "configmaps/" holds a slash; name the plural alone, such as configmaps. A fault on a resource matches its subresources' requests too`,
		},
		{
			name: "a restart that skips its settle",
			ops:  `{"i": 0, "t": "restart", "noSettle": true}`,
			want: "noSettle",
		},
		{
			name: "a settle that skips its settle",
			ops:  `{"i": 0, "t": "settle", "noSettle": true}`,
			want: "noSettle",
		},
		{
			name: "a deleteManaged that skips its settle",
			ops:  `{"i": 0, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0, "noSettle": true}`,
			want: "noSettle",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := UnmarshalSequence([]byte(`{"seed": 1, "target": "toy-widget", "ops": [` + test.ops + `]}`))

			if err == nil {
				t.Fatalf("The op %s was accepted, want an error naming %q.", test.ops, test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("The op %s was rejected with %q, want the error to name %q.", test.ops, err, test.want)
			}
		})
	}
}

func TestSequenceAcceptsTheOpsTheRunnerExecutes(t *testing.T) {
	for _, ops := range []string{
		`{"i": 0, "t": "create", "obj": {"kind": "Widget"}, "noSettle": true}`,
		`{"i": 0, "t": "update", "patch": {"spec": {"count": null}}}`,
		`{"i": 0, "t": "delete", "noSettle": true}`,
		`{"i": 0, "t": "recreate", "obj": {"kind": "Widget"}}`,
		`{"i": 0, "t": "fault", "spec": {"action": {"delay": "250ms"}, "until": {"for": "5s"}}}`,
		`{"i": 0, "t": "fault", "spec": {"action": {"drop": true}, "until": {"count": 3}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "get"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "list"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "watch"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "create"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "update"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "patch"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "delete"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "deletecollection"}, "action": {"drop": true}}}`,
	} {
		t.Run(ops, func(t *testing.T) {
			// The trailing settle is what the sequence needs, not the op under test.
			whole := `{"seed": 1, "target": "t", "ops": [` + ops + `, {"i": 1, "t": "settle"}]}`
			if _, err := UnmarshalSequence([]byte(whole)); err != nil {
				t.Errorf("The op was rejected: %v", err)
			}
		})
	}
}

// DESIGN.md §6: the teardown waits for convergence before it opens its quiet
// window only after a fault, so a sequence that ends while the target is
// still working is judged on that work. Every sequence ends with an op that
// settles.
func TestSequenceRequiresALastOpThatSettles(t *testing.T) {
	create := `{"i": 0, "t": "create", "obj": {"kind": "Widget"}}`
	for _, test := range []struct {
		name string
		ops  string
		want string
	}{
		{name: "no ops at all", ops: ``, want: "no ops"},
		{name: "a last op that skips its settle", want: "settle",
			ops: `{"i": 0, "t": "create", "obj": {"kind": "Widget"}, "noSettle": true}`},
		{name: "a trailing restart", ops: create + `, {"i": 1, "t": "restart"}`, want: "settle"},
		{name: "a trailing fault", want: "settle",
			ops: create + `, {"i": 1, "t": "fault", "spec": {"action": {"drop": true}, "until": {"count": 3}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := UnmarshalSequence([]byte(`{"seed": 1, "target": "t", "ops": [` + test.ops + `]}`))

			if err == nil {
				t.Fatalf("The ops %s were accepted, want an error naming %q.", test.ops, test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("The ops %s were rejected with %q, want the error to name %q.", test.ops, err, test.want)
			}
		})
	}
}

// OnCR says which ops carry noSettle (DESIGN.md §4), which is what lets
// generation draw one.
func TestOnlyTheOpsOnThePrimaryCRActOnIt(t *testing.T) {
	for _, test := range []struct {
		opType OpType
		want   bool
	}{
		{opType: OpCreate, want: true},
		{opType: OpUpdate, want: true},
		{opType: OpDelete, want: true},
		{opType: OpRecreate, want: true},
		{opType: OpSettle},
		{opType: OpRestart},
		{opType: OpFault},
		{opType: OpDeleteManaged},
	} {
		t.Run(string(test.opType), func(t *testing.T) {
			if got := test.opType.OnCR(); got != test.want {
				t.Errorf("A %s op acts on the CR: %t, want %t.", test.opType, got, test.want)
			}
		})
	}
}

func TestOpSettlesUnlessItSaysOtherwise(t *testing.T) {
	for _, test := range []struct {
		opType OpType
		want   bool
	}{
		{opType: OpCreate, want: true},
		{opType: OpUpdate, want: true},
		{opType: OpDelete, want: true},
		{opType: OpRecreate, want: true},
		{opType: OpDeleteManaged, want: true},
		{opType: OpSettle, want: true},
		{opType: OpRestart},
		{opType: OpFault},
	} {
		t.Run(string(test.opType), func(t *testing.T) {
			if got := (Op{Type: test.opType}).Settles(); got != test.want {
				t.Errorf("A %s op settles: %t, want %t.", test.opType, got, test.want)
			}
			if got := (Op{Type: test.opType, NoSettle: true}).Settles(); got && test.opType != OpSettle {
				t.Errorf("A %s op with noSettle still settles.", test.opType)
			}
		})
	}
}

func TestManagedKindResolvesAgainstWhatTheTargetManages(t *testing.T) {
	toy := &target.Target{Manages: []schema.GroupVersionKind{
		configMapKind,
		{Group: "apps", Version: "v1", Kind: "Deployment"},
	}}

	for _, test := range []struct {
		declared string
		want     schema.GroupVersionKind
	}{
		{declared: "v1/ConfigMap", want: configMapKind},
		{declared: "apps/v1/Deployment", want: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}},
	} {
		t.Run(test.declared, func(t *testing.T) {
			got, err := managedKind(toy, test.declared)
			if err != nil || got != test.want {
				t.Errorf("managedKind returned (%v, %v), want %v.", got, err, test.want)
			}
		})
	}

	if _, err := managedKind(toy, "v1/Secret"); err == nil || !strings.Contains(err.Error(), "v1/Secret") {
		t.Errorf("managedKind returned %v for a kind the target does not manage, want an error naming it.", err)
	}
}

func TestReadSequenceReportsAMissingFile(t *testing.T) {
	_, err := ReadSequence(filepath.Join(t.TempDir(), "absent.json"))

	if err == nil || !strings.Contains(err.Error(), "absent.json") {
		t.Errorf("ReadSequence returned %v, want an error naming the file.", err)
	}
}

func TestWriteSequenceWritesTheCanonicalForm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sequence.json")
	golden := readGolden(t)

	if err := WriteSequence(path, golden); err != nil {
		t.Fatalf("WriteSequence failed: %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Reading back the sequence failed: %v", err)
	}
	want, err := os.ReadFile(goldenSequence)
	if err != nil {
		t.Fatalf("Reading the golden sequence failed: %v", err)
	}
	if string(written) != string(want) {
		t.Errorf("WriteSequence wrote\n%s\nwant\n%s", written, want)
	}
}

func readGolden(t *testing.T) Sequence {
	t.Helper()
	sequence, err := ReadSequence(goldenSequence)
	if err != nil {
		t.Fatalf("Reading the golden sequence failed: %v", err)
	}
	return sequence
}

func opTypesOf(s Sequence) []string {
	var types []string
	for _, op := range s.Ops {
		types = append(types, string(op.Type))
	}
	return types
}
