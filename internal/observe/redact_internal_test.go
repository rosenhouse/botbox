package observe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestAMarkersDigestIsTheValuesHMACUnderTheProcessKey(t *testing.T) {
	value := []byte("correct horse battery staple")
	mac := hmac.New(sha256.New, markerKey)
	mac.Write(value)
	want := "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil))[:16] + "]"

	if got := marker(value); !strings.HasSuffix(got, want) {
		t.Errorf("The marker is %s, want it to end %s.", got, want)
	}
}
