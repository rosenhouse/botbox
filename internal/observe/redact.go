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

var secretGVK = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}

// markerKey is drawn once per process and never written, so a marker cannot
// be looked up or brute-forced from what botbox writes.
var markerKey = []byte(rand.Text())

// Redacted returns a copy of a Secret whose data and annotation values are
// markers, because a controller may annotate a Secret with a copy or a hash of
// its data. Equal values share a marker within one process. Any other kind is
// returned as it is, and content is never modified.
func Redacted(gvk schema.GroupVersionKind, content map[string]any) map[string]any {
	if gvk != secretGVK {
		return content
	}
	secret := runtime.DeepCopyJSON(content)
	data, _ := secret["data"].(map[string]any)
	for key, value := range data {
		// The API server serves only base64 strings, so other values may share a marker.
		encoded, _ := value.(string)
		decoded, _ := base64.StdEncoding.DecodeString(encoded)
		data[key] = marker(decoded)
	}
	metadata, _ := secret["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	for key, value := range annotations {
		text, _ := value.(string)
		annotations[key] = marker([]byte(text))
	}
	return secret
}

func marker(secret []byte) string {
	mac := hmac.New(sha256.New, markerKey)
	mac.Write(secret)
	return fmt.Sprintf("[redacted %d bytes hmac-sha256:%x]", len(secret), mac.Sum(nil)[:8])
}
