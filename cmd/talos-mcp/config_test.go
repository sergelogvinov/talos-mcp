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

package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/secrets"
	"github.com/sergelogvinov/talos-mcp/internal/talos/talostest"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearUnlockEnv makes the tests independent of the caller's environment.
func clearUnlockEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{envTalosConfig, envContext, envIdentity, envPassphraseFile, envAskpass, envPassphrase, envRequireAll} {
		t.Setenv(key, "")
	}

	setTerminal(t, false)
}

// setTerminal decides whether the terminal prompt is possible, whatever
// terminal go test runs on.
func setTerminal(t *testing.T, ok bool) {
	t.Helper()

	old := haveTerminal
	haveTerminal = func() bool { return ok }

	t.Cleanup(func() { haveTerminal = old })
}

// runRoot runs the talos-mcp command line and returns its stdout and stderr.
func runRoot(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(args)

	err := root.ExecuteContext(t.Context())

	return stdout.String(), stderr.String(), err
}

// ageKey writes a new age identity file and returns its path and recipient.
func ageKey(t *testing.T) (string, string) {
	t.Helper()

	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "age.key")
	require.NoError(t, os.WriteFile(path, []byte(id.String()+"\n"), 0o600))

	return path, id.Recipient().String()
}

// plainTalosconfig writes a talosconfig with a reader context that has a
// discovery block and an operator context without one.
func plainTalosconfig(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "talosconfig")
	require.NoError(t, os.WriteFile(path, []byte(talostest.TalosConfig(t, time.Now(),
		talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Discovery: "prod-id"},
		talostest.Context{Name: "staging", Roles: []string{"os:operator"}},
	)), 0o600))

	return path
}

func loadContexts(t *testing.T, path string) map[string]*clientconfig.Context {
	t.Helper()

	cfg, err := clientconfig.Open(path)
	require.NoError(t, err)

	return cfg.Contexts
}

func TestConfigEncryptRoundTrip(t *testing.T) {
	clearUnlockEnv(t)

	input := plainTalosconfig(t)
	identity, recipient := ageKey(t)
	output := filepath.Join(t.TempDir(), "mcp-config")

	_, stderr, err := runRoot(t, "config", "encrypt", "--talosconfig", input, "-o", output, "--recipient", recipient)
	require.NoError(t, err)
	assert.Equal(t, "encrypted 3 fields, 0 already encrypted, written to "+output+"\n", stderr)

	info, err := os.Stat(output)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	plain, encrypted := loadContexts(t, input), loadContexts(t, output)
	for name, c := range encrypted {
		assert.True(t, secrets.IsEncrypted(c.Key), name)
		assert.Equal(t, plain[name].Crt, c.Crt, "crt stays readable")
		assert.Equal(t, plain[name].CA, c.CA)
	}

	data, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.NotContains(t, string(data), talostest.Secret)
	assert.Contains(t, string(data), "cluster_id: prod-id")

	stdout, _, err := runRoot(t, "config", "check", "--talosconfig", output, "--talosconfig-identity", identity)
	require.NoError(t, err)
	assert.Regexp(t, `prod +reader +key,cluster_secret +ok\n`, stdout)
	assert.Regexp(t, `staging +operator +key +ok\n`, stdout)

	stdout, _, err = runRoot(t, "config", "decrypt", "--talosconfig", output, "--talosconfig-identity", identity)
	require.NoError(t, err)

	tc, err := config.ParseTalosConfig([]byte(stdout), "")
	require.NoError(t, err)

	for name, c := range plain {
		assert.Equal(t, c.Key, tc.Config.Contexts[name].Key, name)
	}

	assert.Equal(t, talostest.Secret, tc.Discovery["prod"].ClusterSecret)

	// Running it again on the output keeps the encrypted fields as they are.
	again := filepath.Join(t.TempDir(), "again")

	_, stderr, err = runRoot(t, "config", "encrypt", "--talosconfig", output, "-o", again, "--recipient", recipient)
	require.NoError(t, err)
	assert.Contains(t, stderr, "encrypted 0 fields, 3 already encrypted")

	againData, err := os.ReadFile(again)
	require.NoError(t, err)
	assert.Equal(t, string(data), string(againData))
}

func TestConfigEncryptSelect(t *testing.T) {
	clearUnlockEnv(t)

	input := plainTalosconfig(t)
	_, recipient := ageKey(t)
	recipients := filepath.Join(t.TempDir(), "recipients")
	require.NoError(t, os.WriteFile(recipients, []byte("# team\n"+recipient+"\n"), 0o600))

	output := filepath.Join(t.TempDir(), "mcp-config")

	_, _, err := runRoot(t, "config", "encrypt", "--talosconfig", input, "-o", output, "-R", recipients, "--context", "prod", "--field", "cluster_secret")
	require.NoError(t, err)

	contexts := loadContexts(t, output)
	assert.False(t, secrets.IsEncrypted(contexts["prod"].Key))
	assert.False(t, secrets.IsEncrypted(contexts["staging"].Key))

	data, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.NotContains(t, string(data), talostest.Secret)
}

