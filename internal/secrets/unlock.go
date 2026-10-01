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

package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"golang.org/x/crypto/ssh"
)

// askpassTimeout bounds how long an askpass program may run.
const askpassTimeout = 60 * time.Second

// talosconfigPrompt asks for the passphrase of the talosconfig values.
const talosconfigPrompt = "Enter passphrase for talosconfig: "

// ErrNoPassphrase is returned by Unlocker.Passphrase when no passphrase
// source is set.
var ErrNoPassphrase = errors.New("no passphrase source is set")

// Options are the unlock sources (docs/secrets.md §4).
type Options struct {
	// IdentityFiles are age identity files or OpenSSH private keys.
	IdentityFiles []string
	// PassphraseFile holds the passphrase on its first line.
	PassphraseFile string
	// Askpass is a program that prints the passphrase on stdout. It gets the
	// prompt as its only argument, like SSH_ASKPASS.
	Askpass string
	// Passphrase is the value of TALOSCONFIG_PASSPHRASE, see TakeEnv.
	Passphrase []byte
	// Prompt reads a passphrase from the terminal. It is nil where a prompt
	// is not allowed, such as in mcp and server mode, and is used only when
	// no other passphrase source is set.
	Prompt func(prompt string) ([]byte, error)
}

// Configured reports whether any unlock source is set.
func (o *Options) Configured() bool {
	return len(o.IdentityFiles) > 0 || o.PassphraseFile != "" || o.Askpass != "" || o.Passphrase != nil || o.Prompt != nil
}

// TakeEnv returns the value of the environment variable key, or nil when it
// is unset or empty, and removes it from the process environment so that
// child processes do not inherit it.
func TakeEnv(key string) []byte {
	val, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}

	os.Unsetenv(key) //nolint:errcheck

	if val == "" {
		return nil
	}

	return []byte(val)
}

// Unlocker holds the unlock material for one load of the talosconfig. It
// reads each source once, when a value first needs it, and Close drops it.
// It is not safe for concurrent use.
type Unlocker struct {
	opts Options

	loaded bool
	ids    []age.Identity
	// keys are identity file contents still referenced by an encrypted SSH
	// identity, zeroed by Close.
	keys [][]byte
	// passphrases caches each passphrase by prompt.
	passphrases map[string][]byte
}

// NewUnlocker returns an Unlocker for opts. It reads nothing until a value
// needs it.
func NewUnlocker(opts Options) *Unlocker {
	// Copy what Close zeroes or drops, so the caller's options stay usable
	// for a later load.
	opts.IdentityFiles = slices.Clone(opts.IdentityFiles)
	opts.Passphrase = bytes.Clone(opts.Passphrase)

	return &Unlocker{opts: opts, passphrases: map[string][]byte{}}
}

// Configured reports whether any unlock source is set.
func (u *Unlocker) Configured() bool {
	return u.opts.Configured()
}

// Load reads the identity files. Decrypt calls it as needed; calling it
// first reports an unreadable identity file before any value is decrypted.
func (u *Unlocker) Load() error {
	_, err := u.identities()

	return err
}

// Passphrase returns the passphrase for prompt from the first source set:
// the passphrase file, the askpass program, TALOSCONFIG_PASSPHRASE, then the
// terminal prompt. The result is cached until Close.
func (u *Unlocker) Passphrase(prompt string) ([]byte, error) {
	if p, ok := u.passphrases[prompt]; ok {
		return p, nil
	}

	var (
		p   []byte
		err error
	)

	switch {
	case u.opts.PassphraseFile != "":
		p, err = readPassphraseFile(u.opts.PassphraseFile)
	case u.opts.Askpass != "":
		p, err = runAskpass(u.opts.Askpass, prompt)
	case u.opts.Passphrase != nil:
		p = bytes.Clone(u.opts.Passphrase)
	case u.opts.Prompt != nil:
		p, err = u.opts.Prompt(prompt)
	default:
		return nil, ErrNoPassphrase
	}

	if err != nil {
		return nil, err
	}

	if len(p) == 0 {
		return nil, errors.New("passphrase is empty")
	}

	u.passphrases[prompt] = p

	return p, nil
}

