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
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/state"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/sergelogvinov/talos-mcp/internal/talos/talostest"
	"github.com/sergelogvinov/talos-mcp/internal/tools"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/cluster"
	"github.com/siderolabs/talos/pkg/machinery/resources/k8s"
	talosruntime "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// nodeState is a node's COSI state with its MachineStatus and, when
// kubelet is set, its KubeletStatus and the cluster Info.
func nodeState(t *testing.T, stage talosruntime.MachineStage, ready bool, kubelet string, unmet ...talosruntime.UnmetCondition) state.State {
	t.Helper()

	st := emptyState()

	ms := talosruntime.NewMachineStatus()
	ms.TypedSpec().Stage = stage
	ms.TypedSpec().Status.Ready = ready
	ms.TypedSpec().Status.UnmetConditions = unmet
	require.NoError(t, st.Create(t.Context(), ms))

	if kubelet != "" {
		ks := k8s.NewKubeletStatus(k8s.NamespaceName, k8s.KubeletID)
		ks.TypedSpec().Image = kubelet
		require.NoError(t, st.Create(t.Context(), ks))

		info := cluster.NewInfo()
		info.TypedSpec().ClusterName = "prod-cluster"
		require.NoError(t, st.Create(t.Context(), info))
	}

	return st
}

// describeFake has cp-1 healthy, worker-1 up but not ready with a failing
// service, and worker-2 without a usable address.
func describeFake(t *testing.T) *talostest.FakeClient {
	t.Helper()

	boot := uint64(now.Add(-(51*time.Hour + 4*time.Minute)).Unix()) //nolint:gosec

	return &talostest.FakeClient{
		St: membersState(t),
		Nodes: map[string]*talostest.FakeNode{
			"10.0.0.11": {
				Version: "v1.14.2",
				Services: []*machineapi.ServiceInfo{
					{Id: "etcd", State: "Running", Health: &machineapi.ServiceHealth{Healthy: true}},
					{Id: "kubelet", State: "Running", Health: &machineapi.ServiceHealth{Healthy: true}},
				},
				BootTime: boot, CPUs: 4, MemTotalKiB: 8 << 20, MemAvailableKiB: 6 << 20,
				St: nodeState(t, talosruntime.MachineStageRunning, true, "ghcr.io/siderolabs/kubelet:v1.34.1"),
			},
			"10.0.0.21": {
				Version: "v1.14.1",
				Services: []*machineapi.ServiceInfo{
					{Id: "kubelet", State: "Running", Health: &machineapi.ServiceHealth{LastMessage: "token=abcdefghijklmnopqrstuvwx failed"}},
					{Id: "cri", State: "Waiting"},
				},
				BootTime: uint64(now.Add(-5 * time.Minute).Unix()), CPUs: 2, MemTotalKiB: 4 << 20, MemAvailableKiB: 3 << 20, //nolint:gosec
				St: nodeState(t, talosruntime.MachineStageBooting, false, "", talosruntime.UnmetCondition{Name: "nodeReady", Reason: "node not ready"}),
			},
		},
		EtcdMembers: []*machineapi.EtcdMember{{Hostname: "cp-1", Id: 0xabc123}},
	}
}

func TestClustersDescribe(t *testing.T) {
	tt := newNodeTools(t, describeFake(t), nodeContexts()...)

	result, err := tt.ClustersDescribe(t.Context(), "")
	require.NoError(t, err)

	assert.Equal(t, &tools.ClustersDescribeResult{
		Cluster:           "prod",
		ClusterName:       "prod-cluster",
		KubernetesVersion: "v1.34.1",
		Endpoints:         []string{"10.0.0.10"},
		NodeSource:        "members",
		Count:             3,
		Reachable:         2,
		Nodes: []tools.NodeSummary{
			{
				Hostname: "cp-1", Address: "10.0.0.11", Role: "controlplane", TalosVersion: "v1.14.2", KubernetesVersion: "v1.34.1",
				Reachable: true, Stage: "running", Ready: true, Uptime: "2d3h4m", Resources: "cpu=4, memory=8.0GiB (used=2.0GiB)",
			},
			{
				Hostname: "worker-1", Address: "10.0.0.21", Role: "worker", TalosVersion: "v1.14.1",
				Reachable: true, Stage: "booting", Uptime: "5m", Resources: "cpu=2, memory=4.0GiB (used=1.0GiB)",
				UnhealthyServices: []string{"cri (Waiting)", "kubelet (unhealthy: token=*** failed)"},
				UnmetConditions:   []string{"nodeReady: node not ready"},
			},
			{Hostname: "worker-2", Role: "worker"},
		},
		Etcd:     []tools.EtcdMemberSummary{{Hostname: "cp-1", ID: "abc123"}},
		Warnings: []string{"node worker-2 in cluster prod has no usable address, skipped"},
	}, result)
}

func TestClustersDescribeGolden(t *testing.T) {
	tt := newNodeTools(t, describeFake(t), nodeContexts()...)

	res, text := callText(t, tt, tools.ToolClustersDescribe, nil)
	require.False(t, res.IsError, text)
	golden(t, "clusters_describe", text)
}

