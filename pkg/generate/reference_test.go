package generate

import (
	"slices"
	"strconv"
	"testing"

	"github.com/rosenhouse/botbox/internal/reference"
)

func TestTheReferenceNamesEveryOverlayKeyword(t *testing.T) {
	documented := reference.Spans(reference.Row(t, "## target.yaml", "generate.overlay")[1])

	if !slices.Equal(slices.Sorted(slices.Values(documented)), keywords) {
		t.Errorf("The row for generate.overlay names %v, want the keywords botbox reads, %v.", documented, keywords)
	}
}

func TestTheReferenceGivesTheDefaultMaxCRs(t *testing.T) {
	documented := reference.Row(t, "## target.yaml", "generate.maxCRs")[0]

	if want := "`" + strconv.Itoa(defaultMaxCRs) + "`"; documented != want {
		t.Errorf("The reference gives generate.maxCRs the default %s, want %s.", documented, want)
	}
}
