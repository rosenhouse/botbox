package proxy

import (
	"net/http"
	"slices"
	"testing"
)

func TestVerbsHoldsEveryVerbOfAResourceRequest(t *testing.T) {
	methods := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	requests := []Request{{Resource: "configmaps"}, {Resource: "configmaps", Name: "cm"}, {Resource: "configmaps", Watch: true}}
	for _, method := range methods {
		for _, r := range requests {
			if recorded := verb(method, r); !slices.Contains(Verbs, recorded) {
				t.Errorf("A %s of %+v records the verb %q, which Verbs leaves out.", method, r, recorded)
			}
		}
	}
}
