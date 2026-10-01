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

package tools_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/sergelogvinov/talos-mcp/internal/talos/talostest"
	"github.com/sergelogvinov/talos-mcp/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// mixedContexts has a reader cluster with discovery and default nodes, an
// operator cluster whose certificate expires soon, and an admin cluster.
func mixedContexts() []talostest.Context {
	return []talostest.Context{
		{Name: "prod", Roles: []string{"os:reader"}, Nodes: []string{"10.0.0.11"}, Discovery: "prod-id"},
		{Name: "staging", Roles: []string{"os:operator"}, NotAfter: now.Add(48 * time.Hour)},
		{Name: "dev", Roles: []string{"os:admin"}, Endpoints: []string{"10.1.0.1", "10.1.0.2"}},
	}
}

func newPool(t *testing.T, contexts ...talostest.Context) *talos.Pool {
	t.Helper()

	tc, err := config.ParseTalosConfig([]byte(talostest.TalosConfig(t, now, contexts...)), "")
	require.NoError(t, err)

	pool, err := talos.NewPool(tc, talos.WithClock(func() time.Time { return now }))
	require.NoError(t, err)

	t.Cleanup(func() { pool.Close() }) //nolint:errcheck

	return pool
}

// newServer registers the tools on a fresh MCP server and connects an
// in-memory client to it.
func newServer(t *testing.T, tt *tools.TalosTools) *mcp.ClientSession {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "dev"}, nil)
	tt.RegisterTools(srv)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverSession, err := srv.Connect(t.Context(), serverTransport, nil)
	require.NoError(t, err)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "dev"}, nil)

	clientSession, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(t, err)

	t.Cleanup(func() {
		clientSession.Close() //nolint:errcheck
		serverSession.Close() //nolint:errcheck
	})

	return clientSession
}

func allExtensions() map[string]bool {
	return map[string]bool{config.ExtensionCluster: true, config.ExtensionNode: true}
}

// TestToolSchemas checks that every registered tool has valid input and
// output schemas (design §14). Later tools are covered automatically.
func TestToolSchemas(t *testing.T) {
	session := newServer(t, tools.NewTalosTools(newPool(t, mixedContexts()...), true, allExtensions()))

	res, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, res.Tools)

	for _, tool := range res.Tools {
		t.Run(tool.Name, func(t *testing.T) {
			assert.NotEmpty(t, tool.Description)
			require.NotNil(t, tool.Annotations, "every tool declares annotations")

			for kind, schema := range map[string]any{"input": tool.InputSchema, "output": tool.OutputSchema} {
				require.NotNil(t, schema, "%s schema", kind)

				data, err := json.Marshal(schema)
				require.NoError(t, err)

				var raw any
				require.NoError(t, json.Unmarshal(data, &raw))
				assert.Empty(t, typeLists(raw, kind), "a nullable field uses anyOf branches, not a `type` list")

				var s jsonschema.Schema
				require.NoError(t, json.Unmarshal(data, &s), "%s schema", kind)
				assert.Equal(t, "object", s.Type, "%s schema", kind)

				_, err = s.Resolve(nil)
				require.NoError(t, err, "%s schema resolves", kind)
			}
		})
	}
}

// typeLists returns the paths of the schemas in node whose `type` is a list,
// such as ["null","array"].
func typeLists(node any, path string) []string {
	var found []string

	switch n := node.(type) {
	case map[string]any:
		if _, ok := n["type"].([]any); ok {
			found = append(found, path)
		}

		for k, v := range n {
			found = append(found, typeLists(v, path+"/"+k)...)
		}
	case []any:
		for i, v := range n {
			found = append(found, typeLists(v, fmt.Sprintf("%s/%d", path, i))...)
		}
	}

	return found
}

func TestRegisterToolsExtensions(t *testing.T) {
	pool := newPool(t, mixedContexts()...)

	for _, tt := range []struct {
		name       string
		extensions map[string]bool
		expected   []string
	}{
		{name: "all", extensions: allExtensions(), expected: []string{
			tools.ToolClustersDescribe, tools.ToolClustersEvent, tools.ToolClustersList, tools.ToolClustersMembers, tools.ToolNodeDmesg, tools.ToolNodeLogs,
		}},
		{name: "cluster", extensions: map[string]bool{config.ExtensionCluster: true}, expected: []string{
			tools.ToolClustersDescribe, tools.ToolClustersEvent, tools.ToolClustersList, tools.ToolClustersMembers,
		}},
		{name: "node", extensions: map[string]bool{config.ExtensionNode: true}, expected: []string{tools.ToolNodeDmesg, tools.ToolNodeLogs}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			session := newServer(t, tools.NewTalosTools(pool, false, tt.extensions))

			res, err := session.ListTools(t.Context(), nil)
			require.NoError(t, err)

			names := make([]string, 0, len(res.Tools))
			for _, tool := range res.Tools {
				names = append(names, tool.Name)
			}

			assert.Equal(t, tt.expected, names)
		})
	}
}

func TestClustersList(t *testing.T) {
	tt := tools.NewTalosTools(newPool(t, mixedContexts()...), false, allExtensions())
	newServer(t, tt) // registers the tools, which fills the per-cluster tool lists

	assert.Equal(t, &tools.ClustersListResult{
		Current: "prod",
		Count:   3,
		Clusters: []tools.ClusterSummary{
			{
				Name:      "dev",
				Endpoints: []string{"10.1.0.1", "10.1.0.2"},
				Role:      "operator",
				Tools:     []string{tools.ToolClustersDescribe, tools.ToolClustersEvent, tools.ToolClustersList, tools.ToolNodeDmesg, tools.ToolNodeLogs},
			},
			{
				Name:      "prod",
				Endpoints: []string{"prod.example.com"},
				Nodes:     []string{"10.0.0.11"},
				Current:   true,
				Discovery: config.DefaultDiscoveryEndpoint,
				Role:      "reader",
				Tools:     []string{tools.ToolClustersDescribe, tools.ToolClustersEvent, tools.ToolClustersList, tools.ToolClustersMembers, tools.ToolNodeDmesg, tools.ToolNodeLogs},
			},
			{
				Name:        "staging",
				Endpoints:   []string{"staging.example.com"},
				CertExpires: "2026-10-03T12:00:00Z",
				Role:        "operator",
				Tools:       []string{tools.ToolClustersDescribe, tools.ToolClustersEvent, tools.ToolClustersList, tools.ToolNodeDmesg, tools.ToolNodeLogs},
			},
		},
	}, tt.ClustersList(now))
}

func TestClustersListCall(t *testing.T) {
	session := newServer(t, tools.NewTalosTools(newPool(t, mixedContexts()...), false, allExtensions()))

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tools.ToolClustersList})
	require.NoError(t, err)
	require.False(t, res.IsError)

	require.Len(t, res.Content, 1)
	text := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, "Default cluster: prod")
	assert.Contains(t, text, "| dev |")

	data, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)

	var structured tools.ClustersListResult
	require.NoError(t, json.Unmarshal(data, &structured))
	assert.Equal(t, 3, structured.Count)
	assert.Equal(t, "prod", structured.Current)
}
