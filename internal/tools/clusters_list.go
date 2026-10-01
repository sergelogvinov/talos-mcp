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

package tools

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
)

// ToolClustersList is the name of the clusters list tool.
const ToolClustersList = "talos_clusters_list"

// RegisterClustersList registers the clusters list tool.
func (t *TalosTools) RegisterClustersList(srv *mcp.Server) {
	mcp.AddTool(srv,
		&mcp.Tool{
			Name: ToolClustersList,
			Description: "List the Talos clusters (talosconfig contexts) configured in the MCP server, " +
				"with their endpoints, credential role and the tools usable on each. Makes no network calls.",
			Annotations: &mcp.ToolAnnotations{
				IdempotentHint: true,
				ReadOnlyHint:   true,
				OpenWorldHint:  new(false),
			},
		},
		t.handlerClustersList,
	)

	t.track(toolSpec{name: ToolClustersList, minRole: talos.RoleReader})
}

func (t *TalosTools) handlerClustersList(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ClustersListResult, error) {
	return textResult(t.ClustersList(time.Now()))
}

// ClustersList returns the configured clusters. now is used for the
// certificate expiry check.
func (t *TalosTools) ClustersList(now time.Time) *ClustersListResult {
	names := t.pool.List()

	result := &ClustersListResult{
		Current:  t.pool.Current(),
		Count:    len(names),
		Clusters: make([]ClusterSummary, 0, len(names)),
	}

	for _, name := range names {
		endpoints, nodes, err := t.pool.ContextInfo(name)
		if err != nil {
			continue
		}

		summary := ClusterSummary{
			Name:      name,
			Endpoints: endpoints,
			Nodes:     nodes,
			Current:   name == result.Current,
			Role:      t.pool.Role(name).String(),
			Tools:     t.toolsFor(name),
		}

		if cred := t.pool.Credential(name); cred != nil && cred.ExpiresSoon(now) {
			summary.CertExpires = cred.NotAfter.UTC().Format(time.RFC3339)
		}

		if d := t.pool.Discovery(name); d != nil {
			summary.Discovery = d.Endpoint
		}

		result.Clusters = append(result.Clusters, summary)
	}

	return result
}
