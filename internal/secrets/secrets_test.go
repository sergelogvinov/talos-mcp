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

package secrets_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/sergelogvinov/talos-mcp/internal/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

const plaintext = "-----BEGIN EC PRIVATE KEY-----\nsecret material\n-----END EC PRIVATE KEY-----\n"

func init() {
	*secrets.ScryptWorkFactor = 10
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

// ageIdentity returns an age identity file and its recipient.
func ageIdentity(t *testing.T) (string, age.Recipient) {
	t.Helper()

	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	return writeFile(t, "age.key", "# test\n"+id.String()+"\n"), id.Recipient()
}

// sshKey returns an OpenSSH private key file, encrypted when passphrase is
// set, and its public key in authorized_keys form.
func sshKey(t *testing.T, kind, passphrase string) (string, string) {
	t.Helper()

	var priv any

	switch kind {
	case "ed25519":
		_, k, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)

		priv = k
	case "rsa":
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)

		priv = k
	}

	var (
		block *pem.Block
		err   error
	)

	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}

	require.NoError(t, err)

	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)

	return writeFile(t, "id_"+kind, string(pem.EncodeToMemory(block))), string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
}

func encrypt(t *testing.T, recipients ...age.Recipient) string {
	t.Helper()

	value, err := secrets.Encrypt([]byte(plaintext), recipients...)
	require.NoError(t, err)
	require.True(t, secrets.IsEncrypted(value))

	return value
}

func passphraseRecipient(t *testing.T, passphrase string) age.Recipient {
	t.Helper()

	r, err := secrets.PassphraseRecipient([]byte(passphrase))
	require.NoError(t, err)

	return r
}

func TestIsEncrypted(t *testing.T) {
	random := make([]byte, 32)
	_, err := rand.Read(random)
	require.NoError(t, err)

	_, recipient := ageIdentity(t)

	for name, tt := range map[string]struct {
		value string
		want  bool
	}{
		"plaintext pem":  {base64.StdEncoding.EncodeToString([]byte(plaintext)), false},
		"random base64":  {base64.StdEncoding.EncodeToString(random), false},
		"invalid base64": {"not base64!", false},
		"empty":          {"", false},
		"raw armor":      {"-----BEGIN AGE ENCRYPTED FILE-----", false},
		"encrypted":      {encrypt(t, recipient), true},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, secrets.IsEncrypted(tt.value))
		})
	}
}

func TestRoundTrip(t *testing.T) {
	ageFile, ageRecipient := ageIdentity(t)
	edFile, edPub := sshKey(t, "ed25519", "")
	rsaFile, rsaPub := sshKey(t, "rsa", "")

	edRecipient, err := secrets.ParseRecipient(edPub)
	require.NoError(t, err)

	rsaRecipient, err := secrets.ParseRecipient(rsaPub)
	require.NoError(t, err)

	multi := encrypt(t, ageRecipient, edRecipient, rsaRecipient)

	for name, tt := range map[string]struct {
		value string
		opts  secrets.Options
	}{
		"passphrase":     {encrypt(t, passphraseRecipient(t, "hunter2")), secrets.Options{PassphraseFile: writeFile(t, "pass", "hunter2\nignored\n")}},
		"age identity":   {encrypt(t, ageRecipient), secrets.Options{IdentityFiles: []string{ageFile}}},
		"ssh-ed25519":    {encrypt(t, edRecipient), secrets.Options{IdentityFiles: []string{edFile}}},
		"ssh-rsa":        {encrypt(t, rsaRecipient), secrets.Options{IdentityFiles: []string{rsaFile}}},
		"multi, age":     {multi, secrets.Options{IdentityFiles: []string{ageFile}}},
		"multi, ed25519": {multi, secrets.Options{IdentityFiles: []string{edFile}}},
		"multi, rsa":     {multi, secrets.Options{IdentityFiles: []string{rsaFile}}},
	} {
		t.Run(name, func(t *testing.T) {
			u := secrets.NewUnlocker(tt.opts)
			defer u.Close()

			plain, err := secrets.Decrypt(tt.value, u)
			require.NoError(t, err)
			assert.Equal(t, plaintext, string(plain))
		})
	}
}

