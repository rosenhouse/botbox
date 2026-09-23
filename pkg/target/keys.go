package target

import (
	"fmt"
	"reflect"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"
)

// unknownKey finds the first key in target.yaml that the declaration has no
// field for, and says where it is. It is nil where it finds none.
func unknownKey(data []byte) error {
	var document yamlv3.Node
	_ = yamlv3.Unmarshal(data, &document) // It leaves YAML it cannot parse empty.
	if len(document.Content) == 0 {
		return nil
	}
	return keyIn(document.Content[0], reflect.TypeFor[declaration](), "")
}

func keyIn(node *yamlv3.Node, declared reflect.Type, path string) error {
	switch {
	case declared.Kind() == reflect.Slice && node.Kind == yamlv3.SequenceNode:
		for i, item := range node.Content {
			if err := keyIn(item, declared.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case declared.Kind() == reflect.Struct && node.Kind == yamlv3.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Tag == "!!merge" {
				continue
			}
			keyPath := key.Value
			if path != "" {
				keyPath = path + "." + key.Value
			}
			field, known := fieldNamed(declared, key.Value)
			if !known {
				return notAKey(key, keyPath, path, declared)
			}
			if err := keyIn(value, field.Type, keyPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// fieldNamed matches a key as the JSON decoder does, whatever its case.
func fieldNamed(declared reflect.Type, key string) (reflect.StructField, bool) {
	for field := range declared.Fields() {
		if strings.EqualFold(jsonName(field), key) {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

func jsonName(field reflect.StructField) string {
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	return name
}

func notAKey(key *yamlv3.Node, keyPath, parent string, declared reflect.Type) error {
	var names []string
	for field := range declared.Fields() {
		names = append(names, jsonName(field))
	}
	if near := nearest(key.Value, names); near != "" {
		return fmt.Errorf("line %d: %s is not a key; did you mean %s?", key.Line, keyPath, near)
	}
	if parent == "" {
		parent = "target.yaml"
	}
	last := len(names) - 1
	return fmt.Errorf("line %d: %s is not a key; %s takes %s and %s",
		key.Line, keyPath, parent, strings.Join(names[:last], ", "), names[last])
}

// nearest is the name closest to key within two edits, or empty.
func nearest(key string, names []string) string {
	best, bestDistance := "", 3
	for _, name := range names {
		if distance := editDistance(strings.ToLower(key), strings.ToLower(name)); distance < bestDistance && distance < len(key) {
			best, bestDistance = name, distance
		}
	}
	return best
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current := make([]int, len(b)+1)
		current[0] = i
		for j := 1; j <= len(b); j++ {
			substitution := previous[j-1]
			if a[i-1] != b[j-1] {
				substitution++
			}
			current[j] = min(previous[j]+1, current[j-1]+1, substitution)
		}
		previous = current
	}
	return previous[len(b)]
}