func TestConfigEncryptErrors(t *testing.T) {
	clearUnlockEnv(t)

	input := plainTalosconfig(t)
	_, recipient := ageKey(t)
	output := filepath.Join(t.TempDir(), "out")

	for name, tt := range map[string]struct {
		args []string
		want string
	}{
		"no output":          {[]string{"--recipient", recipient}, `required flag(s) "output" not set`},
		"input as output":    {[]string{"-o", input, "--recipient", recipient}, "--output must not be the input talosconfig"},
		"no recipient":       {[]string{"-o", output}, "set --passphrase, --recipient, --recipient-file, --talosconfig-passphrase-file or --talosconfig-askpass"},
		"both":               {[]string{"-o", output, "--recipient", recipient, "--passphrase"}, "--passphrase cannot be combined"},
		"bad field":          {[]string{"-o", output, "--recipient", recipient, "--field", "crt"}, `invalid field "crt"`},
		"bad recipient":      {[]string{"-o", output, "--recipient", "age1nope"}, "invalid age recipient"},
		"unknown context":    {[]string{"-o", output, "--recipient", recipient, "--context", "dev"}, `unknown context "dev"`},
		"passphrase, no tty": {[]string{"-o", output, "--passphrase"}, "--passphrase needs a terminal"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runRoot(t, append([]string{"config", "encrypt", "--talosconfig", input}, tt.args...)...)
			require.ErrorContains(t, err, tt.want)

			_, statErr := os.Stat(output)
			assert.True(t, os.IsNotExist(statErr), "no output is written")
		})
	}
}

func TestConfigEncryptPassphraseFile(t *testing.T) {
	clearUnlockEnv(t)

	input := plainTalosconfig(t)
	output := filepath.Join(t.TempDir(), "mcp-config")
	pass := filepath.Join(t.TempDir(), "pass")
	require.NoError(t, os.WriteFile(pass, []byte("correct horse\n"), 0o600))

	_, _, err := runRoot(t, "config", "encrypt", "--talosconfig", input, "-o", output, "--context", "staging", "--talosconfig-passphrase-file", pass)
	require.NoError(t, err, "the passphrase file implies --passphrase")

	stdout, _, err := runRoot(t, "config", "check", "--talosconfig", output, "--talosconfig-passphrase-file", pass)
	require.NoError(t, err)
	assert.Regexp(t, `staging +operator +key +ok\n`, stdout)
	assert.Regexp(t, `prod +reader +- +ok\n`, stdout)

	// With recipients, the passphrase file only unlocks: it does not turn
	// on passphrase encryption.
	_, recipient := ageKey(t)
	withRecipient := filepath.Join(t.TempDir(), "with-recipient")

	_, _, err = runRoot(t, "config", "encrypt", "--talosconfig", input, "-o", withRecipient, "--recipient", recipient, "--talosconfig-passphrase-file", pass)
	require.NoError(t, err)
}

func TestConfigCheckFailure(t *testing.T) {
	clearUnlockEnv(t)

	input := plainTalosconfig(t)
	_, recipient := ageKey(t)
	other, _ := ageKey(t)
	output := filepath.Join(t.TempDir(), "mcp-config")

	_, _, err := runRoot(t, "config", "encrypt", "--talosconfig", input, "-o", output, "--recipient", recipient, "--context", "prod")
	require.NoError(t, err)

	stdout, _, err := runRoot(t, "config", "check", "--talosconfig", output, "--talosconfig-identity", other)
	require.EqualError(t, err, "contexts do not decrypt: prod")
	assert.Regexp(t, `prod +reader +key,cluster_secret +error: key: no identity matches the recipients\n`, stdout,
		"the role is read from crt without decrypting anything")
	assert.Regexp(t, `staging +operator +- +ok\n`, stdout)

	_, _, err = runRoot(t, "config", "check", "--talosconfig", output)
	require.ErrorIs(t, err, config.ErrNoUnlockSource)

	_, _, err = runRoot(t, "config", "decrypt", "--talosconfig", output)
	require.ErrorIs(t, err, config.ErrNoUnlockSource)

	_, _, err = runRoot(t, "config", "decrypt", "--talosconfig", output, "--talosconfig-identity", other)
	require.ErrorContains(t, err, `context "prod": key: no identity matches the recipients`)
}

