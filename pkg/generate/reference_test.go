package generate

import (
	"os"
	"strings"
	"testing"
)

func TestTheReferenceNamesEveryOverlayKeyword(t *testing.T) {
	const reference = "../../docs/reference.md"
	doc, err := os.ReadFile(reference)
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for _, line := range strings.Split(string(doc), "\n") {
		if strings.HasPrefix(line, "| `generate.overlay` |") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("%s has no row for generate.overlay.", reference)
	}

	for _, keyword := range keywords {
		if !strings.Contains(row, "`"+keyword+"`") {
			t.Errorf("The row for generate.overlay does not name %s.", keyword)
		}
	}
}
