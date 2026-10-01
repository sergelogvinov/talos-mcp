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
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/sergelogvinov/talos-mcp/internal/talos/talostest"
	"github.com/sergelogvinov/talos-mcp/internal/tools"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/resources/cluster"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// rebootContexts: prod is a reader cluster (the current one), staging an
// operator cluster and dev an admin cluster.
func rebootContexts() []talostest.Context {
	return []talostest.Context{
		{Name: "prod", Roles: []string{"os:reader"}},
		{Name: "staging", Roles: []string{"os:operator"}, Endpoints: []string{"10.0.0.10"}},
		{Name: "dev", Roles: []string{"os:admin"}},
	}
}

// newRebootTools builds tools with --allow-destructive whose clients are
// fake. clients counts client creations, so a test can check that no Talos
// call was attempted.
func newRebootTools(t *testing.T, fake *talostest.FakeClient, allowDestructive bool, contexts ...talostest.Context) (*tools.TalosTools, *atomic.Int32) {
	t.Helper()

	tc, err := config.ParseTalosConfig([]byte(talostest.TalosConfig(t, now, contexts...)), "")
	require.NoError(t, err)

	var clients atomic.Int32

	pool, err := talos.NewPool(tc,
		talos.WithClock(func() time.Time { return now }),
		talos.WithClientFactory(func(context.Context, *clientconfig.Config, string) (talos.Client, error) {
			clients.Add(1)

			return fake, nil
		}),
	)
	require.NoError(t, err)

	t.Cleanup(func() { pool.Close() }) //nolint:errcheck

	return tools.NewTalosTools(pool, allowDestructive, allExtensions()), &clients
}

func toolByName(t *testing.T, tt *tools.TalosTools, name string) *mcp.Tool {
	t.Helper()

	res, err := newServer(t, tt).ListTools(t.Context(), nil)
	require.NoError(t, err)

	for _, tool := range res.Tools {
		if tool.Name == name {
			return tool
		}
	}

	return nil
}

// TestNodeRebootRegistration is the registration matrix of design §14.
func TestNodeRebootRegistration(t *testing.T) {
	readers := []talostest.Context{
		{Name: "prod", Roles: []string{"os:reader"}},
		{Name: "qa", Roles: []string{"os:reader"}},
	}

	t.Run("absent without --allow-destructive", func(t *testing.T) {
		tt, _ := newRebootTools(t, &talostest.FakeClient{}, false, rebootContexts()...)
		assert.Nil(t, toolByName(t, tt, tools.ToolNodeReboot))
	})

	t.Run("absent when every cluster is a reader", func(t *testing.T) {
		tt, _ := newRebootTools(t, &talostest.FakeClient{}, true, readers...)
		assert.Nil(t, toolByName(t, tt, tools.ToolNodeReboot))
	})

	t.Run("present with the operator clusters as enum", func(t *testing.T) {
		tt, _ := newRebootTools(t, &talostest.FakeClient{}, true, rebootContexts()...)

		tool := toolByName(t, tt, tools.ToolNodeReboot)
		require.NotNil(t, tool)

		assert.Contains(t, tool.Description, "Available on: dev, staging.")
		require.NotNil(t, tool.Annotations)
		assert.Equal(t, new(true), tool.Annotations.DestructiveHint)
		assert.False(t, tool.Annotations.ReadOnlyHint)

		s := inputSchema(t, tool)
		assert.Equal(t, []any{"dev", "staging"}, s.Properties["cluster"].Enum)
		assert.Equal(t, []any{"", "default", "powercycle"}, s.Properties["mode"].Enum)
		assert.ElementsMatch(t, []string{"cluster", "node"}, s.Required)
	})

	t.Run("clusters list shows reboot only on operator clusters", func(t *testing.T) {
		tt, _ := newRebootTools(t, &talostest.FakeClient{}, true, rebootContexts()...)
		newServer(t, tt)

		for _, c := range tt.ClustersList(now).Clusters {
			if c.Role == "operator" {
				assert.Contains(t, c.Tools, tools.ToolNodeReboot, c.Name)
			} else {
				assert.NotContains(t, c.Tools, tools.ToolNodeReboot, c.Name)
			}
		}
	})
}

