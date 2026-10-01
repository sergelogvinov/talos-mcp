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

// Package utils provides shared helpers for the talos-mcp server.
// It masks sensitive values inside free-form log text before they are surfaced to the
// MCP client. Redaction is unconditional and one-way: there is no escape hatch
// and no way to recover the original value.
package utils

import (
	"regexp"
	"slices"
	"strings"
)

// defaultMask is the placeholder used to replace sensitive values when the
// Sanitizer is constructed without an explicit mask.
const defaultMask = "***"

// defaultSensitiveKeys are the exact key names whose values are masked by
// NewDefaultSanitizer. See mimiops docs/logs.md §4.1.
var defaultSensitiveKeys = []string{
	"password", "passwd", "secret", "token", "api_key", "apikey",
	"access_key", "access_token", "refresh_token", "auth_token",
	"client_secret", "client_id", "card_number", "cardnumber",
	"cc_number", "ccn", "cvv", "cvc", "pin", "ssn", "phone",
	"telephone", "mobile", "email", "authorization", "cookie",
	"private_key", "privatekey", "aws_secret_access_key",
	"connection_string", "dsn",
}

// defaultKeyPatterns are regexes matching sensitive key variants
// (e.g. db_password, access_token). See mimiops docs/logs.md §4.1.
var defaultKeyPatterns = []string{
	`(?i)(password|passwd|pwd)\b`,
	`(?i)(token|secret|credential|creds|apikey|api_key)\b`,
	`(?i)(card|cc|cvv|cvc)\b`,
	`(?i)(phone|telephone|mobile|tel)\b`,
}

// defaultValuePatterns are regexes matching high-confidence secret formats
// that appear without a sensitive key. See mimiops docs/logs.md §4.2.
//
// Each pattern must use a single capturing group: group 1 is the value that
// gets replaced by the mask. The card-number pattern is intentionally broad
// (digit runs with optional separators); candidates are Luhn-validated in Go
// before masking to avoid false positives.
var defaultValuePatterns = []string{
	// Credit-card candidates: 13-19 digits, optional spaces or dashes.
	// Luhn-validated in Go.
	`(?P<card>(?:\d[ -]?){12,18}\d)`,
	// JWT / Bearer token (three base64url segments).
	`(?P<jwt>eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)`,
	// GitHub tokens.
	`(?P<github>gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`,
	// Gitlab tokens.
	`(?P<gitlab>glpat-[A-Za-z0-9]{20,})`,
	// Slack tokens.
	`(?P<slack>xox[baprs]-[A-Za-z0-9-]+)`,
	// AWS access key id.
	`(?P<aws>(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16})`,
	// GCP access key id.
	`(?P<gcp>AIza[\w-]{35})`,
	// Stripe live/test keys.
	`(?P<stripe>(?:sk|pk)_(?:live|test)_[A-Za-z0-9]{16,})`,
	// Twilio tokens.
	`(?P<twilio>SK[0-9a-fA-F]{32})`,
	// URL credentials are handled by urlCredentialRe (added separately so it
	// can be rebuilt with scheme/host preserved). Listed here only as a
	// placeholder; the actual compiled regex is urlCredentialRe.
	urlCredentialRe.String(),
	// Generic long hex/base64url secret (>= 32 chars of [A-Za-z0-9_-]).
	// '/' and '+' are deliberately excluded so that file paths (common in
	// stack traces) are not mistaken for secrets.
	`(?P<generic>[A-Za-z0-9_-]{32,})`,
	// Email.
	`(?P<email>[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,})`,
	// Phone: E.164 (+CC...) or grouped US-style with separators/parens.
	// Bare digit runs (dates, ids) are excluded by requiring either a
	// leading + or at least one grouping separator.
	`(?P<phone>\+\d{1,3}[\d\- ]{7,}\d|\(\d{3}\) ?\d{3}[\- ]?\d{4}|\d{3}[\- ]\d{3}[\- ]\d{4})`,
}

// pemBlockPattern matches a full PEM private key block across multiple lines.
var pemBlockPattern = regexp.MustCompile(
	`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`,
)

// urlCredentialRe matches a scheme://user[:password]@host URL so the
// userinfo can be masked while preserving the scheme and host.
var urlCredentialRe = regexp.MustCompile(
	`(?P<urlscheme>[A-Za-z][A-Za-z0-9+.\-]*)://(?P<urlcred>[^\s/@:]+(?::[^\s/@]+)?)@(?P<urlhost>[^\s/@:]+)`,
)

