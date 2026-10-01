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
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"filippo.io/age/armor"
)

// scryptWorkFactor is the scrypt work factor of passphrase encryption: 2^18
// is about one second on a modern machine. Tests lower it.
var scryptWorkFactor = 18

// Encrypt encrypts plain to recipients and returns the armored age file in
// base64, ready to be stored as a talosconfig value.
func Encrypt(plain []byte, recipients ...age.Recipient) (_ string, err error) {
	defer recoverCrypto(&err)

	var buf bytes.Buffer

	aw := armor.NewWriter(&buf)

	w, err := age.Encrypt(aw, recipients...)
	if err != nil {
		return "", err
	}

	if _, err := w.Write(plain); err != nil {
		return "", err
	}

	if err := w.Close(); err != nil {
		return "", err
	}

	if err := aw.Close(); err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// recoverCrypto turns a panic in the crypto code into an error. Microsoft Go
// runs crypto on OpenSSL, which panics on the empty HKDF secret that age uses
// for ssh-ed25519 keys; build with MS_GO_NOSYSTEMCRYPTO=1 to avoid it. The
// panic value is an OpenSSL error and holds no key material.
func recoverCrypto(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("crypto backend failed (with Microsoft Go, build with MS_GO_NOSYSTEMCRYPTO=1): %v", r)
	}
}

// PassphraseRecipient returns the scrypt recipient for passphrase. age
// requires it to be the only recipient of a file.
func PassphraseRecipient(passphrase []byte) (age.Recipient, error) {
	r, err := age.NewScryptRecipient(string(passphrase))
	if err != nil {
		return nil, err
	}

	r.SetWorkFactor(scryptWorkFactor)

	return r, nil
}

// ParseRecipient parses an age public key (age1...) or an SSH public key
// (ssh-ed25519 or ssh-rsa, as in an authorized_keys or .pub file).
func ParseRecipient(s string) (age.Recipient, error) {
	s = strings.TrimSpace(s)

	if strings.HasPrefix(s, "ssh-") {
		r, err := agessh.ParseRecipient(s)
		if err != nil {
			return nil, fmt.Errorf("invalid SSH recipient: %w", err)
		}

		return r, nil
	}

	rs, err := age.ParseRecipients(strings.NewReader(s))
	if err != nil {
		return nil, fmt.Errorf("invalid age recipient %q: %w", s, err)
	}

	return rs[0], nil
}

// ReadRecipientsFile reads one recipient per line from path, skipping empty
// lines and # comments. It accepts .pub files and age recipient files.
func ReadRecipientsFile(path string) ([]age.Recipient, error) {
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil, fmt.Errorf("reading recipients: %w", err)
	}

	var recipients []age.Recipient

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		r, err := ParseRecipient(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}

		recipients = append(recipients, r)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading recipients: %w", err)
	}

	if len(recipients) == 0 {
		return nil, fmt.Errorf("%s: no recipients found", path)
	}

	return recipients, nil
}
