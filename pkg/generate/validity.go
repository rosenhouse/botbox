package generate

import (
	"context"
	"encoding/json"
	"fmt"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

// crdRules judges a CR with the API server's own code, as the API server
// judges it against its CRD before any admission webhook: defaults, value
// validations, list types and x-kubernetes-validations rules.
type crdRules struct {
	structural *structuralschema.Structural
	schema     validation.SchemaValidator
	rules      *cel.Validator
}

func newCRDRules(openAPIV3Schema map[string]any) (*crdRules, error) {
	data, err := json.Marshal(openAPIV3Schema)
	if err != nil {
		return nil, fmt.Errorf("reading the CRD's rules: %w", err)
	}
	var declared apiextensionsv1.JSONSchemaProps
	if err := json.Unmarshal(data, &declared); err != nil {
		return nil, fmt.Errorf("reading the CRD's rules: %w", err)
	}
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&declared, &internal, nil); err != nil {
		return nil, fmt.Errorf("reading the CRD's rules: %w", err)
	}
	structural, err := structuralschema.NewStructural(&internal)
	if err != nil {
		return nil, fmt.Errorf("reading the CRD's rules: %w", err)
	}
	schema, _, err := validation.NewSchemaValidator(&internal)
	if err != nil {
		return nil, fmt.Errorf("reading the CRD's rules: %w", err)
	}
	return &crdRules{
		structural: structural,
		schema:     schema,
		rules:      cel.NewValidator(structural, true, celconfig.PerCallLimit),
	}, nil
}

// refusal is why the API server refuses cr. A nil old judges a create, and
// any other an update of old.
func (c *crdRules) refusal(cr, old map[string]any) error {
	cr = c.defaulted(cr)
	errs := validation.ValidateCustomResource(nil, cr, c.schema)
	errs = append(errs, listtype.ValidateListSetsAndMaps(nil, c.structural, cr)...)
	if len(errs) == 0 && c.rules != nil {
		var previous any
		if old != nil {
			previous = c.defaulted(old)
		}
		errs, _ = c.rules.Validate(context.Background(), nil, c.structural, cr, previous, celconfig.RuntimeCELCostBudget)
	}
	return errs.ToAggregate()
}

// defaulted is a copy of the object with the schema's defaults applied, as
// the API server applies them before it validates.
func (c *crdRules) defaulted(object map[string]any) map[string]any {
	copied := runtime.DeepCopyJSON(object)
	defaulting.Default(copied, c.structural)
	return copied
}
