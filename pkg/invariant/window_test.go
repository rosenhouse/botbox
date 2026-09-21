package invariant_test

import (
	"testing"

	"github.com/rosenhouse/botbox/pkg/invariant"
)

// A violation quotes the evidence nearest it, because that is what explains it
// (DESIGN.md §5.7, D35).
func TestRecentQuotesTheEntriesNearestTheViolation(t *testing.T) {
	entries := make([]int, 25)
	for i := range entries {
		entries[i] = i
	}

	got := invariant.Recent(entries)

	if len(got) != 20 {
		t.Fatalf("Recent kept %d of %d entries, want the 20 a report carries.", len(got), len(entries))
	}
	if got[0] != 5 || got[19] != 24 {
		t.Errorf("Recent kept the entries %v, want the last 20.", got)
	}
}
