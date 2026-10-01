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
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// identity returns unlock options with a new age identity, and its recipient.
func identity(t *testing.T) (secrets.Options, age.Recipient) {
	t.Helper()

	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "age.key")
	require.NoError(t, os.WriteFile(path, []byte(id.String()+"\n"), 0o600))

	return secrets.Options{IdentityFiles: []string{path}}, id.Recipient()
}

// encryptValue encrypts a base64 talosconfig value to r.
func encryptValue(t *testing.T, value string, r age.Recipient) string {
	t.Helper()

	plain, err := base64.StdEncoding.DecodeString(value)
	require.NoError(t, err)

	encrypted, err := secrets.Encrypt(plain, r)
	require.NoError(t, err)

	return encrypted
}

func encryptedConfig(prodKey, prodSecret, stagingKey string) string {
	return fmt.Sprintf(`
context: prod
contexts:
  prod:
    endpoints: [10.0.0.10]
    ca: Y2E=
    crt: Y3J0
    key: %s
    discovery:
      cluster_id: prod-id
      cluster_secret: %s
  staging:
    endpoints: [staging.example.com]
    ca: Y2E=
    crt: Y3J0
    key: %s
`, prodKey, prodSecret, stagingKey)
}

func TestParseTalosConfigEncrypted(t *testing.T) {
	unlock, recipient := identity(t)
	_, other := identity(t)

	key := encryptValue(t, "a2V5", recipient)
	secret := encryptValue(t, validSecret, recipient)

	t.Run("mixed", func(t *testing.T) {
		tc, err := config.ParseTalosConfig([]byte(encryptedConfig(key, secret, "a2V5")), "", config.WithUnlock(unlock))
		require.NoError(t, err)

		assert.Equal(t, []string{"prod", "staging"}, tc.Contexts())
		assert.Equal(t, "a2V5", tc.Config.Contexts["prod"].Key)
		assert.Equal(t, validSecret, tc.Discovery["prod"].ClusterSecret)
		assert.Equal(t, []string{"staging"}, tc.PlaintextKeys)
		assert.Empty(t, tc.Warnings)
	})

	t.Run("undecryptable context is skipped", func(t *testing.T) {
		input := encryptedConfig(encryptValue(t, "a2V5", other), secret, "a2V5")

		tc, err := config.ParseTalosConfig([]byte(input), "", config.WithUnlock(unlock))
		require.NoError(t, err)

		assert.Equal(t, []string{"staging"}, tc.Contexts())
		assert.Equal(t, []string{"prod"}, tc.Skipped)
		assert.Equal(t, []string{
			`context "prod" skipped: key: no identity matches the recipients`,
			`current context "prod" is not usable, using "staging"`,
		}, tc.Warnings)
		assert.NotContains(t, tc.Discovery, "prod")
	})

	t.Run("undecryptable cluster_secret skips the context", func(t *testing.T) {
		input := encryptedConfig(key, encryptValue(t, validSecret, other), "a2V5")

		tc, err := config.ParseTalosConfig([]byte(input), "", config.WithUnlock(unlock))
		require.NoError(t, err)

		assert.Equal(t, []string{"staging"}, tc.Contexts())
		assert.Equal(t, `context "prod" skipped: cluster_secret: no identity matches the recipients`, tc.Warnings[0])
	})

	t.Run("undecryptable --context is an error", func(t *testing.T) {
		input := encryptedConfig(encryptValue(t, "a2V5", other), secret, "a2V5")

		_, err := config.ParseTalosConfig([]byte(input), "prod", config.WithUnlock(unlock))
		require.EqualError(t, err, `context "prod": key: no identity matches the recipients`)
	})

	t.Run("every context skipped", func(t *testing.T) {
		bad := encryptValue(t, "a2V5", other)

		_, err := config.ParseTalosConfig([]byte(encryptedConfig(bad, secret, bad)), "", config.WithUnlock(unlock))
		require.ErrorIs(t, err, config.ErrNoContexts)
	})

	t.Run("no unlock source", func(t *testing.T) {
		_, err := config.ParseTalosConfig([]byte(encryptedConfig(key, secret, "a2V5")), "")
		require.ErrorIs(t, err, config.ErrNoUnlockSource)
	})

	t.Run("encrypted fields of a filtered-out context need no unlock source", func(t *testing.T) {
		tc, err := config.ParseTalosConfig([]byte(encryptedConfig(key, secret, "a2V5")), "staging")
		require.NoError(t, err)
		assert.Equal(t, []string{"staging"}, tc.Contexts())
	})

	t.Run("unreadable identity is an error", func(t *testing.T) {
		_, err := config.ParseTalosConfig([]byte(encryptedConfig(key, secret, "a2V5")), "",
			config.WithUnlock(secrets.Options{IdentityFiles: []string{"/nonexistent/age.key"}}))
		require.ErrorContains(t, err, "identity /nonexistent/age.key")
	})

	t.Run("plaintext needs no unlock source", func(t *testing.T) {
		tc, err := config.ParseTalosConfig([]byte(twoContexts), "")
		require.NoError(t, err)
		assert.Equal(t, []string{"prod", "staging"}, tc.PlaintextKeys)
	})
}

