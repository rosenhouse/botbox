//go:build envtest

package main

import (
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/target"
)

func TestTheSessionRefusesAKindTheClusterServesAtClusterScope(t *testing.T) {
	toy, err := target.Load(toyTargetYAML)
	if err != nil {
		t.Fatal(err)
	}
	toy.Launch.Binary = "sh"
	s, err := openSession(options{}, toy)
	if err != nil {
		t.Fatalf("Opening the session failed: %v", err)
	}
	defer func() { _ = s.close() }()
	clusterRoles := *toy
	clusterRoles.Manages = append(slices.Clone(toy.Manages),
		schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"})

	if err := s.vet(toy); err != nil {
		t.Errorf("The session refused the toy: %v", err)
	}
	if err := s.vet(&clusterRoles); err == nil || !strings.Contains(err.Error(), "ClusterRole") {
		t.Errorf("The session answered %v for a target that manages ClusterRoles.", err)
	}
}
