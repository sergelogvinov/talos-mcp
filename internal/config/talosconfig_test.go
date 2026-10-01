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

package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validSecret is base64 of 32 bytes, a valid AES-256 key.
const validSecret = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

const twoContexts = `
context: prod
contexts:
  prod:
    endpoints: [10.0.0.10]
    nodes: [10.0.0.11]
    ca: Y2E=
    crt: Y3J0
    key: a2V5
  staging:
    endpoints: [staging.example.com]
    ca: Y2E=
    crt: Y3J0
    key: a2V5
`

func TestParseTalosConfig(t *testing.T) {
	for _, tt := range []struct {
		name             string
		input            string
		contextFilter    string
		expectedContexts []string
		expectedCurrent  string
		expectedWarnings []string
		expectedError    error
		expectedErrorMsg string
	}{
		{
			name:             "all contexts, current from file",
			input:            twoContexts,
			expectedContexts: []string{"prod", "staging"},
			expectedCurrent:  "prod",
		},
		{
			name:             "context filter selects one context",
			input:            twoContexts,
			contextFilter:    "staging",
			expectedContexts: []string{"staging"},
			expectedCurrent:  "staging",
		},
		{
			name:             "unknown context filter lists valid names",
			input:            twoContexts,
			contextFilter:    "dev",
			expectedError:    config.ErrUnknownContext,
			expectedErrorMsg: `unknown context "dev": talosconfig has prod, staging`,
		},
		{
			name: "context filter on an unusable context is an error",
			input: twoContexts + `
  broken:
    endpoints: []
    ca: Y2E=
    crt: Y3J0
    key: a2V5
`,
			contextFilter:    "broken",
			expectedErrorMsg: `context "broken": no endpoints`,
		},
		{
			name: "context without endpoints is skipped",
			input: twoContexts + `
  broken:
    ca: Y2E=
    crt: Y3J0
    key: a2V5
`,
			expectedContexts: []string{"prod", "staging"},
			expectedCurrent:  "prod",
			expectedWarnings: []string{`context "broken" skipped: no endpoints`},
		},
		{
			name: "context without credentials is skipped",
			input: twoContexts + `
  broken:
    endpoints: [10.0.0.20]
    ca: Y2E=
`,
			expectedContexts: []string{"prod", "staging"},
			expectedCurrent:  "prod",
			expectedWarnings: []string{`context "broken" skipped: missing crt, key`},
		},
		{
			name: "omni context is skipped",
			input: twoContexts + `
  omni:
    endpoints: [https://omni.example.com]
    auth:
      siderov1:
        identity: user@example.com
`,
			expectedContexts: []string{"prod", "staging"},
			expectedCurrent:  "prod",
			expectedWarnings: []string{`context "omni" skipped: omni (auth.siderov1) contexts are not supported`},
		},
		{
			name: "unusable current context falls back to first usable",
			input: `
context: broken
contexts:
  broken:
    endpoints: [10.0.0.20]
  staging:
    endpoints: [staging.example.com]
    ca: Y2E=
    crt: Y3J0
    key: a2V5
`,
			expectedContexts: []string{"staging"},
			expectedCurrent:  "staging",
			expectedWarnings: []string{
				`context "broken" skipped: missing ca, crt, key`,
				`current context "broken" is not usable, using "staging"`,
			},
		},
		{
			name: "missing current context falls back to first usable",
			input: `
contexts:
  b:
    endpoints: [10.0.0.2]
    ca: Y2E=
    crt: Y3J0
    key: a2V5
  a:
    endpoints: [10.0.0.1]
    ca: Y2E=
    crt: Y3J0
    key: a2V5
`,
			expectedContexts: []string{"a", "b"},
			expectedCurrent:  "a",
			expectedWarnings: []string{`talosconfig has no current context, using "a"`},
		},
		{
			name:          "no contexts",
			input:         "context: prod\n",
			expectedError: config.ErrNoContexts,
		},
		{
			name: "no usable contexts",
			input: `
contexts:
  broken:
    endpoints: [10.0.0.20]
`,
			expectedError: config.ErrNoContexts,
		},
		{
			name:          "invalid yaml",
			input:         "contexts: [",
			expectedError: config.ErrInvalidTalosConfig,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc, err := config.ParseTalosConfig([]byte(tt.input), tt.contextFilter)

			if tt.expectedError != nil || tt.expectedErrorMsg != "" {
				require.Error(t, err)

				if tt.expectedError != nil {
					assert.ErrorIs(t, err, tt.expectedError)
				}

				if tt.expectedErrorMsg != "" {
					assert.EqualError(t, err, tt.expectedErrorMsg)
				}

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expectedContexts, tc.Contexts())
			assert.Equal(t, tt.expectedCurrent, tc.Current)
			assert.Equal(t, tt.expectedCurrent, tc.Config.Context)
			assert.Equal(t, tt.expectedWarnings, tc.Warnings)
		})
	}
}

func TestParseTalosConfigKeepsContextFields(t *testing.T) {
	tc, err := config.ParseTalosConfig([]byte(twoContexts), "")
	require.NoError(t, err)

	prod := tc.Config.Contexts["prod"]
	require.NotNil(t, prod)
	assert.Equal(t, []string{"10.0.0.10"}, prod.Endpoints)
	assert.Equal(t, []string{"10.0.0.11"}, prod.Nodes)
	assert.Equal(t, "Y2E=", prod.CA)
	assert.Equal(t, "Y3J0", prod.Crt)
	assert.Equal(t, "a2V5", prod.Key)
}