func TestClustersDescribePartialFailure(t *testing.T) {
	fake := describeFake(t)
	fake.Nodes["10.0.0.21"] = &talostest.FakeNode{Err: status.Error(codes.Unavailable, "connection refused")}
	fake.EtcdMemberListErr = status.Error(codes.Unavailable, "etcd is down")

	tt := newNodeTools(t, fake, nodeContexts()...)

	result, err := tt.ClustersDescribe(t.Context(), "")
	require.NoError(t, err)

	assert.Equal(t, 1, result.Reachable)
	assert.Equal(t, tools.NodeSummary{Hostname: "worker-1", Address: "10.0.0.21", Role: "worker"}, result.Nodes[1],
		"an unreachable node keeps only its Members data")
	assert.True(t, result.Nodes[0].Reachable)
	assert.Empty(t, result.Etcd)
	assert.Equal(t, []string{
		"node worker-2 in cluster prod has no usable address, skipped",
		"node worker-1 (10.0.0.21): unreachable: connection refused",
		"etcd members (through 10.0.0.11): etcd is down",
	}, result.Warnings)
}

func TestClustersDescribeEndpointsDown(t *testing.T) {
	fake := describeFake(t)
	fake.APIErr = status.Error(codes.Unavailable, "connection refused")

	tt := newNodeTools(t, fake, nodeContexts()...)

	_, err := tt.ClustersDescribe(t.Context(), "")
	require.EqualError(t, err, "cluster prod: endpoint 10.0.0.10 unreachable: connection refused")
}

func TestClustersDescribeNodesDownEndpointUp(t *testing.T) {
	fake := describeFake(t)
	fake.Nodes["10.0.0.11"].Err = status.Error(codes.Unavailable, "connection refused")
	fake.Nodes["10.0.0.21"].Err = status.Error(codes.Unavailable, "connection refused")

	tt := newNodeTools(t, fake, nodeContexts()...)

	_, err := tt.ClustersDescribe(t.Context(), "")
	require.EqualError(t, err, "cluster prod: no node answered: node cp-1 (10.0.0.11): unreachable: connection refused; "+
		"node worker-1 (10.0.0.21): unreachable: connection refused",
		"the endpoint answered the Members read, so it is not blamed")
}

func TestClustersDescribeDuplicateAddress(t *testing.T) {
	fake := describeFake(t)
	fake.St = emptyState()

	tt := newNodeTools(t, fake, talostest.Context{
		Name: "prod", Roles: []string{"os:reader"}, Endpoints: []string{"10.0.0.10"}, Nodes: []string{"10.0.0.11", "10.0.0.11:50000"},
	})

	result, err := tt.ClustersDescribe(t.Context(), "")
	require.NoError(t, err)

	require.Len(t, result.Nodes, 1, "both talosconfig entries are the same node")
	assert.True(t, result.Nodes[0].Reachable)
	assert.Equal(t, []string{"cluster prod: apid returned no Members, using talosconfig addresses"}, result.Warnings)
}

func TestClustersDescribeTalosconfigFallback(t *testing.T) {
	fake := describeFake(t)
	fake.St = emptyState()
	fake.Nodes["10.0.0.21"].St = emptyState()

	tt := newNodeTools(t, fake, nodeContexts()...)

	result, err := tt.ClustersDescribe(t.Context(), "")
	require.NoError(t, err)

	assert.Equal(t, "talosconfig", result.NodeSource)
	require.Len(t, result.Nodes, 1)
	assert.Equal(t, "10.0.0.21", result.Nodes[0].Address)
	assert.Empty(t, result.Nodes[0].Role)
	assert.Empty(t, result.Etcd, "no control plane node is known, so etcd is skipped")
	assert.Equal(t, []string{"cluster prod: apid returned no Members, using talosconfig addresses"}, result.Warnings,
		"missing resources on older Talos versions are not warnings")
}

func TestClustersDescribeNoDiscovery(t *testing.T) {
	discovery := &talostest.FakeDiscovery{Affiliates: affiliatesFixture(t)}
	dialer, dialed := talostest.StartFakeDiscovery(t, discovery)

	tc, err := config.ParseTalosConfig([]byte(talostest.TalosConfig(t, now, nodeContexts()...)), "")
	require.NoError(t, err)

	fake := describeFake(t)
	pool, err := talos.NewPool(tc,
		talos.WithClock(func() time.Time { return now }),
		talos.WithDiscoveryDialer(dialer),
		talos.WithClientFactory(func(context.Context, *clientconfig.Config, string) (talos.Client, error) { return fake, nil }),
	)
	require.NoError(t, err)

	t.Cleanup(func() { pool.Close() }) //nolint:errcheck

	tt := tools.NewTalosTools(pool, false, allExtensions())
	require.NotNil(t, pool.Discovery("prod"), "prod has discovery keys")

	res, text := callText(t, tt, tools.ToolClustersDescribe, map[string]any{"cluster": "prod"})
	require.False(t, res.IsError, text)

	assert.Zero(t, discovery.Calls("List"))
	assert.Empty(t, *dialed)
	assert.NotContains(t, text, "discovery")
	assert.NotContains(t, text, "prod-id")
}
