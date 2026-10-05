package target

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// loadRBAC reads every RBAC file the target declares, validates each document
// and appends the roles and cluster roles to the target.
func loadRBAC(dir string, paths []string, t *Target) error {
	if len(paths) == 0 {
		return nil
	}
	roleNames := map[string]bool{}
	clusterRoleNames := map[string]bool{}

	for _, path := range paths {
		resolved := resolve(dir, path)
		info, err := os.Stat(resolved)
		if err != nil {
			return fmt.Errorf("rbac: %w", err)
		}
		if info.IsDir() {
			return fmt.Errorf("rbac: %s is a directory; list each file", resolved)
		}

		documents, err := splitYAMLDocuments(resolved)
		if err != nil {
			return err
		}
		for _, document := range documents {
			if err := loadRBACDocument(resolved, document, t, roleNames, clusterRoleNames); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadRBACDocument decodes one YAML document and validates it as a Role or
// ClusterRole.
func loadRBACDocument(file string, data []byte, t *Target, roleNames, clusterRoleNames map[string]bool) error {
	// Peek at the kind before strict decoding.
	var header struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	if err := yaml.Unmarshal(data, &header); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if header.APIVersion != "rbac.authorization.k8s.io/v1" {
		return fmt.Errorf("%s: apiVersion %q is not rbac.authorization.k8s.io/v1", file, header.APIVersion)
	}

	switch header.Kind {
	case "Role":
		var role rbacv1.Role
		if err := yaml.UnmarshalStrict(data, &role); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if role.Name == "" {
			return fmt.Errorf("%s: a Role has no metadata.name", file)
		}
		if role.Namespace != "" {
			return fmt.Errorf("%s: the Role %s sets metadata.namespace; drop it, because botbox binds roles in each run's own namespace", file, role.Name)
		}
		if roleNames[role.Name] {
			return fmt.Errorf("%s: duplicate Role %s", file, role.Name)
		}
		roleNames[role.Name] = true
		t.Roles = append(t.Roles, role)

	case "ClusterRole":
		var cr rbacv1.ClusterRole
		if err := yaml.UnmarshalStrict(data, &cr); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if cr.Name == "" {
			return fmt.Errorf("%s: a ClusterRole has no metadata.name", file)
		}
		if cr.AggregationRule != nil {
			return fmt.Errorf("%s: the ClusterRole %s sets aggregationRule; botbox copies the rules, not an aggregation", file, cr.Name)
		}
		if clusterRoleNames[cr.Name] {
			return fmt.Errorf("%s: duplicate ClusterRole %s", file, cr.Name)
		}
		clusterRoleNames[cr.Name] = true
		t.ClusterRoles = append(t.ClusterRoles, cr)

	default:
		return fmt.Errorf("%s: kind %s is not Role or ClusterRole", file, header.Kind)
	}
	return nil
}

// splitYAMLDocuments reads a YAML file and splits it into individual documents.
func splitYAMLDocuments(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	var documents [][]byte
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		// Skip empty or comment-only documents.
		asJSON, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if string(asJSON) == "null" {
			continue
		}
		documents = append(documents, doc)
	}
	if len(documents) == 0 {
		return nil, fmt.Errorf("%s: holds no object", path)
	}
	return documents, nil
}
