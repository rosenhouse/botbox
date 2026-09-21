package controller

import (
	"strconv"
	"strings"
	"testing"
)

func TestParseBugAcceptsEveryCatalogEntry(t *testing.T) {
	for id := 0; id <= MaxBug; id++ {
		bug, err := ParseBug(id)
		if err != nil {
			t.Errorf("ParseBug(%d) returned an error: %v", id, err)
		}
		if int(bug) != id {
			t.Errorf("ParseBug(%d) returned bug %d.", id, bug)
		}
	}
}

func TestParseBugRejectsIDsOutsideTheCatalog(t *testing.T) {
	for _, id := range []int{-1, MaxBug + 1} {
		_, err := ParseBug(id)
		if err == nil {
			t.Fatalf("ParseBug(%d) accepted an ID outside the catalog.", id)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(MaxBug)) {
			t.Errorf("ParseBug(%d) returned %q, which does not name the valid range.", id, err)
		}
	}
}
