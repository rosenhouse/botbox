package generate

import (
	"slices"
	"strconv"
	"testing"

	"github.com/rosenhouse/botbox/internal/reference"
)

func TestTheReferenceNamesEveryOverlayKeyword(t *testing.T) {
	documented := reference.Spans(reference.Row(t, "## target.yaml", "generate.overlay")[1])

	for _, keyword := range keywords {
		if !slices.Contains(documented, keyword) {
			t.Errorf("The row for generate.overlay does not name %s.", keyword)
		}
	}
}

func TestTheReferenceGivesTheDefaultMaxCRs(t *testing.T) {
	documented := reference.Row(t, "## target.yaml", "generate.maxCRs")[0]

	if want := "`" + strconv.Itoa(defaultMaxCRs) + "`"; documented != want {
		t.Errorf("The reference gives generate.maxCRs the default %s, want %s.", documented, want)
	}
}
