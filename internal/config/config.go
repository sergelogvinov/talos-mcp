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

// Package config defines the configuration of the talos-mcp server and
// loads and validates the talosconfig file.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the configuration for the talos-mcp server.
type Config struct {
	TalosConfig      string // resolved talosconfig path
	Context          string // optional: restrict to one context
	Port             int
	Listen           string // server listen address
	NoHostCheck      bool   // server: disable DNS rebinding protection
	Extensions       string
	AllowDestructive bool
	LogLevel         string
	LogFormat        string
}

// Extension groups selectable with --extensions.
const (
	ExtensionAll     = "all"
	ExtensionCluster = "cluster"
	ExtensionNode    = "node"
)

// ErrInvalidExtension is returned for an unknown --extensions value.
var ErrInvalidExtension = errors.New("invalid extension")

// defaultTalosConfig is the talosctl default path, relative to $HOME.
const defaultTalosConfig = ".talos/config"

// ResolveTalosConfigPath returns the talosconfig path to use. path is the
// --talosconfig flag value, which already falls back to $TALOSCONFIG. An
// empty path means $HOME/.talos/config. A leading "~/" is expanded.
func ResolveTalosConfigPath(path string) (string, error) {
	if path == "" {
		path = "~/" + defaultTalosConfig
	}

	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving talosconfig path: %w", err)
		}

		path = filepath.Join(home, rest)
	}

	return filepath.Clean(path), nil
}

// ParseExtensions splits a comma-separated --extensions value and validates
// each entry. "all" expands to every group.
func ParseExtensions(value string) (map[string]bool, error) {
	result := map[string]bool{}

	for item := range strings.SplitSeq(value, ",") {
		item = strings.TrimSpace(item)

		switch item {
		case "":
			continue
		case ExtensionAll:
			result[ExtensionCluster] = true
			result[ExtensionNode] = true
		case ExtensionCluster, ExtensionNode:
			result[item] = true
		default:
			return nil, fmt.Errorf("%w %q: must be one of all, cluster, node", ErrInvalidExtension, item)
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("%w: no extensions selected", ErrInvalidExtension)
	}

	return result, nil
}
