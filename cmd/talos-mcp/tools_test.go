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
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseArguments(t *testing.T) {
	arguments, err := parseArguments([]string{
		"tail=50",
		"offset=-2",
		"node=10.0.0.21",
		"empty=",
		"version=1.2",
		"filter=a=b",
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"tail":    50,
		"offset":  -2,
		"node":    "10.0.0.21",
		"empty":   "",
		"version": "1.2",
		"filter":  "a=b",
	}, arguments)
}

func TestParseArgumentsInvalid(t *testing.T) {
	_, err := parseArguments([]string{"kubelet"})
	require.EqualError(t, err, `argument "kubelet" must be in key=value format`)

	_, err = parseArguments([]string{"=x"})
	require.EqualError(t, err, `argument "=x" has empty key`)
}

func TestToolsLogLevel(t *testing.T) {
	for _, tt := range []struct {
		name     string
		env      string
		args     []string
		expected string
	}{
		{name: "default is warn", expected: toolsLogLevel},
		{name: "flag wins", args: []string{"--log-level", "debug"}, expected: "debug"},
		{name: "env wins", env: "info", expected: "info"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envLogLevel, tt.env)

			flags := DefaultFlags()
			root := &cobra.Command{Use: bin, SilenceUsage: true, SilenceErrors: true}
			flags.AddPersistentFlags(root.PersistentFlags())
			root.AddCommand(newToolsCmd(flags))

			// A missing talosconfig stops the command right after the log
			// level is chosen, before any output.
			root.SetArgs(append([]string{"tools", "--talosconfig", filepath.Join(t.TempDir(), "missing")}, tt.args...))
			require.Error(t, root.ExecuteContext(t.Context()))

			assert.Equal(t, tt.expected, flags.LogLevel)
		})
	}
}