// Sanitizer masks sensitive values in log text. It is safe for concurrent use
// after construction: all regexes are compiled once at build time and never
// mutated.
type Sanitizer struct {
	exactKeys     map[string]struct{}
	keyPatterns   []*regexp.Regexp
	valuePatterns []*regexp.Regexp
	mask          string

	// combinedKeyRe is rebuilt whenever keys/patterns change. It matches a
	// sensitive key followed by an assignment and a value, in either logfmt
	// or JSON style.
	combinedKeyRe *regexp.Regexp

	// yamlKeys enables masking of YAML-style "key: value" lines. yamlKeyRe
	// is rebuilt together with combinedKeyRe. See sanitize_talos.go.
	yamlKeys  bool
	yamlKeyRe *regexp.Regexp

	// literals is the set of exact strings that are always masked, and
	// literalReplacer replaces them, longest first.
	literals        map[string]struct{}
	literalReplacer *strings.Replacer
}

// NewDefaultSanitizer returns a Sanitizer with the default sensitive keys and patterns.
func NewDefaultSanitizer() (*Sanitizer, error) {
	s := &Sanitizer{
		exactKeys: make(map[string]struct{}, len(defaultSensitiveKeys)),
		mask:      defaultMask,
	}
	s.AddSensitiveKeys(defaultSensitiveKeys...)
	if err := s.AddKeyPatterns(defaultKeyPatterns...); err != nil {
		return nil, err
	}
	if err := s.AddValuePatterns(defaultValuePatterns...); err != nil {
		return nil, err
	}
	return s, nil
}

// AddSensitiveKeys adds exact key names whose values should be masked.
// Keys are lower-cased before storage; matching is case-insensitive.
func (s *Sanitizer) AddSensitiveKeys(keys ...string) {
	for _, k := range keys {
		if k == "" {
			continue
		}
		s.exactKeys[strings.ToLower(k)] = struct{}{}
	}
	s.rebuildKeyRegex()
}

// AddKeyPatterns compiles and adds key regexes. Returns an error if any
// pattern fails to compile.
func (s *Sanitizer) AddKeyPatterns(patterns ...string) error {
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return err
		}
		s.keyPatterns = append(s.keyPatterns, re)
	}
	s.rebuildKeyRegex()
	return nil
}

// AddValuePatterns compiles and adds value regexes. Returns an error if any
// pattern fails to compile.
func (s *Sanitizer) AddValuePatterns(patterns ...string) error {
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return err
		}
		s.valuePatterns = append(s.valuePatterns, re)
	}
	return nil
}

// SetMask overrides the mask string used to replace sensitive values.
func (s *Sanitizer) SetMask(mask string) {
	if mask != "" {
		s.mask = mask
	}
}

// Sanitize returns a copy of input with sensitive values masked. The input is
// never mutated. See mimiops docs/logs.md §5 for the order of operations.
func (s *Sanitizer) Sanitize(input string) string {
	if input == "" {
		return input
	}

	// 0. Mask configured literal values, so no pattern can split them.
	out := s.maskLiterals(input)

	// 1. Mask multi-line PEM blocks across the whole input first.
	out = pemBlockPattern.ReplaceAllString(out, s.mask)
	if out == "" {
		return out
	}

	// 2. Split into lines; for each line run value-based masking first (so
	// space-separated values like card numbers are masked as a whole), then
	// key-based masking for structured key=value pairs.
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		masked := s.maskValues(line)
		masked = s.maskKeys(masked)
		masked = s.maskYAMLKeys(masked)
		lines[i] = masked
	}
	return strings.Join(lines, "\n")
}

// maskKeys applies the combined key regex to a single line, replacing the
// value of any sensitive key with the mask while preserving the key, the
// separator, and surrounding quotes.
func (s *Sanitizer) maskKeys(line string) string {
	if s.combinedKeyRe == nil {
		return line
	}
	return s.combinedKeyRe.ReplaceAllStringFunc(line, func(match string) string {
		sub := s.combinedKeyRe.FindStringSubmatch(match)
		if sub == nil {
			return match
		}

		// Find which value group matched. Indices follow the order in the
		// template: key, val, jkey, jval.
		// FindStringSubmatchIndex gives offsets; we use named groups for clarity.
		idx := s.combinedKeyRe.SubexpIndex("val")
		jidx := s.combinedKeyRe.SubexpIndex("jval")

		if idx >= 0 && idx < len(sub) && sub[idx] != "" {
			// logfmt value. Preserve surrounding quotes if present (double or single).
			val := sub[idx]
			if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
				return strings.Replace(match, val, string(val[0])+s.mask+string(val[0]), 1)
			}
			return strings.Replace(match, val, s.mask, 1)
		}
		if jidx >= 0 && jidx < len(sub) && sub[jidx] != "" {
			// JSON value. Preserve surrounding quotes if present.
			val := sub[jidx]
			if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
				return strings.Replace(match, val, `"`+s.mask+`"`, 1)
			}
			return strings.Replace(match, val, s.mask, 1)
		}
		return match
	})
}