func TestDecryptErrors(t *testing.T) {
	_, recipient := ageIdentity(t)
	otherFile, _ := ageIdentity(t)
	byPassphrase := encrypt(t, passphraseRecipient(t, "right"))

	for name, tt := range map[string]struct {
		value string
		opts  secrets.Options
		want  string
	}{
		"wrong passphrase":   {byPassphrase, secrets.Options{Passphrase: []byte("wrong")}, "wrong passphrase"},
		"no passphrase":      {byPassphrase, secrets.Options{IdentityFiles: []string{otherFile}}, "encrypted with a passphrase, but no passphrase source is set"},
		"wrong identity":     {encrypt(t, recipient), secrets.Options{IdentityFiles: []string{otherFile}}, "no identity matches the recipients"},
		"no identity":        {encrypt(t, recipient), secrets.Options{Passphrase: []byte("x")}, "encrypted to recipients, but no identity is set"},
		"plaintext":          {base64.StdEncoding.EncodeToString([]byte(plaintext)), secrets.Options{}, "value is not encrypted"},
		"missing identity":   {encrypt(t, recipient), secrets.Options{IdentityFiles: []string{"/nonexistent/key"}}, "identity /nonexistent/key"},
		"empty passphrase":   {byPassphrase, secrets.Options{PassphraseFile: writeFile(t, "pass", "\n")}, "passphrase is empty"},
		"missing passphrase": {byPassphrase, secrets.Options{PassphraseFile: "/nonexistent/pass"}, "reading passphrase file"},
	} {
		t.Run(name, func(t *testing.T) {
			u := secrets.NewUnlocker(tt.opts)
			defer u.Close()

			plain, err := secrets.Decrypt(tt.value, u)
			require.Error(t, err)
			assert.Nil(t, plain)
			assert.Contains(t, err.Error(), tt.want)
			assert.NotContains(t, err.Error(), "secret material")
			assert.NotContains(t, err.Error(), tt.value[:20])
		})
	}
}

// TestEncryptedSSHIdentityIsLazy checks that the passphrase of an encrypted
// SSH key is asked for only when a value is encrypted to that key.
func TestEncryptedSSHIdentityIsLazy(t *testing.T) {
	keyFile, pub := sshKey(t, "ed25519", "ssh-pass")
	ageFile, ageRecipient := ageIdentity(t)

	sshRecipient, err := secrets.ParseRecipient(pub)
	require.NoError(t, err)

	var prompts []string

	u := secrets.NewUnlocker(secrets.Options{
		IdentityFiles: []string{keyFile, ageFile},
		Prompt: func(prompt string) ([]byte, error) {
			prompts = append(prompts, prompt)

			return []byte("ssh-pass"), nil
		},
	})
	defer u.Close()

	require.NoError(t, u.Load())
	assert.Empty(t, prompts, "loading the key must not ask for its passphrase")

	plain, err := secrets.Decrypt(encrypt(t, ageRecipient), u)
	require.NoError(t, err)
	assert.Equal(t, plaintext, string(plain))
	assert.Empty(t, prompts, "a value encrypted to the age identity must not ask")

	for range 2 {
		plain, err = secrets.Decrypt(encrypt(t, sshRecipient), u)
		require.NoError(t, err)
		assert.Equal(t, plaintext, string(plain))
	}

	assert.Equal(t, []string{"Enter passphrase for " + keyFile + ": "}, prompts, "asked once, then cached")
}

