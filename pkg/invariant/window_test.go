package invariant_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/invariant"
	"github.com/rosenhouse/botbox/pkg/observe"
)

// A violation quotes the evidence nearest it, because that is what explains it
// (DESIGN.md §5.7, D35).
func TestRecentQuotesTheEntriesNearestTheViolation(t *testing.T) {
	entries := make([]int, 25)
	for i := range entries {
		entries[i] = i
	}

	got := invariant.Recent(entries).Quoted

	if len(got) != 20 {
		t.Fatalf("Recent kept %d of %d entries, want the 20 a report carries.", len(got), len(entries))
	}
	if got[0] != 5 || got[19] != 24 {
		t.Errorf("Recent kept the entries %v, want the last 20.", got)
	}
}

// A check bounds what it quotes, so it has to say how much it chose from or a
// report cannot tell a complete excerpt from a bounded one (#22).
func TestRecentSaysHowMuchItChoseFrom(t *testing.T) {
	for _, entries := range []int{3, invariant.MaxEvidence, 25} {
		got := invariant.Recent(make([]int, entries))

		if got.Total != entries {
			t.Errorf("Recent over %d entries says it chose from %d.", entries, got.Total)
		}
		if want := min(entries, invariant.MaxEvidence); len(got.Quoted) != want {
			t.Errorf("Recent over %d entries quotes %d, want %d.", entries, len(got.Quoted), want)
		}
	}
}

func TestReadinessSaysHowMuchItChoseFrom(t *testing.T) {
	history := make([]observe.Version, 25)
	for i := range history {
		history[i] = observe.Version{Key: observe.Key{GVK: widgetGVK, Name: "w"}, Time: at(time.Duration(i) * time.Second)}
	}
	managed := make([]observe.Version, 5)
	for i := range managed {
		managed[i] = observe.Version{
			Key:  observe.Key{GVK: configMapGVK, Name: "w-" + strconv.Itoa(i)},
			Time: at(30 * time.Second),
		}
	}

	got := invariant.Readiness(history, managed)

	if want := len(history) + len(managed); got.Total != want {
		t.Errorf("Readiness says it chose from %d versions, want the %d it was given.", got.Total, want)
	}
	if len(got.Quoted) != invariant.MaxEvidence {
		t.Errorf("Readiness quotes %d versions, want the bound of %d.", len(got.Quoted), invariant.MaxEvidence)
	}
}
