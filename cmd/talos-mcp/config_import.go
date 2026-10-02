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
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/secrets"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/role"
	"github.com/spf13/cobra"
)

type importOptions struct {
	encryptOptions

	roles       []string
	crtTTL      time.Duration
	endpoints   []string
	nodes       []string
	noDiscovery bool
	force       bool
}

func newConfigImportCmd(flags *Flags) *cobra.Command {
	var o importOptions

	cmd := &cobra.Command{
		Use:   "import MACHINECONFIG",
		Short: "Add a context to a talosconfig from a control plane machine config",
		Long: "Build a context from a control plane machine config (\"-\" reads stdin): the Talos API CA, a new client certificate signed by the CA key, " +
			"the endpoints, and the discovery block with cluster_id and cluster_secret. The context is added to --output, which is created when missing. " +
			"The context is named after the cluster unless --context is set. With --passphrase, --recipient or --recipient-file, " +
			"key and cluster_secret are encrypted before they are written.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigImport(cmd.InOrStdin(), cmd.ErrOrStderr(), flags, args[0], &o)
		},
	}

	cmd.Flags().StringVarP(&o.output, "output", "o", "", "talosconfig to add the context to, created when missing, written with mode 0600 (required)")
	cmd.Flags().StringSliceVarP(&o.roles, "roles", "", []string{string(role.Reader)}, "Talos roles of the client certificate")
	cmd.Flags().DurationVarP(&o.crtTTL, "crt-ttl", "", constants.TalosAPIDefaultCertificateValidityDuration, "lifetime of the client certificate")
	cmd.Flags().StringSliceVarP(&o.endpoints, "endpoints", "e", nil, "Talos API endpoints (default: the host of the cluster endpoint)")
	cmd.Flags().StringSliceVarP(&o.nodes, "nodes", "n", nil, "default nodes of the context")
	cmd.Flags().BoolVarP(&o.noDiscovery, "no-discovery", "", false, "leave out the discovery block")
	cmd.Flags().BoolVarP(&o.force, "force", "f", false, "replace the context when it already exists")
	o.addFlags(cmd.Flags())
	cmd.MarkFlagRequired("output") //nolint:errcheck

	return cmd
}

func runConfigImport(stdin io.Reader, stderr io.Writer, f *Flags, source string, o *importOptions) error {
	encrypt, err := o.resolve(f)
	if err != nil {
		return err
	}

	roles, unknown := role.Parse(o.roles)
	if len(unknown) > 0 {
		return fmt.Errorf("unknown roles: %s", strings.Join(unknown, ", "))
	}

	// Write through a symlinked talosconfig, such as one kept in a dotfiles
	// repo, instead of replacing the link with a regular file.
	if target, err := filepath.EvalSymlinks(o.output); err == nil {
		o.output = target
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("resolving --output: %w", err)
	}

	mc, err := readMachineConfig(stdin, source, o.output)
	if err != nil {
		return err
	}

	existing, err := os.ReadFile(o.output)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading talosconfig: %w", err)
	}

	res, err := config.ImportMachineConfig(mc, config.ImportOptions{
		Roles:       roles,
		TTL:         o.crtTTL,
		Endpoints:   o.endpoints,
		Nodes:       o.nodes,
		NoDiscovery: o.noDiscovery,
		Now:         time.Now(),
	})
	if err != nil {
		return err
	}

	name := f.Context
	if name == "" {
		name = res.ClusterName
	}

	if name == "" {
		return errors.New("the machine config has no cluster name: set --context")
	}

	if encrypt {
		recipients, err := encryptRecipients(f, &o.encryptOptions)
		if err != nil {
			return err
		}

		if err := encryptEntry(res.Context, recipients); err != nil {
			return err
		}
	}

	out, err := config.SetContext(existing, name, res.Context, o.force)
	if errors.Is(err, config.ErrContextExists) {
		return fmt.Errorf("context %q already exists in %s: use --force to replace it", name, o.output)
	} else if err != nil {
		return err
	}

	if err := writePrivateFile(o.output, out); err != nil {
		return err
	}

	fmt.Fprintf(stderr, "imported context %q (%s, certificate valid until %s) to %s\n",
		name, strings.Join(roles.Strings(), ","), time.Now().Add(o.crtTTL).UTC().Format(time.DateOnly), o.output)

	switch {
	case res.Context.Discovery != nil:
		fmt.Fprintf(stderr, "discovery: cluster_id %s, endpoint %s\n", res.Context.Discovery.ClusterID, res.Context.Discovery.Endpoint)
	case res.DiscoveryNote != "":
		fmt.Fprintf(stderr, "discovery: skipped, %s\n", res.DiscoveryNote)
	}

	if encrypt {
		fmt.Fprintln(stderr, "secrets: key and cluster_secret encrypted")
	} else {
		fmt.Fprintln(stderr, "secrets: stored in plaintext; encrypt them with --passphrase, --recipient or --recipient-file")
	}

	return nil
}

// readMachineConfig reads the machine config from a file, or from stdin for
// "-". It must not be the output talosconfig.
func readMachineConfig(stdin io.Reader, source, output string) ([]byte, error) {
	if source == "-" {
		return io.ReadAll(stdin)
	}

	if same, err := sameFile(source, output); err != nil {
		return nil, err
	} else if same {
		return nil, errors.New("--output must not be the machine config")
	}

	data, err := os.ReadFile(source)
	if err != nil {
		return nil, fmt.Errorf("reading machine config: %w", err)
	}

	return data, nil
}

// encryptEntry encrypts the key and the discovery cluster_secret of a context.
func encryptEntry(c *config.ContextEntry, recipients []age.Recipient) error {
	fields := []*string{&c.Key}
	if c.Discovery != nil {
		fields = append(fields, &c.Discovery.ClusterSecret)
	}

	for _, field := range fields {
		plain, err := base64.StdEncoding.DecodeString(*field)
		if err != nil {
			return err
		}

		*field, err = secrets.Encrypt(plain, recipients...)
		clear(plain)

		if err != nil {
			return err
		}
	}

	return nil
}
