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
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// scryptStanza is the age stanza type of passphrase encryption.
const scryptStanza = "scrypt"

// ErrNotEncrypted is returned by Decrypt for a plaintext value.
var ErrNotEncrypted = errors.New("value is not encrypted")

// Decrypt decrypts an encrypted talosconfig value (see IsEncrypted). The
// identities of u are tried first, then its passphrase, which is read only
// when the value is encrypted with one. Errors never include the ciphertext
// or any plaintext.
func Decrypt(value string, u *Unlocker) (_ []byte, err error) {
	defer recoverCrypto(&err)

	if !IsEncrypted(value) {
		return nil, ErrNotEncrypted
	}

	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, ErrNotEncrypted
	}

	ids, err := u.identities()
	if err != nil {
		return nil, err
	}

	ids = append(slices.Clone(ids), &passphraseIdentity{u: u})

	r, err := age.Decrypt(armor.NewReader(bytes.NewReader(data)), ids...)
	if err != nil {
		return nil, decryptError(err, u)
	}

	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, errors.New("encrypted value is corrupted")
	}

	return plain, nil
}

// decryptError turns an age error into a short message. age errors do not
// carry key material, but the messages are written for a person who set the
// unlock flags.
func decryptError(err error, u *Unlocker) error {
	var noMatch *age.NoIdentityMatchError
	if !errors.As(err, &noMatch) {
		return fmt.Errorf("decrypting: %w", err)
	}

	if slices.Contains(noMatch.StanzaTypes, scryptStanza) {
		if !u.hasPassphrase() {
			return errors.New("encrypted with a passphrase, but no passphrase source is set")
		}

		return errors.New("wrong passphrase")
	}

	if len(u.ids) == 0 {
		return errors.New("encrypted to recipients, but no identity is set")
	}

	return errors.New("no identity matches the recipients")
}

// passphraseIdentity unwraps a passphrase-encrypted file. It asks u for the
// passphrase only when the file has a scrypt stanza, so a value encrypted to
// an identity never triggers a prompt.
type passphraseIdentity struct {
	u *Unlocker
}

func (p *passphraseIdentity) Unwrap(stanzas []*age.Stanza) ([]byte, error) {
	if !slices.ContainsFunc(stanzas, func(s *age.Stanza) bool { return s.Type == scryptStanza }) {
		return nil, fmt.Errorf("%w: not passphrase-encrypted", age.ErrIncorrectIdentity)
	}

	if !p.u.hasPassphrase() {
		return nil, fmt.Errorf("%w: no passphrase source", age.ErrIncorrectIdentity)
	}

	passphrase, err := p.u.Passphrase(talosconfigPrompt)
	if err != nil {
		return nil, err
	}

	id, err := age.NewScryptIdentity(string(passphrase))
	if err != nil {
		return nil, err
	}

	return id.Unwrap(stanzas)
}
