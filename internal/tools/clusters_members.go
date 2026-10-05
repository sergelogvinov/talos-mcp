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
	"fmt"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
)

// ToolClustersMembers is the name of the discovery members tool.
const ToolClustersMembers = "talos_clusters_members"

type clustersMembersInput struct {
	Cluster string `json:"cluster,omitempty" jsonschema:"Cluster name (talosconfig context); default is current"`
	Role    string `json:"role,omitempty" jsonschema:"Filter by machine type: controlplane or worker (default all)"`
}

// RegisterClustersMembers registers the discovery members tool. It is only
// called when at least one cluster has discovery keys.
func (t *TalosTools) RegisterClustersMembers(srv *mcp.Server) {
	clusters := t.pool.ClustersWithDiscovery()

	schema, err := jsonschema.For[clustersMembersInput](nil)
	if err != nil {
		panic(fmt.Sprintf("%s: input schema: %v", ToolClustersMembers, err))
	}

	// An empty role means all, so "" is a valid value next to the types.
	schema.Properties["role"].Enum = toAny([]string{"", talos.MachineTypeControlPlane, talos.MachineTypeWorker})

	// cluster may be omitted, or sent empty, only when the default cluster has
	// discovery keys.
	if slices.Contains(clusters, t.pool.Current()) {
		schema.Properties["cluster"].Enum = toAny(append([]string{""}, clusters...))
	} else {
		schema.Properties["cluster"].Enum = toAny(clusters)
		schema.Required = append(schema.Required, "cluster")
	}

	addTool(srv,
		&mcp.Tool{
			Name: ToolClustersMembers,
			Description: "List the members of a Talos cluster as the discovery service sees them: node ID, hostname, role, " +
				"addresses and KubeSpan data. Needs no Talos API access, so it works when the endpoints are down. " +
				"Available on: " + strings.Join(clusters, ", ") + ".",
			InputSchema: schema,
			Annotations: &mcp.ToolAnnotations{
				IdempotentHint: true,
				ReadOnlyHint:   true,
				OpenWorldHint:  new(true),
			},
		},
		t.handlerClustersMembers,
	)

	t.track(toolSpec{name: ToolClustersMembers, minRole: talos.RoleReader, needDiscovery: true})
}

func (t *TalosTools) handlerClustersMembers(ctx context.Context, _ *mcp.CallToolRequest, in clustersMembersInput) (*mcp.CallToolResult, ClustersMembersResult, error) {
	ctx, cancel := withTimeout(ctx, defaultTimeout)
	defer cancel()

	result, err := t.ClustersMembers(ctx, in.Cluster, in.Role)
	if err != nil {
		return nil, ClustersMembersResult{}, err
	}

	return textResult(result)
}

// ClustersMembers lists the discovery service affiliates of a cluster,
// optionally filtered by machine type. A cluster without discovery keys is
// rejected by Pool.Affiliates before any network call, and there is no
// fallback to apid.
func (t *TalosTools) ClustersMembers(ctx context.Context, cluster, role string) (*ClustersMembersResult, error) {
	name, err := t.pool.Resolve(cluster)
	if err != nil {
		return nil, err
	}

	switch role {
	case "", talos.MachineTypeControlPlane, talos.MachineTypeWorker:
	default:
		return nil, fmt.Errorf("invalid role %q: must be controlplane or worker", role)
	}

	if err := t.pool.Require(name, talos.RoleReader); err != nil {
		return nil, err
	}

	list, err := t.pool.Affiliates(ctx, name)
	if err != nil {
		return nil, err
	}

	result := &ClustersMembersResult{
		Cluster:   name,
		ClusterID: list.ClusterID,
		Endpoint:  list.Endpoint,
		Members:   []MemberSummary{},
	}

	for _, a := range list.Affiliates {
		if role != "" && a.MachineType != role {
			continue
		}

		result.Members = append(result.Members, newMemberSummary(a))
	}

	result.Count = len(result.Members)

	if list.Undecryptable > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%d affiliates could not be decrypted (wrong cluster_secret, or another cluster sharing the ID?)", list.Undecryptable))
	}

	return result, nil
}

func newMemberSummary(a talos.Affiliate) MemberSummary {
	m := MemberSummary{
		NodeID:          a.NodeID,
		Hostname:        a.Hostname,
		NodeName:        a.NodeName,
		Role:            a.MachineType,
		OperatingSystem: a.OperatingSystem,
		Addresses:       a.Addresses,
		Endpoints:       a.Endpoints,
	}

	if m.Addresses == nil {
		m.Addresses = []string{}
	}

	if a.APIServerPort != 0 {
		m.APIServerPort = &a.APIServerPort
	}

	if a.KubeSpan != nil {
		m.KubeSpan = &KubeSpanInfo{
			Address:             a.KubeSpan.Address,
			PublicKey:           a.KubeSpan.PublicKey,
			AdditionalAddresses: a.KubeSpan.AdditionalAddresses,
		}
	}

	return m
}

func toAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}

	return out
}
