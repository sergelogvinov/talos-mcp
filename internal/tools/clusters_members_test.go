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
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/sergelogvinov/talos-mcp/internal/talos/talostest"
	"github.com/sergelogvinov/talos-mcp/internal/tools"
	clientpb "github.com/siderolabs/discovery-api/api/v1alpha1/client/pb"
	serverpb "github.com/siderolabs/discovery-api/api/v1alpha1/server/pb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newDiscoveryTools builds tools whose pool reads affiliates from fake.
func newDiscoveryTools(t *testing.T, fake *talostest.FakeDiscovery, contexts ...talostest.Context) *tools.TalosTools {
	t.Helper()

	dialer, _ := talostest.StartFakeDiscovery(t, fake)

	tc, err := config.ParseTalosConfig([]byte(talostest.TalosConfig(t, now, contexts...)), "")
	require.NoError(t, err)

	pool, err := talos.NewPool(tc,
		talos.WithClock(func() time.Time { return now }),
		talos.WithDiscoveryDialer(dialer),
	)
	require.NoError(t, err)

	return tools.NewTalosTools(pool, false, allExtensions())
}

func affiliatesFixture(t *testing.T) []*serverpb.Affiliate {
	t.Helper()

	block := talostest.Cipher(t, talostest.Secret)
	wrong := talostest.Cipher(t, "ZmVkY2JhOTg3NjU0MzIxMGZlZGNiYTk4NzY1NDMyMTA=")

	return []*serverpb.Affiliate{
		{Id: "w2", Data: talostest.EncryptAffiliate(t, block, &clientpb.Affiliate{NodeId: "w2", Hostname: "worker-2", MachineType: "worker", Addresses: [][]byte{talostest.IP("10.0.0.22")}})},
		{Id: "ks", Data: talostest.EncryptAffiliate(t, block, &clientpb.Affiliate{NodeId: "ext-1", Kubespan: &clientpb.KubeSpan{PublicKey: "extkey=", Address: talostest.IP("fd71:4a6b:e5a3:9c02::99")}})},
		{
			Id: "cp1",
			Data: talostest.EncryptAffiliate(t, block, &clientpb.Affiliate{
				NodeId: "cp1", Hostname: "cp-1", Nodename: "cp-1", MachineType: "controlplane", OperatingSystem: "Talos (v1.14.2)",
				Addresses: [][]byte{talostest.IP("10.0.0.11")}, ControlPlane: &clientpb.ControlPlane{ApiServerPort: 6443},
			}),
			Endpoints: [][]byte{talostest.EncryptEndpoint(t, block, "203.0.113.11:51820")},
		},
		{Id: "w1", Data: talostest.EncryptAffiliate(t, block, &clientpb.Affiliate{NodeId: "w1", Hostname: "Worker-1", MachineType: "worker", Addresses: [][]byte{talostest.IP("10.0.0.21")}})},
		{Id: "foreign-1", Data: talostest.EncryptAffiliate(t, wrong, &clientpb.Affiliate{NodeId: "x"})},
		{Id: "foreign-2", Data: talostest.EncryptAffiliate(t, wrong, &clientpb.Affiliate{NodeId: "y"})},
	}
}

func TestClustersMembers(t *testing.T) {
	fake := &talostest.FakeDiscovery{Affiliates: affiliatesFixture(t)}
	tt := newDiscoveryTools(t, fake, discoveryContexts()...)

	result, err := tt.ClustersMembers(t.Context(), "", "")
	require.NoError(t, err)

	assert.Equal(t, &tools.ClustersMembersResult{
		Cluster:   "prod",
		ClusterID: "prod-id",
		Endpoint:  config.DefaultDiscoveryEndpoint,
		Count:     4,
		Members: []tools.MemberSummary{
			{
				NodeID: "cp1", Hostname: "cp-1", NodeName: "cp-1", Role: "controlplane", OperatingSystem: "Talos (v1.14.2)",
				Addresses: []string{"10.0.0.11"}, Endpoints: []string{"203.0.113.11:51820"}, APIServerPort: new(6443),
			},
			{NodeID: "w1", Hostname: "Worker-1", Role: "worker", Addresses: []string{"10.0.0.21"}},
			{NodeID: "w2", Hostname: "worker-2", Role: "worker", Addresses: []string{"10.0.0.22"}},
			{
				NodeID: "ext-1", Addresses: []string{},
				KubeSpan: &tools.KubeSpanInfo{Address: "fd71:4a6b:e5a3:9c02::99", PublicKey: "extkey="},
			},
		},
		Warnings: []string{"2 affiliates could not be decrypted (wrong cluster_secret, or another cluster sharing the ID?)"},
	}, result)
}

func TestClustersMembersRoleFilter(t *testing.T) {
	fake := &talostest.FakeDiscovery{Affiliates: affiliatesFixture(t)}
	tt := newDiscoveryTools(t, fake, discoveryContexts()...)

	result, err := tt.ClustersMembers(t.Context(), "prod", "controlplane")
	require.NoError(t, err)
	require.Equal(t, 1, result.Count)
	assert.Equal(t, "cp-1", result.Members[0].Hostname)

	result, err = tt.ClustersMembers(t.Context(), "prod", "worker")
	require.NoError(t, err)
	assert.Equal(t, 2, result.Count)

	_, err = tt.ClustersMembers(t.Context(), "prod", "etcd")
	require.EqualError(t, err, `invalid role "etcd": must be controlplane or worker`)
}

