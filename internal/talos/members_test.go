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

package talos_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	"github.com/cosi-project/runtime/pkg/state/impl/namespaced"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/sergelogvinov/talos-mcp/internal/talos/talostest"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/resources/cluster"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stateClient is a Client that serves COSI from an in-memory state. Any other
// call panics through the nil embedded interface.
type stateClient struct {
	talos.Client

	st state.State
}

func (c *stateClient) State() state.State { return c.st }
func (c *stateClient) Close() error       { return nil }

type memberFixture struct {
	id, hostname string
	// nodeName is the resource ID (Kubernetes node name); defaults to id.
	nodeName    string
	machineType machine.Type
	addresses   []string
}

func newMemberState(t *testing.T, members ...memberFixture) state.State {
	t.Helper()

	st := state.WrapCore(namespaced.NewState(inmem.Build))

	for _, m := range members {
		nodeName := m.nodeName
		if nodeName == "" {
			nodeName = m.id
		}

		res := cluster.NewMember(cluster.NamespaceName, nodeName)
		spec := res.TypedSpec()
		spec.NodeID = m.id
		spec.Hostname = m.hostname
		spec.MachineType = m.machineType
		spec.OperatingSystem = "Talos (v1.14.2)"

		for _, a := range m.addresses {
			spec.Addresses = append(spec.Addresses, netip.MustParseAddr(a))
		}

		require.NoError(t, st.Create(t.Context(), res))
	}

	return st
}

// newMembersPool builds a pool whose clients serve st, or fail with clientErr.
func newMembersPool(t *testing.T, st state.State, clientErr error, contexts ...talostest.Context) *talos.Pool {
	t.Helper()

	tc, err := config.ParseTalosConfig([]byte(talostest.TalosConfig(t, now, contexts...)), "")
	require.NoError(t, err)

	pool, err := talos.NewPool(tc,
		talos.WithClock(func() time.Time { return now }),
		talos.WithClientFactory(func(context.Context, *clientconfig.Config, string) (talos.Client, error) {
			if clientErr != nil {
				return nil, clientErr
			}

			return &stateClient{st: st}, nil
		}),
	)
	require.NoError(t, err)

	return pool
}

var threeNodes = []memberFixture{
	{id: "worker-1", hostname: "worker-1", machineType: machine.TypeWorker, addresses: []string{"fe80::1", "10.0.0.21", "2001:db8::21"}},
	{id: "cp-1", hostname: "CP-1", machineType: machine.TypeControlPlane, addresses: []string{"10.0.0.11"}},
	{id: "cp-0", hostname: "cp-0", machineType: machine.TypeInit, addresses: []string{"10.0.0.10"}},
}

func TestPoolMembersFromCOSI(t *testing.T) {
	pool := newMembersPool(t, newMemberState(t, threeNodes...), nil,
		talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Endpoints: []string{"10.0.0.10"}})

	list, err := pool.Members(t.Context(), "")
	require.NoError(t, err)

	assert.Equal(t, talos.MemberSourceMembers, list.Source)
	assert.Empty(t, list.Warnings)
	assert.Equal(t, []talos.Member{
		{NodeID: "cp-0", NodeName: "cp-0", Hostname: "cp-0", MachineType: "controlplane", OperatingSystem: "Talos (v1.14.2)", Addresses: []string{"10.0.0.10"}},
		{NodeID: "cp-1", NodeName: "cp-1", Hostname: "CP-1", MachineType: "controlplane", OperatingSystem: "Talos (v1.14.2)", Addresses: []string{"10.0.0.11"}},
		{NodeID: "worker-1", NodeName: "worker-1", Hostname: "worker-1", MachineType: "worker", OperatingSystem: "Talos (v1.14.2)", Addresses: []string{"fe80::1", "10.0.0.21", "2001:db8::21"}},
	}, list.Members)
}

func TestPoolMembersFallback(t *testing.T) {
	for _, tt := range []struct {
		name            string
		st              state.State
		clientErr       error
		ctx             talostest.Context
		expected        []string
		expectedWarning string
	}{
		{
			name:            "apid error falls back to nodes",
			clientErr:       errors.New("bad key"),
			ctx:             talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Nodes: []string{"10.0.0.11", "10.0.0.12"}},
			expected:        []string{"10.0.0.11", "10.0.0.12"},
			expectedWarning: "cluster prod: reading Members through apid failed, using talosconfig addresses: cluster prod: creating Talos client: bad key",
		},
		{
			name:            "no members falls back to endpoints",
			st:              newMemberState(t),
			ctx:             talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Endpoints: []string{"10.0.0.10:50000", "api.example.com"}},
			expected:        []string{"10.0.0.10", "api.example.com"},
			expectedWarning: "cluster prod: apid returned no Members, using talosconfig addresses",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pool := newMembersPool(t, tt.st, tt.clientErr, tt.ctx)

			list, err := pool.Members(t.Context(), "prod")
			require.NoError(t, err, "a failed apid read does not fail the call")

			assert.Equal(t, talos.MemberSourceTalosconfig, list.Source)
			assert.Equal(t, []string{tt.expectedWarning}, list.Warnings)

			var addrs []string
			for _, m := range list.Members {
				assert.Empty(t, m.MachineType)
				addrs = append(addrs, m.Addresses...)
			}

			assert.Equal(t, tt.expected, addrs)
		})
	}
}