// TestUnlockFromEnv covers the environment variables, and that the server
// modes never get the terminal prompt.
func TestUnlockFromEnv(t *testing.T) {
	clearUnlockEnv(t)
	t.Setenv(envIdentity, "a.key, ~/b.key,")
	t.Setenv(envPassphrase, "env-secret")

	f := DefaultFlags()

	_, ok := os.LookupEnv(envPassphrase)
	assert.False(t, ok, "TALOSCONFIG_PASSPHRASE is removed once read")

	opts := f.unlockOptions()
	assert.Equal(t, []string{"a.key", "~/b.key"}, opts.IdentityFiles)
	assert.Equal(t, "env-secret", string(opts.Passphrase))
	assert.Nil(t, opts.Prompt, "mcp and server mode never prompt")

	f = DefaultFlags()
	f.prompt = true
	assert.Nil(t, f.unlockOptions().Prompt, "no prompt when there is no terminal")

	setTerminal(t, true)

	f = DefaultFlags()
	assert.Nil(t, f.unlockOptions().Prompt, "mcp and server mode never prompt, even at a terminal")

	f.prompt = true
	assert.NotNil(t, f.unlockOptions().Prompt, "tools and config prompt at a terminal")

	f.PassphraseFile = "pass"
	assert.Nil(t, f.unlockOptions().Prompt, "a passphrase source replaces the prompt")
}

// TestConfigIgnoresContextEnv checks that TALOS_CONTEXT, set for the server,
// does not leave contexts out of config encrypt.
func TestConfigIgnoresContextEnv(t *testing.T) {
	clearUnlockEnv(t)
	t.Setenv(envContext, "prod")

	input := plainTalosconfig(t)
	identity, recipient := ageKey(t)
	output := filepath.Join(t.TempDir(), "mcp-config")

	_, stderr, err := runRoot(t, "config", "encrypt", "--talosconfig", input, "-o", output, "--recipient", recipient)
	require.NoError(t, err)
	assert.Contains(t, stderr, "encrypted 3 fields")

	for name, c := range loadContexts(t, output) {
		assert.True(t, secrets.IsEncrypted(c.Key), name)
	}

	stdout, _, err := runRoot(t, "config", "check", "--talosconfig", output, "--talosconfig-identity", identity)
	require.NoError(t, err)
	assert.Contains(t, stdout, "staging")

	stdout, _, err = runRoot(t, "config", "check", "--talosconfig", output, "--talosconfig-identity", identity, "--context", "prod")
	require.NoError(t, err)
	assert.NotContains(t, stdout, "staging", "an explicit --context still selects one")
}

func TestConfigCheckEmptyContext(t *testing.T) {
	clearUnlockEnv(t)

	path := filepath.Join(t.TempDir(), "talosconfig")
	require.NoError(t, os.WriteFile(path, []byte("context: old\ncontexts:\n  old:\n"), 0o600))

	stdout, _, err := runRoot(t, "config", "check", "--talosconfig", path)
	require.NoError(t, err)
	assert.Regexp(t, `old +- +- +ok\n`, stdout)
}

func TestMCPWithEncryptedTalosconfig(t *testing.T) {
	clearUnlockEnv(t)

	input := plainTalosconfig(t)
	identity, recipient := ageKey(t)
	output := filepath.Join(t.TempDir(), "mcp-config")

	_, _, err := runRoot(t, "config", "encrypt", "--talosconfig", input, "-o", output, "--recipient", recipient)
	require.NoError(t, err)

	f := DefaultFlags()
	f.TalosConfig = output

	cfg, err := f.Config()
	require.NoError(t, err)

	_, _, err = newMCPServer(cfg, slog.New(slog.DiscardHandler))
	require.ErrorIs(t, err, config.ErrNoUnlockSource, "mcp mode has no prompt to fall back to")

	f.Identity = []string{identity}

	cfg, err = f.Config()
	require.NoError(t, err)

	srv, pool, err := newMCPServer(cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NotNil(t, srv)

	defer pool.Close() //nolint:errcheck

	assert.Equal(t, []string{"prod", "staging"}, pool.List())
	assert.Empty(t, pool.PlaintextKeys())
}

func TestServerRequireAllContexts(t *testing.T) {
	clearUnlockEnv(t)

	input := plainTalosconfig(t)
	identity, _ := ageKey(t)
	_, other := ageKey(t)
	output := filepath.Join(t.TempDir(), "mcp-config")

	_, _, err := runRoot(t, "config", "encrypt", "--talosconfig", input, "-o", output, "--recipient", other, "--context", "staging")
	require.NoError(t, err)

	f := DefaultFlags()
	f.TalosConfig = output
	f.Identity = []string{identity}
	f.RequireAllContexts = true
	f.LogLevel = "error"

	err = runServer(t.Context(), f)
	require.EqualError(t, err, "--require-all-contexts: talosconfig contexts skipped: staging")
	assert.False(t, strings.Contains(err.Error(), "BEGIN"))
}
