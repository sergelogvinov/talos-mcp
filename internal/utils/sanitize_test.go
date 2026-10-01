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

package utils_test

import (
	"strings"
	"testing"

	"github.com/sergelogvinov/talos-mcp/internal/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitize(t *testing.T) {
	s, err := utils.NewDefaultSanitizer()
	require.NoError(t, err)

	for _, tt := range []struct {
		name           string
		input          string
		expectedOutput string
	}{
		{
			name:           "empty_input",
			input:          "",
			expectedOutput: "",
		},
		{
			name:           "no_match",
			input:          `just a normal log line with no secrets`,
			expectedOutput: `just a normal log line with no secrets`,
		},
		{
			name:           "preserve_non_sensitive",
			input:          `ts=2024-01-01 level=info msg="hello" password=secret count=5`,
			expectedOutput: `ts=2024-01-01 level=info msg="hello" password=*** count=5`,
		},
		{
			name:           "postgres_primary_conninfo",
			input:          `primary_conninfo = 'host=databases sslmode=require sslcert="" user=replication port=5433 password=djkhsjgfdfjlkhg'`,
			expectedOutput: `primary_conninfo = 'host=databases sslmode=require sslcert="" user=replication port=5433 password=***'`,
		},
		{
			name:           "multiple_secrets",
			input:          "password=secret card=4111 1111 1111 1111 email=user@example.com",
			expectedOutput: "password=*** card=*** email=***",
		},
		{
			name:           "python_logs",
			input:          `  File "/common/configurations/environments.py", line 37, in <module>`,
			expectedOutput: `  File "/common/configurations/environments.py", line 37, in <module>`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Sanitize(tt.input)
			assert.Equal(t, tt.expectedOutput, got)
		})
	}
}

func TestSanitize_KeyBased(t *testing.T) {
	s, err := utils.NewDefaultSanitizer()
	require.NoError(t, err)

	for _, tt := range []struct {
		name           string
		input          string
		expectedOutput string
	}{
		{
			name:           "logfmt",
			input:          `password=secret token=abc level=info`,
			expectedOutput: `password=*** token=*** level=info`,
		},
		{
			name:           "logfmt_quoted",
			input:          `password="secret"`,
			expectedOutput: `password="***"`,
		},
		{
			name:           "json",
			input:          `{"token": "abc", "level": "info"}`,
			expectedOutput: `{"token": "***", "level": "info"}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Sanitize(tt.input)
			assert.Equal(t, tt.expectedOutput, got)
		})
	}
}

func TestSanitize_KeyRegexVariants(t *testing.T) {
	s, err := utils.NewDefaultSanitizer()
	require.NoError(t, err)

	for _, tt := range []struct {
		name           string
		input          string
		expectedOutput string
	}{
		{name: "db_password", input: `db_password=hunter2`, expectedOutput: `db_password=***`},
		{name: "user_password", input: `user_password=hunter2`, expectedOutput: `user_password=***`},
		{name: "access_token", input: `access_token=abc`, expectedOutput: `access_token=***`},
		{name: "client_secret", input: `client_secret=shh`, expectedOutput: `client_secret=***`},
		{name: "card_number", input: `card_number=4111111111111111`, expectedOutput: `card_number=***`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Sanitize(tt.input)
			assert.Equal(t, tt.expectedOutput, got)
		})
	}
}

func TestSanitize_ValueBasedCard(t *testing.T) {
	s, err := utils.NewDefaultSanitizer()
	require.NoError(t, err)

	for _, tt := range []struct {
		name           string
		input          string
		expectedOutput string
	}{
		{
			name:           "luhn_valid",
			input:          `card on file: 4111 1111 1111 1111 done`,
			expectedOutput: `card on file: *** done`,
		},
		{
			name:           "luhn_invalid",
			input:          `id=1234567890123456`,
			expectedOutput: `id=1234567890123456`,
		},
		{
			name:           "noisy_short_number",
			input:          `count=123 status=ok`,
			expectedOutput: `count=123 status=ok`,
		},
		{
			name:           "cvv_under_key",
			input:          `cvv=123`,
			expectedOutput: `cvv=***`,
		},
		{
			name:           "bare_cvv_untouched",
			input:          `retry count 123 times`,
			expectedOutput: `retry count 123 times`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Sanitize(tt.input)
			assert.Equal(t, tt.expectedOutput, got)
		})
	}
}

func TestSanitize_PEMBlock(t *testing.T) {
	s, err := utils.NewDefaultSanitizer()
	require.NoError(t, err)

	for _, tt := range []struct {
		name           string
		input          string
		expectedOutput string
	}{
		{
			name: "pem_block",
			input: strings.Join([]string{
				"starting up",
				"-----BEGIN RSA PRIVATE KEY-----",
				"MIIEpAIBAAKCAQEA0X3...redacted...QwE9",
				"-----END RSA PRIVATE KEY-----",
				"done",
			}, "\n"),
			expectedOutput: "starting up\n***\ndone",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Sanitize(tt.input)
			assert.Equal(t, tt.expectedOutput, got)
		})
	}
}

func TestSanitize_TokenPatterns(t *testing.T) {
	s, err := utils.NewDefaultSanitizer()
	require.NoError(t, err)

	for _, tt := range []struct {
		name           string
		input          string
		expectedOutput string
	}{
		{
			name:           "bearer_jwt",
			input:          `Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c`,
			expectedOutput: `Authorization: Bearer ***`,
		},
		{
			name:           "github_token",
			input:          `token=ghp_0123456789abcdefghijklmnopqrstuvwxyz0123`,
			expectedOutput: `token=***`,
		},
		{
			name:           "aws_key",
			input:          `aws_key=AKIAIOSFODNN7EXAMPLE`,
			expectedOutput: `aws_key=***`,
		},
		{
			name:           "email",
			input:          `contact user@example.com for help`,
			expectedOutput: `contact *** for help`,
		},
		{
			name:           "phone",
			input:          `call +1 (555) 123-4567 now`,
			expectedOutput: `call +1 *** now`,
		},
		{
			name:           "url_credentials",
			input:          `db=postgres://replication:secretpw@db.internal:5432/main`,
			expectedOutput: `db=postgres://***@db.internal:5432/main`,
		},
		{
			name:           "url_credentials_login_only",
			input:          `connecting to https://alice@files.example.io/share`,
			expectedOutput: `connecting to https://***@files.example.io/share`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Sanitize(tt.input)
			assert.Equal(t, tt.expectedOutput, got)
		})
	}
}