func TestRemoveRecordsSkipped(t *testing.T) {
	tc, err := config.ParseTalosConfig([]byte(twoContexts), "")
	require.NoError(t, err)

	require.NoError(t, tc.Remove("staging", "no role"))
	assert.Equal(t, []string{"staging"}, tc.Skipped)
	assert.Equal(t, []string{"prod"}, tc.PlaintextKeys)
}

func TestEditSecrets(t *testing.T) {
	input := `# my clusters
context: prod
contexts:
  prod:
    endpoints: [10.0.0.10]
    ca: Y2E=
    crt: Y3J0
    key: a2V5 # the key
    discovery:
      cluster_id: prod-id
      cluster_secret: c2VjcmV0
      unknown: kept
  staging:
    endpoints: [staging.example.com]
    ca: Y2E=
    crt: Y3J0
`

	var visited []string

	out, err := config.EditSecrets([]byte(input), "", func(name, field, value string) (string, error) {
		visited = append(visited, name+"."+field+"="+value)

		return strings.ToUpper(value), nil
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"prod.key=a2V5", "prod.cluster_secret=c2VjcmV0"}, visited)

	for _, want := range []string{"# my clusters", "key: A2V5 # the key", "cluster_secret: C2VJCMV0", "unknown: kept", "ca: Y2E="} {
		assert.Contains(t, string(out), want)
	}

	_, err = config.EditSecrets([]byte(input), "missing", func(_, _, v string) (string, error) { return v, nil })
	require.ErrorIs(t, err, config.ErrUnknownContext)

	_, err = config.EditSecrets([]byte(input), "", func(string, string, string) (string, error) { return "", assert.AnError })
	require.ErrorIs(t, err, assert.AnError)
	require.ErrorContains(t, err, `context "prod": key:`)
}

func TestEditSecretsRejectsAlias(t *testing.T) {
	input := `
contexts:
  prod:
    endpoints: [10.0.0.10]
    key: &shared a2V5
  staging:
    endpoints: [10.0.0.11]
    key: *shared
`

	_, err := config.EditSecrets([]byte(input), "", func(_, _, v string) (string, error) { return v, nil })
	require.EqualError(t, err, `context "staging": key: must be a plain value, not a YAML alias, list or map`)

	err = config.VisitSecrets([]byte(input), "prod", func(string, string, string) error { return nil })
	require.NoError(t, err, "the anchored value itself is plain")
}

func TestNewUnlocker(t *testing.T) {
	unlock, recipient := identity(t)
	input := []byte(encryptedConfig(encryptValue(t, "a2V5", recipient), validSecret, "a2V5"))

	_, err := config.NewUnlocker(input, "", secrets.Options{})
	require.ErrorIs(t, err, config.ErrNoUnlockSource)

	u, err := config.NewUnlocker(input, "staging", secrets.Options{})
	require.NoError(t, err, "staging has no encrypted fields")
	u.Close()

	u, err = config.NewUnlocker(input, "", unlock)
	require.NoError(t, err)
	u.Close()
}

// TestParseTalosConfigNullContext covers a context written with no fields,
// which clientconfig alone panics on.
func TestParseTalosConfigNullContext(t *testing.T) {
	tc, err := config.ParseTalosConfig([]byte(twoContexts+"  old:\n"), "")
	require.NoError(t, err)

	assert.Equal(t, []string{"prod", "staging"}, tc.Contexts())
	assert.Equal(t, []string{"old"}, tc.Skipped)
	assert.Contains(t, tc.Warnings, `context "old" skipped: no endpoints`)
}
