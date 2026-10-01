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
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"filippo.io/age"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/secrets"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/spf13/cobra"
)

// encryptPrompt asks for the passphrase that config encrypt encrypts with.
const encryptPrompt = "Enter passphrase to encrypt talosconfig: "

var secretFields = []string{config.FieldKey, config.FieldClusterSecret}

// newConfigCmd creates the `config` subcommand that encrypts, decrypts and
// checks the secret fields of a talosconfig (docs/secrets.md §6).
func newConfigCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Encrypt, decrypt and check talosconfig secrets",
		Long:  "Encrypt, decrypt and check the key and cluster_secret fields of a talosconfig with age",
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			// These commands may prompt on the terminal for a passphrase.
			flags.prompt = true

			// TALOS_CONTEXT is set for the server. Here it would silently
			// leave the other contexts out, so only --context selects one.
			if !cmd.Flags().Changed(flagContext) {
				flags.Context = ""
			}
		},
	}

	cmd.AddCommand(
		newConfigEncryptCmd(flags),
		newConfigDecryptCmd(flags),
		newConfigCheckCmd(flags),
	)

	return cmd
}

type encryptOptions struct {
	output         string
	passphrase     bool
	recipients     []string
	recipientFiles []string
	fields         []string
}

func newConfigEncryptCmd(flags *Flags) *cobra.Command {
	var o encryptOptions

	cmd := &cobra.Command{
		Use:   "encrypt",
		Short: "Encrypt the secret fields of a talosconfig",
		Long: "Encrypt key and cluster_secret of every context (or of --context; TALOS_CONTEXT is ignored) with a passphrase or to age and SSH recipients. " +
			"Fields that are already encrypted are left as they are. The input file is never overwritten.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConfigEncrypt(cmd.ErrOrStderr(), flags, &o)
		},
	}

	cmd.Flags().StringVarP(&o.output, "output", "o", "", "output file, created with mode 0600 (required)")
	cmd.Flags().BoolVarP(&o.passphrase, "passphrase", "", false, "encrypt with a passphrase, prompted twice on the terminal (implied by --talosconfig-passphrase-file or --talosconfig-askpass)")
	cmd.Flags().StringArrayVarP(&o.recipients, "recipient", "r", nil, "age (age1...) or SSH public key to encrypt to (repeatable)")
	cmd.Flags().StringArrayVarP(&o.recipientFiles, "recipient-file", "R", nil, "file with recipients, one per line, such as ~/.ssh/id_ed25519.pub (repeatable)")
	cmd.Flags().StringSliceVarP(&o.fields, "field", "", nil, "fields to encrypt: key, cluster_secret (default: both)")
	cmd.MarkFlagRequired("output") //nolint:errcheck

	return cmd
}

func runConfigEncrypt(stderr io.Writer, f *Flags, o *encryptOptions) error {
	fields := o.fields
	if len(fields) == 0 {
		fields = secretFields
	}

	for _, field := range fields {
		if !slices.Contains(secretFields, field) {
			return fmt.Errorf("invalid field %q: must be one of %s", field, strings.Join(secretFields, ", "))
		}
	}

	hasRecipients := len(o.recipients) > 0 || len(o.recipientFiles) > 0

	// A passphrase source given without recipients asks for passphrase
	// encryption, so --passphrase is not needed with it.
	if !hasRecipients && (f.PassphraseFile != "" || f.Askpass != "" || f.passphrase != nil) {
		o.passphrase = true
	}

	switch {
	case o.passphrase && hasRecipients:
		return errors.New("--passphrase cannot be combined with --recipient or --recipient-file: age allows only one passphrase and no other recipient")
	case !o.passphrase && !hasRecipients:
		return fmt.Errorf("set --passphrase, --recipient, --recipient-file, --%s or --%s", flagPassphraseFile, flagAskpass)
	}

	path, data, err := readTalosConfig(f)
	if err != nil {
		return err
	}

	if same, err := sameFile(path, o.output); err != nil {
		return err
	} else if same {
		return errors.New("--output must not be the input talosconfig")
	}

	recipients, err := encryptRecipients(f, o)
	if err != nil {
		return err
	}

	encrypted, kept := 0, 0

	out, err := config.EditSecrets(data, f.Context, func(_, field, value string) (string, error) {
		if !slices.Contains(fields, field) {
			return value, nil
		}

		if secrets.IsEncrypted(value) {
			kept++

			return value, nil
		}

		plain, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return "", errors.New("not valid base64")
		}
		defer clear(plain)

		encrypted++

		return secrets.Encrypt(plain, recipients...)
	})
	if err != nil {
		return err
	}

	if err := writePrivateFile(o.output, out); err != nil {
		return err
	}

	fmt.Fprintf(stderr, "encrypted %d fields, %d already encrypted, written to %s\n", encrypted, kept, o.output)

	return nil
}

// encryptRecipients returns the recipients of config encrypt: the passphrase,
// or the --recipient and --recipient-file keys.
func encryptRecipients(f *Flags, o *encryptOptions) ([]age.Recipient, error) {
	if o.passphrase {
		passphrase, err := encryptPassphrase(f)
		if err != nil {
			return nil, err
		}
		defer clear(passphrase)

		r, err := secrets.PassphraseRecipient(passphrase)
		if err != nil {
			return nil, err
		}

		return []age.Recipient{r}, nil
	}

	var recipients []age.Recipient

	for _, s := range o.recipients {
		r, err := secrets.ParseRecipient(s)
		if err != nil {
			return nil, err
		}

		recipients = append(recipients, r)
	}

	for _, path := range o.recipientFiles {
		rs, err := secrets.ReadRecipientsFile(path)
		if err != nil {
			return nil, err
		}

		recipients = append(recipients, rs...)
	}

	return recipients, nil
}

