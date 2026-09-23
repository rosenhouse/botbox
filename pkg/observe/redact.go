package observe

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

var secretGVK = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}

// markerKey is drawn once per process and never written, so a marker cannot
// be looked up or brute-forced from what botbox writes.
var markerKey = []byte(rand.Text())

// Redacted returns a copy of a Secret whose data and stringData values and
// last-applied-configuration are markers. Equal values share a marker within
// one process. Any other kind is returned as it is, and content is never
// modified.
func Redacted(gvk schema.GroupVersionKind, content map[string]any) map[string]any {
	if gvk != secretGVK {
		return content
	}
	redacted := runtime.DeepCopyJSON(content)
	data, _ := redacted["data"].(map[string]any)
	for key, value := range data {
		decoded, _ := base64.StdEncoding.DecodeString(text(value))
		data[key] = marker(decoded)
	}
	stringData, _ := redacted["stringData"].(map[string]any)
	for key, value := range stringData {
		stringData[key] = marker([]byte(text(value)))
	}
	metadata, _ := redacted["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	if value, found := annotations[lastApplied]; found {
		annotations[lastApplied] = marker([]byte(text(value)))
	}
	return redacted
}

func marker(secret []byte) string {
	mac := hmac.New(sha256.New, markerKey)
	mac.Write(secret)
	return fmt.Sprintf("[redacted %d bytes hmac-sha256:%x]", len(secret), mac.Sum(nil)[:8])
}

func text(value any) string {
	s, _ := value.(string)
	return s
}
