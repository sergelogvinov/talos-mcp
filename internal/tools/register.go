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
	"fmt"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/sergelogvinov/talos-mcp/internal/utils"
)

// TalosTools provides tool handlers with access to the Talos client pool.
type TalosTools struct {
	pool             *talos.Pool
	allowDestructive bool
	extensions       map[string]bool

	// sanitizer masks secrets in log and dmesg lines (design §10).
	sanitizer *utils.Sanitizer

	// registered lists the tools added by RegisterTools, so
	// talos_clusters_list can report which of them each cluster can use.
	registered []toolSpec
}

// toolSpec is what a cluster needs for a tool to be usable on it (design §2.3).
type toolSpec struct {
	name          string
	minRole       talos.Role
	needDiscovery bool
	// apis are the apid gRPC methods the tool calls, checked against the
	// Talos role rules in tests (§15.1 item 2).
	apis []string
}

// apid methods called by the tools.
const (
	apiLogs        = "/machine.MachineService/Logs"
	apiDmesg       = "/machine.MachineService/Dmesg"
	apiServiceList = "/machine.MachineService/ServiceList"
	apiCOSIList    = "/cosi.resource.State/List"
)

// NewTalosTools creates tool handlers backed by the given pool. extensions
// is the parsed --extensions value (config.ParseExtensions).
func NewTalosTools(pool *talos.Pool, allowDestructive bool, extensions map[string]bool) *TalosTools {
	// The configured cluster_secret values are masked as literals.
	clusters := pool.ClustersWithDiscovery()
	secrets := make([]string, 0, len(clusters))

	for _, name := range clusters {
		secrets = append(secrets, pool.Discovery(name).ClusterSecret)
	}

	sanitizer, err := utils.NewTalosSanitizer(secrets...)
	if err != nil {
		// The patterns are constants; a failure is a programming error.
		panic(fmt.Sprintf("building the log sanitizer: %v", err))
	}

	return &TalosTools{
		pool:             pool,
		allowDestructive: allowDestructive,
		extensions:       extensions,
		sanitizer:        sanitizer,
	}
}

// RegisterTools registers the tools enabled by --extensions, --allow-destructive
// and the credential roles on the MCP server (design §7, §9).
func (t *TalosTools) RegisterTools(srv *mcp.Server) {
	if t.enabled(config.ExtensionCluster) {
		t.RegisterClustersList(srv)
		t.RegisterClustersDescribe(srv)
		t.RegisterClustersEvent(srv)

		// The discovery tool needs at least one cluster with discovery keys (§9).
		if len(t.pool.ClustersWithDiscovery()) > 0 {
			t.RegisterClustersMembers(srv)
		}
	}

	if t.enabled(config.ExtensionNode) {
		t.RegisterNodeDescribe(srv)
		t.RegisterNodeLogs(srv)
		t.RegisterNodeDmesg(srv)

		// The destructive tool needs the flag and at least one operator cluster (§9).
		if t.allowDestructive && len(t.pool.ClustersWithRole(talos.RoleOperator)) > 0 {
			t.RegisterNodeReboot(srv)
		}
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

// isRegistered reports whether a tool was registered, for hints that point
// to another tool.
func (t *TalosTools) isRegistered(name string) bool {
	return slices.ContainsFunc(t.registered, func(s toolSpec) bool { return s.name == name })
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