func TestSanitize_Customization(t *testing.T) {
	for _, tt := range []struct {
		name           string
		setup          func(*utils.Sanitizer)
		input          string
		expectedOutput string
	}{
		{
			name:           "set_mask",
			setup:          func(s *utils.Sanitizer) { s.SetMask("[REDACTED]") },
			input:          `password=secret`,
			expectedOutput: `password=[REDACTED]`,
		},
		{
			name:           "add_sensitive_keys",
			setup:          func(s *utils.Sanitizer) { s.AddSensitiveKeys("custom_secret") },
			input:          `custom_secret=value`,
			expectedOutput: `custom_secret=***`,
		},
		{
			name: "add_value_patterns",
			setup: func(s *utils.Sanitizer) {
				require.NoError(t, s.AddValuePatterns(`(?P<foo>FOO_SECRET_\d+)`))
			},
			input:          `found FOO_SECRET_123 here`,
			expectedOutput: `found *** here`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, err := utils.NewDefaultSanitizer()
			require.NoError(t, err)
			if tt.setup != nil {
				tt.setup(s)
			}
			got := s.Sanitize(tt.input)
			assert.Equal(t, tt.expectedOutput, got)
		})
	}
}

func TestSanitize_BadRegexRejected(t *testing.T) {
	s, err := utils.NewDefaultSanitizer()
	require.NoError(t, err)

	for _, tt := range []struct {
		name    string
		pattern string
	}{
		{name: "key_patterns", pattern: `(`},
		{name: "value_patterns", pattern: `(`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "key_patterns" {
				err := s.AddKeyPatterns(tt.pattern)
				require.Error(t, err)
			} else {
				err := s.AddValuePatterns(tt.pattern)
				require.Error(t, err)
			}
		})
	}
}

func TestLuhnValid(t *testing.T) {
	for _, tt := range []struct {
		name     string
		input    string
		expected bool
	}{
		{name: "luhn_valid_16", input: "4111111111111111", expected: true},
		{name: "luhn_valid_16_spaced", input: "4111 1111 1111 1111", expected: true},
		{name: "luhn_valid_16_alt", input: "4012888888881881", expected: true},
		{name: "luhn_invalid", input: "1234567890123456", expected: false},
		{name: "too_short_11", input: "49927398716", expected: false},
		{name: "too_short_12", input: "499273987164", expected: false},
		{name: "luhn_valid_13", input: "4222222222222", expected: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := utils.LuhnValid(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}
