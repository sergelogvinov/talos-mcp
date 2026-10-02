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

// YAML tags of the nodes SetContext creates.
const (
	yamlMapTag = "!!map"
	yamlStrTag = "!!str"
)

// ErrContextExists is returned by SetContext for a context that is already
// in the talosconfig when replace is not set.
var ErrContextExists = errors.New("context already exists")

// SetContext adds context name to the talosconfig in data, or replaces it
// when replace is set. Empty data starts a new talosconfig. The context
// becomes the current one when the file has none. The rest of the file,
// including comments and unknown keys, is kept.
func SetContext(data []byte, name string, c *ContextEntry, replace bool) ([]byte, error) {
	if name == "" {
		return nil, errors.New("context name is empty")
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, errors.Join(ErrInvalidTalosConfig, err)
	}

	if len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: yamlMapTag}}}
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: not a YAML mapping", ErrInvalidTalosConfig)
	}

	var value yaml.Node
	if err := value.Encode(c); err != nil {
		return nil, err
	}

	if current := mappingValue(root, "context"); current == nil {
		setMappingValue(root, "context", &yaml.Node{Kind: yaml.ScalarNode, Tag: yamlStrTag, Value: name})
	} else if current.Kind == yaml.ScalarNode && current.Value == "" {
		current.Tag, current.Value = yamlStrTag, name
	}

	contexts := mappingValue(root, "contexts")
	if contexts == nil || (contexts.Kind == yaml.ScalarNode && contexts.Tag == "!!null") {
		contexts = &yaml.Node{Kind: yaml.MappingNode, Tag: yamlMapTag}
		setMappingValue(root, "contexts", contexts)
	}

	if contexts.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: contexts is not a mapping", ErrInvalidTalosConfig)
	}

	if mappingValue(contexts, name) != nil && !replace {
		return nil, fmt.Errorf("%w: %q", ErrContextExists, name)
	}

	setMappingValue(contexts, name, &value)

	var buf bytes.Buffer

	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}

	if err := enc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// setMappingValue sets key in a mapping node, appending it when missing.
func setMappingValue(node *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = value

			return
		}
	}

	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: yamlStrTag, Value: key}, value)
}
