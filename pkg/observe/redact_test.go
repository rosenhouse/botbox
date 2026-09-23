package observe_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/observe"
)

const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

var (
	secretGVK = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	marker    = regexp.MustCompile(`^\[redacted (\d+) bytes hmac-sha256:([0-9a-f]{16})\]$`)
)

// secret returns a Secret whose data holds each value base64-encoded, as the
// API server serves it.
func secret(name, resourceVersion string, values map[string]string) *unstructured.Unstructured {
	u := object(secretGVK, name, resourceVersion)
	data := map[string]any{}
	for key, value := range values {
		data[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	u.Object["data"] = data
	return u
}

// writtenObjects records each object as a version of one Secret and returns
// the objects objects.jsonl holds.
func writtenObjects(t *testing.T, objects ...*unstructured.Unstructured) []map[string]any {
	t.Helper()
	s := observe.NewStore(managing(secretGVK))
	for i, obj := range objects {
		s.Record(secretGVK, obj, at(i))
	}
	var written []map[string]any
	for _, raw := range writeHistory(t, s) {
		written = append(written, line(t, []byte(raw))["object"].(map[string]any))
	}
	return written
}

func field(t *testing.T, obj map[string]any, fields ...string) map[string]any {
	t.Helper()
	value, found, err := unstructured.NestedMap(obj, fields...)
	if !found || err != nil {
		t.Fatalf("The object carries no map at %v: %v", fields, err)
	}
	return value
}

// digest is the marker's digest, and fails the test for a value that is no
// marker.
func digest(t *testing.T, value any) string {
	t.Helper()
	parts := marker.FindStringSubmatch(fmt.Sprint(value))
	if parts == nil {
		t.Fatalf("%v is not a marker.", value)
	}
	return parts[2]
}

func TestAHistoryLineHidesASecretsValuesAndNamesItsKeys(t *testing.T) {
	obj := secret("creds", "10", map[string]string{"token": "s3cr3t"})
	obj.Object["stringData"] = map[string]any{"password": "hunter2"}
	written := writtenObjects(t, obj)[0]

	for _, value := range []string{"s3cr3t", "czNjcjN0", "hunter2"} {
		if strings.Contains(fmt.Sprint(written), value) {
			t.Errorf("objects.jsonl holds the Secret's value %q: %v", value, written)
		}
	}
	if got := field(t, written, "data")["token"]; !marker.MatchString(fmt.Sprint(got)) {
		t.Errorf("data.token is %v, want a marker.", got)
	}
	if got := field(t, written, "stringData")["password"]; !marker.MatchString(fmt.Sprint(got)) {
		t.Errorf("stringData.password is %v, want a marker.", got)
	}
}

func TestAMarkerCountsTheBytesTheSecretHolds(t *testing.T) {
	obj := secret("creds", "10", map[string]string{"token": "s3cr3t"})
	obj.Object["stringData"] = map[string]any{"token": "s3cr3t"}
	written := writtenObjects(t, obj)[0]

	encoded, plain := field(t, written, "data")["token"], field(t, written, "stringData")["token"]
	if want := "[redacted 6 bytes hmac-sha256:" + digest(t, plain) + "]"; encoded != want || plain != want {
		t.Errorf("data.token is %v and stringData.token %v, want both %s: the value, not its encoding.", encoded, plain, want)
	}
}

func TestAValueThatIsNoStringIsMarkedToo(t *testing.T) {
	obj := object(secretGVK, "creds", "10")
	obj.Object["data"] = map[string]any{"null": nil, "number": int64(42)}
	data := field(t, writtenObjects(t, obj)[0], "data")

	digest(t, data["null"])
	digest(t, data["number"])
}

func TestAMarkerShowsWhetherAValueChanged(t *testing.T) {
	written := writtenObjects(t,
		secret("creds", "10", map[string]string{"token": "first", "ca": "same"}),
		secret("creds", "11", map[string]string{"token": "second", "ca": "same"}),
	)
	before, after := field(t, written[0], "data"), field(t, written[1], "data")

	if digest(t, before["ca"]) != digest(t, after["ca"]) {
		t.Errorf("An unchanged value is %v, then %v.", before["ca"], after["ca"])
	}
	if digest(t, before["token"]) == digest(t, after["token"]) {
		t.Errorf("A changed value is %v both times.", before["token"])
	}
}

func TestEqualValuesShareAMarkerAcrossRunsOfOneProcess(t *testing.T) {
	first := writtenObjects(t, secret("creds", "10", map[string]string{"token": "s3cr3t"}))[0]
	second := writtenObjects(t, secret("creds", "10", map[string]string{"token": "s3cr3t"}))[0]

	if a, b := field(t, first, "data")["token"], field(t, second, "data")["token"]; a != b {
		t.Errorf("Two runs wrote one value as %v and %v.", a, b)
	}
}

func TestAMarkerIsNoUnkeyedHash(t *testing.T) {
	written := writtenObjects(t, secret("creds", "10", map[string]string{"token": "s3cr3t"}))[0]
	got := digest(t, field(t, written, "data")["token"])

	for _, value := range []string{"s3cr3t", "czNjcjN0"} {
		sum := sha256.Sum256([]byte(value))
		if strings.HasPrefix(hex.EncodeToString(sum[:]), got) {
			t.Errorf("The marker's digest %s is the sha256 of %q, which anyone can recompute.", got, value)
		}
	}
}

func TestEachProcessKeysItsMarkersAfresh(t *testing.T) {
	markerOfAProcess := func() string {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPrintAMarker$")
		cmd.Env = append(os.Environ(), "BOTBOX_PRINT_A_MARKER=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("The helper process failed: %v\n%s", err, out)
		}
		printed, _, _ := strings.Cut(string(out), "\n")
		digest(t, printed)
		return printed
	}

	if first, second := markerOfAProcess(), markerOfAProcess(); first == second {
		t.Errorf("Two processes marked one value %s, so its marker can be looked up.", first)
	}
}

func TestPrintAMarker(t *testing.T) {
	if os.Getenv("BOTBOX_PRINT_A_MARKER") == "" {
		t.Skip("This is TestEachProcessKeysItsMarkersAfresh's helper process.")
	}
	fmt.Println(field(t, observe.Redacted(secretGVK, secret("creds", "10", map[string]string{"token": "s3cr3t"}).Object), "data")["token"])
}

func TestAHistoryLineHidesTheLastAppliedConfiguration(t *testing.T) {
	obj := secret("creds", "10", map[string]string{"token": "s3cr3t"})
	obj.SetAnnotations(map[string]string{
		lastApplied:         `{"apiVersion":"v1","kind":"Secret","data":{"token":"czNjcjN0"}}`,
		"example.com/owner": "team-a",
	})
	annotations := field(t, writtenObjects(t, obj)[0], "metadata", "annotations")

	digest(t, annotations[lastApplied])
	if got := annotations["example.com/owner"]; got != "team-a" {
		t.Errorf("The annotation example.com/owner is %v, want it as the Secret carries it.", got)
	}
}

func TestAConfigMapIsWrittenAsItIs(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Record(configMapGVK, withData(object(configMapGVK, "child", "10"), "czNjcjN0"), at(0))

	written := line(t, []byte(writeHistory(t, s)[0]))["object"].(map[string]any)
	if got := field(t, written, "data")["value"]; got != "czNjcjN0" {
		t.Errorf("A ConfigMap's data.value is written as %v, want it as recorded.", got)
	}
}

func TestWritingTheHistoryLeavesTheRecordedSecretWhole(t *testing.T) {
	s := observe.NewStore(managing(secretGVK))
	s.Record(secretGVK, secret("creds", "10", map[string]string{"token": "s3cr3t"}), at(0))
	writeHistory(t, s)

	recorded := s.History(key(secretGVK, "creds"))[0].Object.Object
	if got := field(t, recorded, "data")["token"]; got != "czNjcjN0" {
		t.Errorf("The store holds data.token %v after writing, want the value G5 compares.", got)
	}
}

func TestRedactedIsWhatObjectsJSONLHolds(t *testing.T) {
	obj := secret("creds", "10", map[string]string{"token": "s3cr3t"})

	written := field(t, writtenObjects(t, obj)[0], "data")["token"]
	if got := field(t, observe.Redacted(secretGVK, obj.Object), "data")["token"]; got != written {
		t.Errorf("Redacted gives data.token %v, and objects.jsonl %v.", got, written)
	}
}