func TestPassphraseSources(t *testing.T) {
	askpass := writeFile(t, "askpass", "#!/bin/sh\necho \"from-askpass:$1\"\necho second line\n")
	require.NoError(t, os.Chmod(askpass, 0o700)) //nolint:gosec // the test runs it

	prompt := func(string) ([]byte, error) { return []byte("from-prompt"), nil }

	for name, tt := range map[string]struct {
		opts secrets.Options
		want string
	}{
		"file":         {secrets.Options{PassphraseFile: writeFile(t, "pass", "from-file\r\n")}, "from-file"},
		"askpass":      {secrets.Options{Askpass: askpass}, "from-askpass:Prompt: "},
		"env":          {secrets.Options{Passphrase: []byte("from-env")}, "from-env"},
		"prompt":       {secrets.Options{Prompt: prompt}, "from-prompt"},
		"file wins":    {secrets.Options{PassphraseFile: writeFile(t, "pass", "from-file"), Askpass: askpass, Passphrase: []byte("from-env"), Prompt: prompt}, "from-file"},
		"askpass wins": {secrets.Options{Askpass: askpass, Passphrase: []byte("from-env"), Prompt: prompt}, "from-askpass:Prompt: "},
		"env over tty": {secrets.Options{Passphrase: []byte("from-env"), Prompt: prompt}, "from-env"},
	} {
		t.Run(name, func(t *testing.T) {
			u := secrets.NewUnlocker(tt.opts)
			defer u.Close()

			p, err := u.Passphrase("Prompt: ")
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(p))
		})
	}

	u := secrets.NewUnlocker(secrets.Options{})
	_, err := u.Passphrase("Prompt: ")
	require.ErrorIs(t, err, secrets.ErrNoPassphrase)
	assert.False(t, u.Configured())

	failing := writeFile(t, "fail", "#!/bin/sh\nexit 3\n")
	require.NoError(t, os.Chmod(failing, 0o700)) //nolint:gosec // the test runs it

	_, err = secrets.NewUnlocker(secrets.Options{Askpass: failing}).Passphrase("Prompt: ")
	require.ErrorContains(t, err, "exit status 3")
}

func TestTakeEnv(t *testing.T) {
	t.Setenv("TEST_TALOSCONFIG_PASSPHRASE", "env-secret")

	assert.Equal(t, "env-secret", string(secrets.TakeEnv("TEST_TALOSCONFIG_PASSPHRASE")))

	_, ok := os.LookupEnv("TEST_TALOSCONFIG_PASSPHRASE")
	assert.False(t, ok, "the variable must be removed from the environment")
	assert.Nil(t, secrets.TakeEnv("TEST_TALOSCONFIG_PASSPHRASE"))
}

func TestCloseZeroesPassphrase(t *testing.T) {
	env := []byte("env-secret")
	value := encrypt(t, passphraseRecipient(t, "env-secret"))
	opts := secrets.Options{Passphrase: env}

	u := secrets.NewUnlocker(opts)

	cached, err := u.Passphrase("Enter passphrase for talosconfig: ")
	require.NoError(t, err)

	_, err = secrets.Decrypt(value, u)
	require.NoError(t, err)

	u.Close()

	assert.Equal(t, make([]byte, len(cached)), cached, "the copy it held is zeroed")
	assert.Equal(t, "env-secret", string(env), "the caller's passphrase is left alone")

	_, err = secrets.Decrypt(value, u)
	require.Error(t, err, "nothing is left to decrypt with after Close")

	// The same options unlock again, as a second load in the process would.
	again := secrets.NewUnlocker(opts)
	defer again.Close()

	plain, err := secrets.Decrypt(value, again)
	require.NoError(t, err)
	assert.Equal(t, plaintext, string(plain))
}

func TestReadRecipientsFile(t *testing.T) {
	_, edPub := sshKey(t, "ed25519", "")
	_, ageRecipient := ageIdentity(t)

	path := writeFile(t, "recipients", "# team\n"+edPub+"\n"+ageRecipient.(*age.X25519Recipient).String()+"\n")

	recipients, err := secrets.ReadRecipientsFile(path)
	require.NoError(t, err)
	assert.Len(t, recipients, 2)

	_, err = secrets.ReadRecipientsFile(writeFile(t, "bad", "age1notakey\n"))
	require.ErrorContains(t, err, ":1:")

	_, err = secrets.ReadRecipientsFile(writeFile(t, "empty", "# nothing\n"))
	require.ErrorContains(t, err, "no recipients")

	_, err = secrets.ParseRecipient(strings.TrimSpace(edPub)[:20])
	require.Error(t, err)
}

// panicRecipient fails like age's ssh-ed25519 recipient on an OpenSSL-backed
// Go toolchain.
type panicRecipient struct{}

func (panicRecipient) Wrap([]byte) ([]*age.Stanza, error) {
	panic("EVP_KDF_derive")
}

func TestEncryptRecoversCryptoPanic(t *testing.T) {
	value, err := secrets.Encrypt([]byte(plaintext), panicRecipient{})
	require.ErrorContains(t, err, "crypto backend failed")
	require.ErrorContains(t, err, "MS_GO_NOSYSTEMCRYPTO=1")
	assert.Empty(t, value)
}
