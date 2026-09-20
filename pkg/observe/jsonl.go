package observe

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// WriteHistory writes the run's objects.jsonl (DESIGN.md §5.7): one version per
// line, in the order recorded.
func (s *Store) WriteHistory(w io.Writer) error {
	encoder := json.NewEncoder(w)
	for _, v := range s.all() {
		if err := encoder.Encode(v); err != nil {
			return fmt.Errorf("writing the object history: %w", err)
		}
	}
	return nil
}

// versionLine is one objects.jsonl line. Its field names are the report's
// interface and change only with §5.7.
type versionLine struct {
	Kind               string                     `json:"kind"` // group/version/Kind
	Namespace          string                     `json:"namespace"`
	Name               string                     `json:"name"`
	UID                types.UID                  `json:"uid,omitempty"`
	ResourceVersion    string                     `json:"resourceVersion,omitempty"`
	Time               time.Time                  `json:"time"`
	Generation         int64                      `json:"generation,omitempty"`
	ObservedGeneration *int64                     `json:"observedGeneration,omitempty"`
	Finalizers         []string                   `json:"finalizers,omitempty"`
	OwnerReferences    []metav1.OwnerReference    `json:"ownerReferences,omitempty"`
	DeletionTimestamp  *metav1.Time               `json:"deletionTimestamp,omitempty"`
	Labels             map[string]string          `json:"labels,omitempty"`
	Deleted            bool                       `json:"deleted,omitempty"`
	Object             *unstructured.Unstructured `json:"object,omitempty"`
}

// MarshalJSON renders v as one objects.jsonl line.
func (v Version) MarshalJSON() ([]byte, error) {
	return json.Marshal(versionLine{
		Kind:               kindName(v.GVK),
		Namespace:          v.Namespace,
		Name:               v.Name,
		UID:                v.UID,
		ResourceVersion:    v.ResourceVersion,
		Time:               v.Time,
		Generation:         v.Generation,
		ObservedGeneration: v.ObservedGeneration,
		Finalizers:         v.Finalizers,
		OwnerReferences:    v.OwnerReferences,
		DeletionTimestamp:  v.DeletionTimestamp,
		Labels:             v.Labels,
		Deleted:            v.Deleted,
		Object:             v.Object,
	})
}