// maskValues applies each value regex to a single line. Card-number
// candidates are Luhn-validated in Go before masking; URL credentials are
// rebuilt with the userinfo masked and the scheme/host preserved.
func (s *Sanitizer) maskValues(line string) string {
	for _, re := range s.valuePatterns {
		name := re.SubexpNames() // nil for non-named patterns
		switch {
		case slices.Contains(name, "card"):
			line = re.ReplaceAllStringFunc(line, func(match string) string {
				if !luhnValid(match) {
					return match
				}
				return s.mask
			})
		case slices.Contains(name, "urlscheme"):
			line = re.ReplaceAllStringFunc(line, s.maskURLCredential)
		default:
			line = re.ReplaceAllString(line, s.mask)
		}
	}
	return line
}

// maskURLCredential rebuilds a matched scheme://user:password@host URL with
// the userinfo replaced by the mask, preserving the scheme and host.
func (s *Sanitizer) maskURLCredential(match string) string {
	m := urlCredentialRe.FindStringSubmatch(match)
	if m == nil {
		return match
	}
	schemeIdx := urlCredentialRe.SubexpIndex("urlscheme")
	hostIdx := urlCredentialRe.SubexpIndex("urlhost")
	if schemeIdx < 0 || schemeIdx >= len(m) || hostIdx < 0 || hostIdx >= len(m) {
		return match
	}
	return m[schemeIdx] + "://" + s.mask + "@" + m[hostIdx]
}

// luhnValid reports whether digits form a valid Luhn checksum. Non-digit
// characters are ignored. The Luhn algorithm doubles every second digit
// counting from the right (i.e. the second-to-last digit is doubled).
func luhnValid(s string) bool {
	var digits []int
	for _, r := range s {
		if r < '0' || r > '9' {
			continue
		}
		digits = append(digits, int(r-'0'))
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	double := false
	for _, d := range slices.Backward(digits) {
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// rebuildKeyRegex builds a single combined regex that matches a sensitive key
// (exact or pattern) followed by an assignment and a value, in either logfmt
// or JSON style. The value is captured in group "val".
func (s *Sanitizer) rebuildKeyRegex() {
	if len(s.exactKeys) == 0 && len(s.keyPatterns) == 0 {
		s.combinedKeyRe = nil
		s.yamlKeyRe = nil
		return
	}

	parts := make([]string, 0, len(s.exactKeys)+len(s.keyPatterns))
	for k := range s.exactKeys {
		parts = append(parts, regexp.QuoteMeta(k))
	}
	for _, re := range s.keyPatterns {
		// Strip a leading (?i) so we can wrap the whole alternation once.
		src := re.String()
		src = strings.TrimPrefix(src, "(?i)")
		parts = append(parts, src)
	}
	keyAlt := strings.Join(parts, "|")

	// Two alternatives:
	//  1. logfmt / key=value (value optionally quoted)
	//  2. JSON "key": "value" (value optionally quoted)
	//
	// Group "key" captures the key name; group "val" captures the value
	// (without surrounding quotes for logfmt; with quotes for JSON so we can
	// preserve them).
	tmpl := "(?i)(?:" +
		// logfmt: key = value | key = "value" | key = 'value'. The unquoted
		// form excludes whitespace, commas, braces, and both quote kinds so
		// it stops at the closing quote of a quoted value.
		"(?P<key>(?:" + keyAlt + "))\\s*=\\s*(?P<val>\"[^\"\\n]*\"|'[^'\\n]*'|[^\\s,}'\"]+)" +
		"|" +
		// JSON: \"key\" : \"value\" | \"key\" : value
		"\"(?P<jkey>(?:" + keyAlt + "))\"\\s*:\\s*(?P<jval>\"[^\"\\n]*\"|[^\\s,}]+)" +
		")"

	s.combinedKeyRe = regexp.MustCompile(tmpl)

	if s.yamlKeys {
		s.yamlKeyRe = buildYAMLKeyRegex(keyAlt)
	}
}