func TestNodeRebootRejectedBeforeTalos(t *testing.T) {
	for _, tc := range []struct {
		name        string
		in          tools.NodeRebootInput
		expectedMsg string
	}{
		{
			name:        "reader cluster",
			in:          tools.NodeRebootInput{Cluster: "prod", Node: "10.0.0.21"},
			expectedMsg: "credential role denied: cluster prod uses an os:reader credential; os:operator is required",
		},
		{
			name:        "cluster is required",
			in:          tools.NodeRebootInput{Node: "10.0.0.21"},
			expectedMsg: "cluster is required for talos_node_reboot; operator clusters: dev, staging",
		},
		{
			name:        "node is required",
			in:          tools.NodeRebootInput{Cluster: "staging"},
			expectedMsg: "node is required for talos_node_reboot: name exactly one node",
		},
		{
			name:        "several nodes",
			in:          tools.NodeRebootInput{Cluster: "staging", Node: "10.0.0.21,10.0.0.22"},
			expectedMsg: `node "10.0.0.21,10.0.0.22" names several nodes; talos_node_reboot reboots exactly one node per call`,
		},
		{
			name:        "invalid mode",
			in:          tools.NodeRebootInput{Cluster: "staging", Node: "10.0.0.21", Mode: "force"},
			expectedMsg: `invalid mode "force": must be default or powercycle`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &talostest.FakeClient{}
			tt, clients := newRebootTools(t, fake, true, rebootContexts()...)

			_, err := tt.NodeReboot(t.Context(), tc.in)
			require.EqualError(t, err, tc.expectedMsg)
			assert.Zero(t, clients.Load(), "no Talos client is created")
			assert.Empty(t, fake.Reboots())
		})
	}
}

func TestNodeRebootReaderClusterViaMCP(t *testing.T) {
	fake := &talostest.FakeClient{}
	tt, clients := newRebootTools(t, fake, true, rebootContexts()...)

	res, err := newServer(t, tt).CallTool(t.Context(), &mcp.CallToolParams{
		Name:      tools.ToolNodeReboot,
		Arguments: map[string]any{"cluster": "prod", "node": "10.0.0.21"},
	})
	require.NoError(t, err)
	require.True(t, res.IsError, "the cluster enum rejects a reader cluster")
	assert.Zero(t, clients.Load())
}

// etcdMember returns an etcd member with peer and client URLs on ip.
func etcdMember(host, ip string, learner bool) *machineapi.EtcdMember {
	return &machineapi.EtcdMember{
		Hostname:   host,
		PeerUrls:   []string{"https://" + ip + ":2380"},
		ClientUrls: []string{"https://" + ip + ":2379"},
		IsLearner:  learner,
	}
}

func threeEtcd() []*machineapi.EtcdMember {
	return []*machineapi.EtcdMember{
		etcdMember("cp-1", "10.0.0.11", false),
		etcdMember("cp-2", "10.0.0.12", false),
		etcdMember("cp-3", "10.0.0.13", false),
	}
}

