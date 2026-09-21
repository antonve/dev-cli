// Package document decodes repository-authored YAML or JSON objects.
package document

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"sigs.k8s.io/yaml"
	yamlv2 "sigs.k8s.io/yaml/goyaml.v2"
)

// Unmarshal accepts exactly one mapping document, rejecting duplicate keys and
// additional documents before callers can make Kubernetes changes. Kubernetes
// Lists represent multiple dependency objects without an implicit stream.
func Unmarshal(data []byte, out any) error {
	decoder := yamlv2.NewDecoder(bytes.NewReader(data))
	decoder.SetStrict(true)
	var object any
	if err := decoder.Decode(&object); err != nil {
		return err
	}
	if _, ok := object.(map[any]any); !ok {
		return fmt.Errorf("expected a YAML or JSON object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err != nil {
			return fmt.Errorf("trailing document: %w", err)
		}
		return fmt.Errorf("multiple YAML documents are not supported; use a Kubernetes List for dependency manifests")
	}
	converted, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(converted, out)
}