func TestParseTalosConfigDiscovery(t *testing.T) {
	for _, tt := range []struct {
		name              string
		discovery         string
		expectedDiscovery *config.DiscoveryConfig
		expectedWarnings  []string
	}{
		{
			name:      "no discovery block",
			discovery: "",
		},
		{
			name: "valid block gets default endpoint",
			discovery: `
    discovery:
      cluster_id: cluster-1
      cluster_secret: ` + validSecret,
			expectedDiscovery: &config.DiscoveryConfig{
				Endpoint:      config.DefaultDiscoveryEndpoint,
				ClusterID:     "cluster-1",
				ClusterSecret: validSecret,
			},
		},
		{
			name: "custom endpoint is kept",
			discovery: `
    discovery:
      endpoint: discovery.example.com:3000
      cluster_id: cluster-1
      cluster_secret: ` + validSecret,
			expectedDiscovery: &config.DiscoveryConfig{
				Endpoint:      "discovery.example.com:3000",
				ClusterID:     "cluster-1",
				ClusterSecret: validSecret,
			},
		},
		{
			name: "missing cluster_id disables discovery",
			discovery: `
    discovery:
      cluster_secret: ` + validSecret,
			expectedWarnings: []string{`context "prod": discovery disabled: invalid discovery block: cluster_id is required`},
		},
		{
			name: "missing cluster_secret disables discovery",
			discovery: `
    discovery:
      cluster_id: cluster-1`,
			expectedWarnings: []string{`context "prod": discovery disabled: invalid discovery block: cluster_secret is required`},
		},
		{
			name: "non-base64 secret disables discovery",
			discovery: `
    discovery:
      cluster_id: cluster-1
      cluster_secret: "not base64!"`,
			expectedWarnings: []string{`context "prod": discovery disabled: invalid discovery block: cluster_secret is not valid base64`},
		},
		{
			name: "wrong key size disables discovery",
			discovery: `
    discovery:
      cluster_id: cluster-1
      cluster_secret: c2hvcnQ=`,
			expectedWarnings: []string{`context "prod": discovery disabled: invalid discovery block: cluster_secret is not a valid AES key (5 bytes)`},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := `
context: prod
contexts:
  prod:
    endpoints: [10.0.0.10]
    ca: Y2E=
    crt: Y3J0
    key: a2V5` + tt.discovery + "\n"

			tc, err := config.ParseTalosConfig([]byte(input), "")
			require.NoError(t, err, "a discovery block never disables the context")

			assert.Equal(t, []string{"prod"}, tc.Contexts())
			assert.Equal(t, tt.expectedDiscovery, tc.Discovery["prod"])
			assert.Equal(t, tt.expectedWarnings, tc.Warnings)

			for _, w := range tc.Warnings {
				assert.NotContains(t, w, validSecret, "warnings must not leak the secret")
			}
		})
	}
}

func TestParseTalosConfigDiscoveryOfSkippedContext(t *testing.T) {
	input := twoContexts + `
  broken:
    endpoints: []
    discovery:
      cluster_id: cluster-1
      cluster_secret: ` + validSecret + "\n"

	tc, err := config.ParseTalosConfig([]byte(input), "")
	require.NoError(t, err)

	assert.NotContains(t, tc.Discovery, "broken")
	assert.Empty(t, tc.Discovery)
}

func TestParseTalosConfigDiscoveryFilteredOut(t *testing.T) {
	input := `
context: prod
contexts:
  prod:
    endpoints: [10.0.0.10]
    ca: Y2E=
    crt: Y3J0
    key: a2V5
    discovery:
      cluster_id: cluster-1
      cluster_secret: ` + validSecret + `
  staging:
    endpoints: [staging.example.com]
    ca: Y2E=
    crt: Y3J0
    key: a2V5
`

	tc, err := config.ParseTalosConfig([]byte(input), "staging")
	require.NoError(t, err)

	assert.Equal(t, []string{"staging"}, tc.Contexts())
	assert.Empty(t, tc.Discovery)
}

func TestLoadTalosConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "talosconfig")
	require.NoError(t, os.WriteFile(path, []byte(twoContexts), 0o600))

	tc, err := config.LoadTalosConfig(path, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"prod", "staging"}, tc.Contexts())

	missing := filepath.Join(dir, "missing")
	_, err = config.LoadTalosConfig(missing, "")
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.NoFileExists(t, missing, "a missing talosconfig must not be created")
}

func TestTalosConfigRemove(t *testing.T) {
	input := `
context: prod
contexts:
  prod:
    endpoints: [10.0.0.10]
    ca: Y2E=
    crt: Y3J0
    key: a2V5
    discovery:
      cluster_id: cluster-1
      cluster_secret: ` + validSecret + `
  staging:
    endpoints: [staging.example.com]
    ca: Y2E=
    crt: Y3J0
    key: a2V5
`

	tc, err := config.ParseTalosConfig([]byte(input), "")
	require.NoError(t, err)

	require.NoError(t, tc.Remove("prod", "no known role"))
	assert.Equal(t, []string{"staging"}, tc.Contexts())
	assert.Empty(t, tc.Discovery, "discovery of a removed context is dropped")
	assert.Equal(t, "staging", tc.Current)
	assert.Equal(t, "staging", tc.Config.Context)
	assert.Equal(t, []string{
		`context "prod" skipped: no known role`,
		`current context "prod" is not usable, using "staging"`,
	}, tc.Warnings)

	require.NoError(t, tc.Remove("unknown", "ignored"), "removing an unknown context is a no-op")
	require.ErrorIs(t, tc.Remove("staging", "no known role"), config.ErrNoContexts)
}

func TestTalosConfigRemoveFilteredContext(t *testing.T) {
	tc, err := config.ParseTalosConfig([]byte(twoContexts), "staging")
	require.NoError(t, err)

	require.EqualError(t, tc.Remove("staging", "no known role"), `context "staging": no known role`)
	assert.Equal(t, []string{"staging"}, tc.Contexts())
}