func TestClustersMembersRejectsClusterWithoutKeys(t *testing.T) {
	fake := &talostest.FakeDiscovery{Affiliates: affiliatesFixture(t)}
	tt := newDiscoveryTools(t, fake, discoveryContexts()...)

	_, err := tt.ClustersMembers(t.Context(), "staging", "")
	require.ErrorIs(t, err, talos.ErrNoDiscovery)
	require.EqualError(t, err, "cluster staging: no discovery keys; clusters with discovery: dev, prod")
	assert.Zero(t, fake.Calls("List"), "rejected before any network call")
}

func TestClustersMembersServiceUnreachable(t *testing.T) {
	fake := &talostest.FakeDiscovery{Err: status.Error(codes.Unavailable, "connection refused")}
	session := newServer(t, newDiscoveryTools(t, fake, discoveryContexts()...))

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tools.ToolClustersMembers, Arguments: map[string]any{"cluster": "prod"}})
	require.NoError(t, err)
	require.True(t, res.IsError, "an unreachable service is a tool error, with no apid fallback")
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, "discovery service unreachable")
}

func TestClustersMembersCall(t *testing.T) {
	fake := &talostest.FakeDiscovery{Affiliates: affiliatesFixture(t)}
	session := newServer(t, newDiscoveryTools(t, fake, discoveryContexts()...))

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tools.ToolClustersMembers, Arguments: map[string]any{"role": "worker"}})
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, "| w1 | Worker-1 |")

	// Empty strings mean the default cluster and all roles.
	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: tools.ToolClustersMembers, Arguments: map[string]any{"cluster": "", "role": ""}})
	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)

	// The schema enum rejects a cluster without discovery keys, before the
	// handler runs.
	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: tools.ToolClustersMembers, Arguments: map[string]any{"cluster": "staging"}})
	require.NoError(t, err)
	require.True(t, res.IsError)

	text := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, `validating "arguments"`)
	assert.Contains(t, text, "enum")
	assert.NotContains(t, text, "no discovery keys", "the handler was not reached")

	for _, rpc := range []string{"Hello", "AffiliateUpdate", "AffiliateDelete", "Watch"} {
		assert.Zero(t, fake.Calls(rpc), "%s must never be called", rpc)
	}
}

func discoveryContexts() []talostest.Context {
	return []talostest.Context{
		{Name: "prod", Roles: []string{"os:reader"}, Discovery: "prod-id"},
		{Name: "staging", Roles: []string{"os:operator"}},
		{Name: "dev", Roles: []string{"os:reader"}, Discovery: "dev-id"},
	}
}

// membersTool returns the members tool as listed by the server, or nil.
func membersTool(t *testing.T, tt *tools.TalosTools) *mcp.Tool {
	t.Helper()

	res, err := newServer(t, tt).ListTools(t.Context(), nil)
	require.NoError(t, err)

	for _, tool := range res.Tools {
		if tool.Name == tools.ToolClustersMembers {
			return tool
		}
	}

	return nil
}

func inputSchema(t *testing.T, tool *mcp.Tool) *jsonschema.Schema {
	t.Helper()

	data, err := json.Marshal(tool.InputSchema)
	require.NoError(t, err)

	var s jsonschema.Schema
	require.NoError(t, json.Unmarshal(data, &s))

	return &s
}

func TestClustersMembersRegistration(t *testing.T) {
	t.Run("absent without discovery keys", func(t *testing.T) {
		pool := newPool(t,
			talostest.Context{Name: "prod", Roles: []string{"os:reader"}},
			talostest.Context{Name: "staging", Roles: []string{"os:operator"}},
		)

		assert.Nil(t, membersTool(t, tools.NewTalosTools(pool, true, allExtensions())))
	})

	t.Run("enum of discovery clusters, cluster optional when current has keys", func(t *testing.T) {
		tool := membersTool(t, newDiscoveryTools(t, &talostest.FakeDiscovery{}, discoveryContexts()...))
		require.NotNil(t, tool)

		assert.Contains(t, tool.Description, "Available on: dev, prod.")

		s := inputSchema(t, tool)
		assert.Equal(t, []any{"", "dev", "prod"}, s.Properties["cluster"].Enum, `"" means the current cluster`)
		assert.Equal(t, []any{"", "controlplane", "worker"}, s.Properties["role"].Enum, `"" means all roles`)
		assert.NotContains(t, s.Required, "cluster")
	})

	t.Run("cluster required when current has no keys", func(t *testing.T) {
		contexts := discoveryContexts()
		contexts[0], contexts[1] = contexts[1], contexts[0] // staging becomes current

		tool := membersTool(t, newDiscoveryTools(t, &talostest.FakeDiscovery{}, contexts...))
		require.NotNil(t, tool)

		s := inputSchema(t, tool)
		assert.Contains(t, s.Required, "cluster")
		assert.Equal(t, []any{"dev", "prod"}, s.Properties["cluster"].Enum, `"" is not allowed when cluster is required`)
	})
}
