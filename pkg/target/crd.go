package target

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// ReadCRDs reads every document of the CRD manifests the paths name. A path is
// a file or a directory of them, as the cluster reads them.
func ReadCRDs(paths []string) ([]map[string]any, error) {
	var documents []map[string]any
	for _, path := range paths {
		files, err := manifests(path)
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			read, err := readDocuments(file)
			if err != nil {
				return nil, err
			}
			documents = append(documents, read...)
		}
	}
	return documents, nil
}

// manifestExtensions are the files a CRD directory holds.
var manifestExtensions = []string{".yaml", ".yml", ".json"}

func manifests(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading the CRDs: %w", err)
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("reading the CRDs: %w", err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && slices.Contains(manifestExtensions, filepath.Ext(entry.Name())) {
			files = append(files, filepath.Join(path, entry.Name()))
		}
	}
	return files, nil
}

func readDocuments(path string) ([]map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the CRDs: %w", err)
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var documents []map[string]any
	for {
		document, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return documents, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading the CRDs in %s: %w", path, err)
		}
		var decoded map[string]any
		if err := yaml.Unmarshal(document, &decoded); err != nil {
			return nil, fmt.Errorf("reading the CRDs in %s: %w", path, err)
		}
		if decoded != nil {
			documents = append(documents, decoded)
		}
	}
}

// clusterScopedByCRD reports the kinds the CRDs define as cluster-scoped.
func clusterScopedByCRD(documents []map[string]any) func(schema.GroupVersionKind) bool {
	return func(gvk schema.GroupVersionKind) bool {
		for _, document := range documents {
			spec, _ := document["spec"].(map[string]any)
			names, _ := spec["names"].(map[string]any)
			if spec["group"] == gvk.Group && names["kind"] == gvk.Kind && spec["scope"] == "Cluster" {
				return true
			}
		}
		return false
	}
}