// Close zeroes the passphrases and identity file contents it holds and drops
// the identities. Only the decrypted values stay in memory.
func (u *Unlocker) Close() {
	for _, p := range u.passphrases {
		clear(p)
	}

	for _, k := range u.keys {
		clear(k)
	}

	clear(u.opts.Passphrase)

	u.passphrases = map[string][]byte{}
	u.ids, u.keys = nil, nil
	u.opts = Options{}
	u.loaded = true
}

func (u *Unlocker) hasPassphrase() bool {
	return u.opts.PassphraseFile != "" || u.opts.Askpass != "" || u.opts.Passphrase != nil || u.opts.Prompt != nil
}

func (u *Unlocker) identities() ([]age.Identity, error) {
	if u.loaded {
		return u.ids, nil
	}

	for _, path := range u.opts.IdentityFiles {
		ids, err := u.readIdentity(path)
		if err != nil {
			return nil, fmt.Errorf("identity %s: %w", path, err)
		}

		u.ids = append(u.ids, ids...)
	}

	u.loaded = true

	return u.ids, nil
}

// readIdentity parses an OpenSSH private key or an age identity file. An
// encrypted SSH key asks for its passphrase only when a value is encrypted
// to it.
func (u *Unlocker) readIdentity(path string) ([]age.Identity, error) {
	path = expandHome(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if !bytes.Contains(data, []byte("-----BEGIN")) {
		defer clear(data)

		return age.ParseIdentities(bytes.NewReader(data))
	}

	id, err := agessh.ParseIdentity(data)
	if err == nil {
		clear(data)

		return []age.Identity{id}, nil
	}

	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) {
		clear(data)

		return nil, err
	}

	pub := missing.PublicKey
	if pub == nil {
		// Older PEM keys do not embed the public key: read it from the .pub file.
		pub, err = readPublicKey(path + ".pub")
		if err != nil {
			clear(data)

			return nil, fmt.Errorf("encrypted key without a public key: %w", err)
		}
	}

	prompt := "Enter passphrase for " + path + ": "

	enc, err := agessh.NewEncryptedSSHIdentity(pub, data, func() ([]byte, error) {
		return u.Passphrase(prompt)
	})
	if err != nil {
		clear(data)

		return nil, err
	}

	u.keys = append(u.keys, data)

	return []age.Identity{enc}, nil
}

func readPublicKey(path string) (ssh.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	pub, _, _, _, err := ssh.ParseAuthorizedKey(data) //nolint:dogsled
	if err != nil {
		return nil, err
	}

	return pub, nil
}

// readPassphraseFile returns the first line of path, without the newline.
func readPassphraseFile(path string) ([]byte, error) {
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil, fmt.Errorf("reading passphrase file: %w", err)
	}

	line, _, _ := bytes.Cut(data, []byte("\n"))
	p := bytes.Clone(bytes.TrimSuffix(line, []byte("\r")))

	clear(data)

	return p, nil
}

// runAskpass runs program with prompt as its argument and no stdin, and
// returns the first line of its output.
func runAskpass(program, prompt string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), askpassTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, program, prompt)

	out, err := cmd.Output()
	if err != nil {
		clear(out)

		if ctx.Err() != nil {
			return nil, fmt.Errorf("askpass %s: timed out after %s", program, askpassTimeout)
		}

		return nil, fmt.Errorf("askpass %s: %w", program, err)
	}

	line, _, _ := bytes.Cut(out, []byte("\n"))
	p := bytes.Clone(bytes.TrimSuffix(line, []byte("\r")))

	clear(out)

	return p, nil
}

// expandHome expands a leading "~/", which a path from an environment
// variable or an MCP client config keeps unexpanded.
func expandHome(path string) string {
	rest, ok := strings.CutPrefix(path, "~/")
	if !ok {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	return filepath.Join(home, rest)
}