func TestNodeReboot(t *testing.T) {
	for _, tc := range []struct {
		name          string
		fake          *talostest.FakeClient
		in            tools.NodeRebootInput
		expectedMode  machineapi.RebootRequest_Mode
		expectedEtcd  string
		expectedError string
		expectedCalls []string
	}{
		{
			name:          "control plane with healthy quorum",
			fake:          &talostest.FakeClient{EtcdMembers: threeEtcd(), ActorID: "actor-1"},
			in:            tools.NodeRebootInput{Cluster: "staging", Node: "10.0.0.11"},
			expectedEtcd:  "etcd has 3 voting members, 3 healthy; 2 would stay healthy, quorum needs 2",
			expectedCalls: []string{"MemberList@", "Status@10.0.0.11", "Status@10.0.0.12", "Status@10.0.0.13"},
		},
		{
			name: "control plane refused when another member is down",
			fake: &talostest.FakeClient{
				EtcdMembers:   threeEtcd(),
				EtcdUnhealthy: map[string]error{"10.0.0.12": status.Error(codes.Unavailable, "connection refused")},
			},
			in: tools.NodeRebootInput{Cluster: "staging", Node: "10.0.0.11"},
			expectedError: "refusing to reboot 10.0.0.11: it would break etcd quorum: etcd has 3 voting members, 2 healthy; " +
				"1 would stay healthy, quorum needs 2 (unhealthy: cp-2: status: connection refused)",
		},
		{
			name: "member status errors count as unhealthy",
			fake: &talostest.FakeClient{
				EtcdMembers: threeEtcd(),
				EtcdErrors:  map[string][]string{"10.0.0.13": {"NOSPACE"}},
			},
			in:            tools.NodeRebootInput{Cluster: "staging", Node: "10.0.0.11"},
			expectedError: "refusing to reboot 10.0.0.11: it would break etcd quorum: etcd has 3 voting members, 2 healthy; 1 would stay healthy, quorum needs 2 (unhealthy: cp-3: NOSPACE)",
		},
		{
			name: "an unhealthy target can be rebooted",
			fake: &talostest.FakeClient{
				EtcdMembers:   threeEtcd(),
				EtcdUnhealthy: map[string]error{"10.0.0.11": errors.New("down")},
			},
			in:           tools.NodeRebootInput{Cluster: "staging", Node: "10.0.0.11"},
			expectedEtcd: "etcd has 3 voting members, 2 healthy; 2 would stay healthy, quorum needs 2 (unhealthy: cp-1: status: down)",
		},
		{
			name:          "single control plane is refused",
			fake:          &talostest.FakeClient{EtcdMembers: []*machineapi.EtcdMember{etcdMember("cp-1", "10.0.0.11", false)}},
			in:            tools.NodeRebootInput{Cluster: "staging", Node: "10.0.0.11"},
			expectedError: "refusing to reboot 10.0.0.11: it would break etcd quorum: etcd has 1 voting members, 1 healthy; 0 would stay healthy, quorum needs 1",
		},
		{
			name: "learner does not affect quorum",
			fake: &talostest.FakeClient{EtcdMembers: append(threeEtcd()[:1], etcdMember("cp-4", "10.0.0.14", true))},
			in:   tools.NodeRebootInput{Cluster: "staging", Node: "10.0.0.14", Mode: "powercycle"},

			expectedMode:  machineapi.RebootRequest_POWERCYCLE,
			expectedEtcd:  "target is an etcd learner; quorum is not affected",
			expectedCalls: []string{"MemberList@"},
		},
		{
			name:          "node outside etcd is rebooted without a quorum check",
			fake:          &talostest.FakeClient{EtcdMembers: threeEtcd()},
			in:            tools.NodeRebootInput{Cluster: "dev", Node: "10.0.0.21"},
			expectedCalls: []string{"MemberList@"},
		},
		{
			name:          "unknown etcd membership refuses",
			fake:          &talostest.FakeClient{EtcdMemberListErr: status.Error(codes.Unavailable, "connection refused")},
			in:            tools.NodeRebootInput{Cluster: "staging", Node: "10.0.0.21"},
			expectedError: "refusing to reboot 10.0.0.21: cannot verify etcd quorum: cluster staging: endpoint 10.0.0.10 unreachable: connection refused",
		},
		{
			name:          "reboot error is mapped",
			fake:          &talostest.FakeClient{EtcdMembers: threeEtcd(), RebootErr: status.Error(codes.PermissionDenied, "denied")},
			in:            tools.NodeRebootInput{Cluster: "staging", Node: "10.0.0.21"},
			expectedError: "cluster staging: credential role operator lacks permission for Reboot",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tt, _ := newRebootTools(t, tc.fake, true, rebootContexts()...)

			result, err := tt.NodeReboot(t.Context(), tc.in)
			if tc.expectedError != "" {
				require.EqualError(t, err, tc.expectedError)

				if tc.fake.RebootErr == nil {
					assert.Empty(t, tc.fake.Reboots(), "a refused reboot is never sent")
				}

				return
			}

			require.NoError(t, err)
			assert.True(t, result.Accepted)
			assert.Equal(t, tc.in.Node, result.Node)
			assert.Equal(t, tc.expectedEtcd, result.Etcd)
			assert.Equal(t, tc.fake.ActorID, result.ActorID)
			assert.Equal(t, []talostest.RebootCall{{Node: tc.in.Node, Mode: tc.expectedMode}}, tc.fake.Reboots())

			if tc.expectedCalls != nil {
				assert.Equal(t, tc.expectedCalls, tc.fake.EtcdCalls)
			}
		})
	}
}

func TestNodeRebootWorkerByHostnameSkipsEtcd(t *testing.T) {
	st := emptyState()
	member := cluster.NewMember(cluster.NamespaceName, "worker-1")
	member.TypedSpec().Hostname = "worker-1"
	member.TypedSpec().MachineType = machine.TypeWorker
	member.TypedSpec().Addresses = []netip.Addr{netip.MustParseAddr("10.0.0.21")}
	require.NoError(t, st.Create(t.Context(), member))

	fake := &talostest.FakeClient{St: st, EtcdMemberListErr: errors.New("must not be called")}
	tt, _ := newRebootTools(t, fake, true, rebootContexts()...)

	result, err := tt.NodeReboot(t.Context(), tools.NodeRebootInput{Cluster: "staging", Node: "worker-1"})
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.21", result.Node)
	assert.Equal(t, "worker-1", result.Hostname)
	assert.Empty(t, result.Etcd)
	assert.Empty(t, fake.EtcdCalls, "a known worker needs no quorum check")
	assert.Equal(t, []talostest.RebootCall{{Node: "10.0.0.21"}}, fake.Reboots())
}
