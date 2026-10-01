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

package utils

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
)

// minLiteralLen is the shortest literal accepted by AddLiterals. Shorter
// values would mask ordinary words in log lines.
const minLiteralLen = 8

// talosValuePatterns are Talos-specific secret formats. They run before the
// default value patterns, so the generic pattern cannot mask only part of a
// base64 value. See docs/design.md §10.
var talosValuePatterns = []string{
	// Base64-encoded PEM ("-----BEGIN" encodes to "LS0tLS1CRUdJTi"), as found
	// in machine.ca.key, cluster.ca.crt and other config dump fields.
	`(?P<b64pem>LS0tLS1CRUdJTi[A-Za-z0-9+/=]*)`,
	// Talos join token and Kubernetes bootstrap token: [a-z0-9]{6}.[a-z0-9]{16}.
	`(?P<jointoken>\b[a-z0-9]{6}\.[a-z0-9]{16}\b)`,
}

// NewTalosSanitizer returns a Sanitizer with the default keys and patterns,
// the Talos-specific value patterns, YAML "key: value" masking for config
// dumps (machine.token, cluster.secret, ...) and the given literal values,
// such as the configured discovery cluster_secret values.
func NewTalosSanitizer(literals ...string) (*Sanitizer, error) {
	s := &Sanitizer{
		exactKeys: make(map[string]struct{}, len(defaultSensitiveKeys)),
		mask:      defaultMask,
		yamlKeys:  true,
	}
	s.AddSensitiveKeys(defaultSensitiveKeys...)
	if err := s.AddKeyPatterns(defaultKeyPatterns...); err != nil {
		return nil, err
	}
	if err := s.AddValuePatterns(talosValuePatterns...); err != nil {
		return nil, err
	}
	if err := s.AddValuePatterns(defaultValuePatterns...); err != nil {
		return nil, err
	}
	s.AddLiterals(literals...)
	return s, nil
}

// AddLiterals adds exact strings that are always masked, wherever they appear.
// Values shorter than minLiteralLen are ignored.
func (s *Sanitizer) AddLiterals(values ...string) {
	for _, v := range values {
		if len(v) < minLiteralLen {
			continue
		}
		if s.literals == nil {
			s.literals = make(map[string]struct{})
		}
		s.literals[v] = struct{}{}
	}
	if len(s.literals) == 0 {
		return
	}

	// strings.Replacer tries the pairs in argument order, so put longer
	// literals first: a literal that contains another one is masked whole.
	keys := make([]string, 0, len(s.literals))
	for v := range s.literals {
		keys = append(keys, v)
	}
	slices.SortFunc(keys, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})

	pairs := make([]string, 0, 2*len(keys))
	for _, v := range keys {
		pairs = append(pairs, v, s.mask)
	}
	s.literalReplacer = strings.NewReplacer(pairs...)
}

// maskLiterals replaces every configured literal with the mask.
func (s *Sanitizer) maskLiterals(input string) string {
	if s.literalReplacer == nil {
		return input
	}
	return s.literalReplacer.Replace(input)
}

// maskYAMLKeys masks the value of a YAML-style "key: value" line whose key
// ends with a sensitive key name. Quotes around the value are preserved.
func (s *Sanitizer) maskYAMLKeys(line string) string {
	if s.yamlKeyRe == nil {
		return line
	}
	m := s.yamlKeyRe.FindStringSubmatchIndex(line)
	if m == nil {
		return line
	}
	i := s.yamlKeyRe.SubexpIndex("yval")
	start, end := m[2*i], m[2*i+1]
	if start < 0 {
		return line
	}

	val := line[start:end]
	repl := s.mask
	if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
		repl = string(val[0]) + s.mask + string(val[0])
	}
	return line[:start] + repl + line[end:]
}

// buildYAMLKeyRegex matches an indented YAML mapping line (optionally a list
// item) whose key ends with one of the sensitive key alternatives, e.g.
// "  token: x", "  - secret: x" or "  secretboxEncryptionSecret: x". The
// first value token is captured in group "yval".
func buildYAMLKeyRegex(keyAlt string) *regexp.Regexp {
	return regexp.MustCompile(
		`(?i)^\s*(?:-\s+)?[\w.-]*?(?:` + keyAlt + `)\s*:\s+` +
			`(?P<yval>"[^"\n]*"|'[^'\n]*'|[^\s#]+)`,
	)
}
