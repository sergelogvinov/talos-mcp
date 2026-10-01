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
	"testing"

	"github.com/sergelogvinov/talos-mcp/internal/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTalosSanitize(t *testing.T) {
	s, err := utils.NewTalosSanitizer("c2VjcmV0LWNsdXN0ZXIta2V5+/abc=", "short")
	require.NoError(t, err)

	for _, tt := range []struct {
		name           string
		input          string
		expectedOutput string
	}{
		{
			name:           "join_token_in_text",
			input:          `joining cluster with token abcdef.0123456789abcdef`,
			expectedOutput: `joining cluster with token ***`,
		},
		{
			name:           "machine_token_yaml",
			input:          `  token: abcdef.0123456789abcdef`,
			expectedOutput: `  token: ***`,
		},
		{
			name:           "machine_token_logfmt",
			input:          `msg="config loaded" machine.token=r1ju8u.jwzuyqyxhdcf0y5e`,
			expectedOutput: `msg="config loaded" machine.token=***`,
		},
		{
			name:           "cluster_secret_yaml",
			input:          `  secret: Zm9vYmFyYmF6cXV4`,
			expectedOutput: `  secret: ***`,
		},
		{
			name:           "secretbox_secret_quoted_yaml",
			input:          `  secretboxEncryptionSecret: "k1j2+/h3=="`,
			expectedOutput: `  secretboxEncryptionSecret: "***"`,
		},
		{
			name:           "token_yaml_list_item",
			input:          `  - token: abc123`,
			expectedOutput: `  - token: ***`,
		},
		{
			name:           "base64_pem_key",
			input:          `    key: LS0tLS1CRUdJTiBFQzIgUFJJVkFURSBLRVktLS0tLQpNSFFDQVFFRUlB+/==`,
			expectedOutput: `    key: ***`,
		},
		{
			name:           "base64_pem_inline",
			input:          `ca=LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUJQ+/= ok`,
			expectedOutput: `ca=*** ok`,
		},
		{
			name:           "literal_cluster_secret",
			input:          `discovery failed for secret c2VjcmV0LWNsdXN0ZXIta2V5+/abc= on node`,
			expectedOutput: `discovery failed for secret *** on node`,
		},
		{
			name:           "short_literal_ignored",
			input:          `a short line`,
			expectedOutput: `a short line`,
		},
		{
			name:           "non_sensitive_yaml_preserved",
			input:          "  hostname: worker-1\n  endpoint: https://10.0.0.1:6443",
			expectedOutput: "  hostname: worker-1\n  endpoint: https://10.0.0.1:6443",
		},
		{
			name:           "multiline_config_dump",
			input:          "machine:\n  type: worker\n  token: abcdef.0123456789abcdef\ncluster:\n  secret: Zm9vYmFy\n  clusterName: prod",
			expectedOutput: "machine:\n  type: worker\n  token: ***\ncluster:\n  secret: ***\n  clusterName: prod",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expectedOutput, s.Sanitize(tt.input))
		})
	}
}

func TestDefaultSanitizerSkipsYAML(t *testing.T) {
	s, err := utils.NewDefaultSanitizer()
	require.NoError(t, err)

	assert.Equal(t, `  token: abc123`, s.Sanitize(`  token: abc123`))
}

func TestAddLiteralsLongestFirst(t *testing.T) {
	s, err := utils.NewTalosSanitizer("abcdefgh", "abcdefgh-ijklmnop")
	require.NoError(t, err)

	assert.Equal(t, `x=*** y=***`, s.Sanitize(`x=abcdefgh-ijklmnop y=abcdefgh`))
}
