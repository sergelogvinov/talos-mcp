/*
Copyright 2026 Serge Logvinov.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package config

import (
	"bytes"
	"errors"
	"fmt"

	yaml "go.yaml.in/yaml/v3"
)

// SecretVisitor gets one secret field of a context and returns its new value.
type SecretVisitor func(context, field, value string) (string, error)

// EditSecrets calls fn on the key and the discovery cluster_secret of each
// context, or only of contextFilter when it is set, in file order. A field
// that is missing or empty is not visited, and one that is not a plain value,
// such as a YAML alias, is an error. It returns the talosconfig with the
// values fn returned; the rest of the file, including comments and unknown
// keys, is kept.
func EditSecrets(data []byte, contextFilter string, fn SecretVisitor) ([]byte, error) {
	doc, err := walkSecrets(data, contextFilter, fn)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer

	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}

	if err := enc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// VisitSecrets calls fn on the secret fields like EditSecrets, without
// changing or encoding the file.
func VisitSecrets(data []byte, contextFilter string, fn func(context, field, value string) error) error {
	_, err := walkSecrets(data, contextFilter, func(name, field, value string) (string, error) {
		return value, fn(name, field, value)
	})

	return err
}

func walkSecrets(data []byte, contextFilter string, fn SecretVisitor) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, errors.Join(ErrInvalidTalosConfig, err)
	}

	if len(doc.Content) == 0 {
		return nil, ErrNoContexts
	}

	contexts := mappingValue(doc.Content[0], "contexts")
	if contexts == nil || len(contexts.Content) == 0 {
		return nil, ErrNoContexts
	}

	found := contextFilter == ""

	for i := 0; i+1 < len(contexts.Content); i += 2 {
		name, ctx := contexts.Content[i].Value, contexts.Content[i+1]
		if contextFilter != "" && name != contextFilter {
			continue
		}

		found = true

		fields := []*yaml.Node{mappingValue(ctx, FieldKey), mappingValue(mappingValue(ctx, "discovery"), FieldClusterSecret)}

		for j, field := range []string{FieldKey, FieldClusterSecret} {
			node := fields[j]
			if node == nil || (node.Kind == yaml.ScalarNode && node.Value == "") {
				continue
			}

			if node.Kind != yaml.ScalarNode || node.Alias != nil {
				return nil, fmt.Errorf("context %q: %s: must be a plain value, not a YAML alias, list or map", name, field)
			}

			value, err := fn(name, field, node.Value)
			if err != nil {
				return nil, fmt.Errorf("context %q: %s: %w", name, field, err)
			}

			node.Value = value
		}
	}

	if !found {
		return nil, fmt.Errorf("%w %q", ErrUnknownContext, contextFilter)
	}

	return &doc, nil
}

// mappingValue returns the value of key in a mapping node, or nil.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}

	return nil
}