func TestPoolResolveNode(t *testing.T) {
	ctx := talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Endpoints: []string{"10.0.0.10"}}

	for _, tt := range []struct {
		name          string
		ctx           talostest.Context
		node          string
		expected      string
		expectedName  string
		expectedError error
		expectedMsg   string
	}{
		{name: "ip is used as is", ctx: ctx, node: "192.168.1.5", expected: "192.168.1.5", expectedName: "192.168.1.5"},
		{name: "hostname, case-insensitive", ctx: ctx, node: "cp-1", expected: "10.0.0.11", expectedName: "CP-1"},
		{name: "skips link-local, prefers endpoint family", ctx: ctx, node: "worker-1", expected: "10.0.0.21", expectedName: "worker-1"},
		{
			name:         "prefers IPv6 with IPv6 endpoints",
			ctx:          talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Endpoints: []string{"[2001:db8::10]:50000"}},
			node:         "worker-1",
			expected:     "2001:db8::21",
			expectedName: "worker-1",
		},
		{
			name:         "empty node uses the single default node",
			ctx:          talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Nodes: []string{"10.0.0.11"}},
			expected:     "10.0.0.11",
			expectedName: "10.0.0.11",
		},
		{
			name:          "empty node without default lists members",
			ctx:           ctx,
			expectedError: talos.ErrNoDefaultNode,
			expectedMsg:   "no default node: cluster prod has 0 default nodes in talosconfig, set node to one of cp-0, CP-1, worker-1",
		},
		{
			name:          "unknown hostname lists members",
			ctx:           ctx,
			node:          "worker-9",
			expectedError: talos.ErrUnknownNode,
			expectedMsg:   `unknown node "worker-9" in cluster prod: known nodes are cp-0, CP-1, worker-1`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pool := newMembersPool(t, newMemberState(t, threeNodes...), nil, tt.ctx)

			got, err := pool.ResolveNode(t.Context(), "", tt.node)
			if tt.expectedError != nil {
				require.ErrorIs(t, err, tt.expectedError)
				assert.EqualError(t, err, tt.expectedMsg)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expected, got.Address)
			assert.Equal(t, tt.expectedName, got.Name)
		})
	}
}

func TestPoolResolveNodeAmbiguous(t *testing.T) {
	st := newMemberState(t,
		memberFixture{id: "a", hostname: "node", machineType: machine.TypeWorker, addresses: []string{"10.0.0.1"}},
		memberFixture{id: "b", hostname: "NODE", machineType: machine.TypeWorker, addresses: []string{"10.0.0.2"}},
	)
	pool := newMembersPool(t, st, nil, talostest.Context{Name: "prod", Roles: []string{"os:reader"}})

	_, err := pool.ResolveNode(t.Context(), "prod", "node")
	require.ErrorIs(t, err, talos.ErrAmbiguousNode)
	assert.EqualError(t, err, `ambiguous node "node" in cluster prod: matches `+
		`node (node ID a, addresses 10.0.0.1); NODE (node ID b, addresses 10.0.0.2); use an address instead`)
}

func TestPoolResolveNodeFromTalosconfig(t *testing.T) {
	pool := newMembersPool(t, nil, errors.New("unreachable"),
		talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Nodes: []string{"node-a.example.com", "node-b.example.com"}})

	got, err := pool.ResolveNode(t.Context(), "prod", "NODE-B.example.com")
	require.NoError(t, err)
	assert.Equal(t, "node-b.example.com", got.Address)
	assert.Len(t, got.Warnings, 1, "the talosconfig fallback is reported")
}

func TestSelectAddress(t *testing.T) {
	kubespan := "fd71:4a6b:e5a3:9c02::1" // fd + purpose byte 0x02 at index 7
	siderolink := "fd71:4a6b:e5a3:9c03::1"

	assert.Equal(t, "10.0.0.1", talos.SelectAddress([]string{"fe80::1", kubespan, "10.0.0.1"}, false))
	assert.Equal(t, "2001:db8::1", talos.SelectAddress([]string{"10.0.0.1", "2001:db8::1"}, true))
	assert.Equal(t, "10.0.0.1", talos.SelectAddress([]string{siderolink, "10.0.0.1"}, true), "falls back to the other family")
	assert.Equal(t, kubespan, talos.SelectAddress([]string{"fe80::1", kubespan}, false), "a ULA is the last resort")
	assert.Equal(t, "fd12:3456:789a:2::5", talos.SelectAddress([]string{"fd12:3456:789a:2::5"}, true), "a site ULA that looks like KubeSpan is still usable")
	assert.Empty(t, talos.SelectAddress([]string{"fe80::1"}, false), "link-local is never used")
}

