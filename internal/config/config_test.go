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
	"path/filepath"
	"testing"

	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveTalosConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, tt := range []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty uses talosctl default",
			input:    "",
			expected: filepath.Join(home, ".talos", "config"),
		},
		{
			name:     "explicit path",
			input:    "/etc/talos/config",
			expected: "/etc/talos/config",
		},
		{
			name:     "tilde is expanded",
			input:    "~/clusters/mcp-config",
			expected: filepath.Join(home, "clusters", "mcp-config"),
		},
		{
			name:     "path is cleaned",
			input:    "/etc/talos/../talos/config",
			expected: "/etc/talos/config",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.ResolveTalosConfigPath(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestParseExtensions(t *testing.T) {
	for _, tt := range []struct {
		name          string
		input         string
		expected      map[string]bool
		expectedError bool
	}{
		{
			name:     "all",
			input:    "all",
			expected: map[string]bool{"cluster": true, "node": true},
		},
		{
			name:     "single group",
			input:    "cluster",
			expected: map[string]bool{"cluster": true},
		},
		{
			name:     "list with spaces",
			input:    "cluster, node",
			expected: map[string]bool{"cluster": true, "node": true},
		},
		{
			name:          "unknown group",
			input:         "cluster,etcd",
			expectedError: true,
		},
		{
			name:          "empty",
			input:         " , ",
			expectedError: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.ParseExtensions(tt.input)
			if tt.expectedError {
				require.ErrorIs(t, err, config.ErrInvalidExtension)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}