// encryptPassphrase reads the passphrase to encrypt with from the passphrase
// sources, or prompts for it twice on the terminal.
func encryptPassphrase(f *Flags) ([]byte, error) {
	opts := f.unlockOptions()

	if opts.Prompt == nil {
		u := secrets.NewUnlocker(opts)
		defer u.Close()

		p, err := u.Passphrase(encryptPrompt)
		if errors.Is(err, secrets.ErrNoPassphrase) {
			return nil, fmt.Errorf("--passphrase needs a terminal, --%s, --%s or %s", flagPassphraseFile, flagAskpass, envPassphrase)
		}

		return bytes.Clone(p), err
	}

	p, err := opts.Prompt(encryptPrompt)
	if err != nil {
		return nil, err
	}

	again, err := opts.Prompt("Confirm passphrase: ")
	if err != nil {
		clear(p)

		return nil, err
	}
	defer clear(again)

	if !bytes.Equal(p, again) {
		clear(p)

		return nil, errors.New("passphrases do not match")
	}

	if len(p) == 0 {
		return nil, errors.New("passphrase is empty")
	}

	return p, nil
}

func newConfigDecryptCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "decrypt",
		Short: "Print the talosconfig with its secret fields decrypted",
		Long:  "Print the talosconfig with key and cluster_secret decrypted, for example to restore a file talosctl can use. It writes to stdout only.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConfigDecrypt(cmd.OutOrStdout(), flags)
		},
	}
}

func runConfigDecrypt(stdout io.Writer, f *Flags) error {
	_, data, err := readTalosConfig(f)
	if err != nil {
		return err
	}

	u, err := config.NewUnlocker(data, f.Context, f.unlockOptions())
	if err != nil {
		return err
	}
	defer u.Close()

	out, err := config.EditSecrets(data, f.Context, func(_, _, value string) (string, error) {
		if !secrets.IsEncrypted(value) {
			return value, nil
		}

		plain, err := secrets.Decrypt(value, u)
		if err != nil {
			return "", err
		}
		defer clear(plain)

		return base64.StdEncoding.EncodeToString(plain), nil
	})
	if err != nil {
		return err
	}

	_, err = stdout.Write(out)

	return err
}

func newConfigCheckCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Check that every encrypted field decrypts",
		Long:  "Print each context with its role, its encrypted fields and whether they decrypt. It fails when any field does not.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConfigCheck(cmd.OutOrStdout(), flags)
		},
	}
}

// contextCheck is one row of config check.
type contextCheck struct {
	encrypted []string
	err       error
}

func runConfigCheck(stdout io.Writer, f *Flags) error {
	_, data, err := readTalosConfig(f)
	if err != nil {
		return err
	}

	cfg, err := config.ParseClientConfig(data)
	if err != nil {
		return err
	}

	u, err := config.NewUnlocker(data, f.Context, f.unlockOptions())
	if err != nil {
		return err
	}
	defer u.Close()

	checks := map[string]*contextCheck{}

	err = config.VisitSecrets(data, f.Context, func(name, field, value string) error {
		c := checks[name]
		if c == nil {
			c = &contextCheck{}
			checks[name] = c
		}

		if !secrets.IsEncrypted(value) {
			return nil
		}

		c.encrypted = append(c.encrypted, field)

		if c.err == nil {
			plain, err := secrets.Decrypt(value, u)
			if err != nil {
				c.err = fmt.Errorf("%s: %w", field, err)
			}

			clear(plain)
		}

		return nil
	})
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CONTEXT\tROLE\tENCRYPTED\tSTATUS")

	var failed []string

	for _, name := range slices.Sorted(maps.Keys(cfg.Contexts)) {
		if f.Context != "" && name != f.Context {
			continue
		}

		role := "-"

		if c := cfg.Contexts[name]; c != nil {
			if cred, err := talos.ParseCredential(c.Crt); err == nil {
				role = cred.Role.String()
			}
		}

		c := checks[name]
		if c == nil {
			c = &contextCheck{}
		}

		encrypted, status := "-", "ok"
		if len(c.encrypted) > 0 {
			encrypted = strings.Join(c.encrypted, ",")
		}

		if c.err != nil {
			status = "error: " + c.err.Error()
			failed = append(failed, name)
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, role, encrypted, status)
	}

	if err := w.Flush(); err != nil {
		return err
	}

	if len(failed) > 0 {
		return fmt.Errorf("contexts do not decrypt: %s", strings.Join(failed, ", "))
	}

	return nil
}

// readTalosConfig reads the --talosconfig file.
func readTalosConfig(f *Flags) (string, []byte, error) {
	path, err := config.ResolveTalosConfigPath(f.TalosConfig)
	if err != nil {
		return "", nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("reading talosconfig: %w", err)
	}

	return path, data, nil
}

// sameFile reports whether a and b name the same file.
func sameFile(a, b string) (bool, error) {
	absA, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}

	absB, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}

	if absA == absB {
		return true, nil
	}

	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)

	return errA == nil && errB == nil && os.SameFile(infoA, infoB), nil
}

// writePrivateFile writes data to path with mode 0600. It writes a temporary
// file next to path and renames it, so a failed write leaves path as it was.
func writePrivateFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".talos-mcp-*")
	if err != nil {
		return err
	}

	defer os.Remove(tmp.Name()) //nolint:errcheck

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck

		return err
	}

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close() //nolint:errcheck

		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), path)
}
