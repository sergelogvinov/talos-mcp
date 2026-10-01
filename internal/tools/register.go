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

// Package tools implements the MCP tools of the talos-mcp server.
package tools

import (
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
)

// TalosTools provides tool handlers with access to the Talos client pool.
type TalosTools struct {
	pool             *talos.Pool
	allowDestructive bool
	extensions       map[string]bool

	// registered lists the tools added by RegisterTools, so
	// talos_clusters_list can report which of them each cluster can use.
	registered []toolSpec
}

// toolSpec is what a cluster needs for a tool to be usable on it (design §2.3).
type toolSpec struct {
	name          string
	minRole       talos.Role
	needDiscovery bool
}

// NewTalosTools creates tool handlers backed by the given pool. extensions
// is the parsed --extensions value (config.ParseExtensions).
func NewTalosTools(pool *talos.Pool, allowDestructive bool, extensions map[string]bool) *TalosTools {
	return &TalosTools{
		pool:             pool,
		allowDestructive: allowDestructive,
		extensions:       extensions,
	}
}

// RegisterTools registers the tools enabled by --extensions, --allow-destructive
// and the credential roles on the MCP server (design §7, §9).
func (t *TalosTools) RegisterTools(srv *mcp.Server) {
	if t.enabled(config.ExtensionCluster) {
		t.RegisterClustersList(srv)
	}
}

// enabled reports whether a tool group is selected with --extensions.
func (t *TalosTools) enabled(group string) bool {
	return t.extensions[group]
}

// track records a registered tool for talos_clusters_list.
func (t *TalosTools) track(spec toolSpec) {
	t.registered = append(t.registered, spec)
}

// toolsFor returns the registered tools usable on a cluster, sorted.
func (t *TalosTools) toolsFor(cluster string) []string {
	role := t.pool.Role(cluster)
	hasDiscovery := t.pool.Discovery(cluster) != nil

	names := []string{}

	for _, spec := range t.registered {
		if !role.Allows(spec.minRole) || (spec.needDiscovery && !hasDiscovery) {
			continue
		}

		names = append(names, spec.name)
	}

	slices.Sort(names)

	return names
}