func TestSelectDiscoveryAddress(t *testing.T) {
	kubespan := "fd71:4a6b:e5a3:9c02::1"
	siderolink := "fd71:4a6b:e5a3:9c03::1"
	all := []string{"fe80::1", siderolink, "203.0.113.1", "2001:db8::1", kubespan, "fd00::1", "10.0.0.1"}

	for _, tt := range []struct {
		name     string
		addrs    []string
		expected string
	}{
		{name: "private IPv4 first", addrs: all, expected: "10.0.0.1"},
		{name: "then private IPv6", addrs: all[:6], expected: "fd00::1"},
		{name: "then KubeSpan", addrs: all[:5], expected: kubespan},
		{name: "then public IPv6", addrs: all[:4], expected: "2001:db8::1"},
		{name: "then public IPv4", addrs: all[:3], expected: "203.0.113.1"},
		{name: "SideroLink is never used", addrs: all[:2]},
		{name: "DNS name", addrs: []string{"node.example.com"}, expected: "node.example.com"},
		{name: "first of a kind wins", addrs: []string{"192.168.1.1", "10.0.0.1"}, expected: "192.168.1.1"},
		{name: "link-local is never used", addrs: []string{"fe80::1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, talos.SelectDiscoveryAddress(tt.addrs))
		})
	}
}

func TestPoolResolveNodeWithDiscovery(t *testing.T) {
	st := newMemberState(t,
		memberFixture{id: "cp-1", hostname: "cp-1", machineType: machine.TypeControlPlane, addresses: []string{"2001:db8::11", "10.0.0.11"}},
	)

	// Without discovery, the IPv6 endpoint makes IPv6 the preferred family.
	pool := newMembersPool(t, st, nil, talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Endpoints: []string{"2001:db8::10"}})
	got, err := pool.ResolveNode(t.Context(), "prod", "cp-1")
	require.NoError(t, err)
	assert.Equal(t, "2001:db8::11", got.Address)

	// With discovery, the local network wins.
	pool = newMembersPool(t, st, nil, talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Endpoints: []string{"2001:db8::10"}, Discovery: "prod-id"})
	got, err = pool.ResolveNode(t.Context(), "prod", "cp-1")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.11", got.Address)

	targets, err := pool.ResolveAllNodes(t.Context(), "prod")
	require.NoError(t, err)
	require.Len(t, targets.Nodes, 1)
	assert.Equal(t, "10.0.0.11", targets.Nodes[0].Address)
}

func TestPoolResolveNodeForms(t *testing.T) {
	st := newMemberState(t,
		memberFixture{id: "node-id-1", hostname: "cp-1", nodeName: "cp-1.example.com", machineType: machine.TypeControlPlane, addresses: []string{"10.0.0.11"}},
	)
	pool := newMembersPool(t, st, nil, talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Endpoints: []string{"10.0.0.10"}})

	for _, tt := range []struct {
		node     string
		expected string
	}{
		{node: "cp-1.example.com", expected: "10.0.0.11"}, // Kubernetes node name (registerWithFQDN)
		{node: "node-id-1", expected: "10.0.0.11"},
		{node: "10.0.0.1:50000", expected: "10.0.0.1"},
		{node: "[2001:db8::1]", expected: "2001:db8::1"},
		{node: "[2001:db8::1]:50000", expected: "2001:db8::1"},
		{node: " cp-1 ", expected: "10.0.0.11"},
	} {
		t.Run(tt.node, func(t *testing.T) {
			got, err := pool.ResolveNode(t.Context(), "prod", tt.node)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, got.Address)
		})
	}
}

func TestPoolResolveNodeErrorExplainsFallback(t *testing.T) {
	pool := newMembersPool(t, nil, errors.New("certificate expired"),
		talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Endpoints: []string{"10.0.0.10"}})

	_, err := pool.ResolveNode(t.Context(), "prod", "cp-1")
	require.ErrorIs(t, err, talos.ErrUnknownNode)
	assert.EqualError(t, err, `unknown node "cp-1" in cluster prod: known nodes are 10.0.0.10 `+
		`(cluster prod: reading Members through apid failed, using talosconfig addresses: cluster prod: creating Talos client: certificate expired)`)
}
