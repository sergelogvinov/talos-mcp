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
	"io"
	"testing"
	"time"

	"github.com/sergelogvinov/talos-mcp/internal/talos/talostest"
	"github.com/sergelogvinov/talos-mcp/internal/tools"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// nodeDescribeFake is describeFake with worker-1 (the default node of prod)
// filled in: load, disks, processes, service events, logs and events.
func nodeDescribeFake(t *testing.T) *talostest.FakeClient {
	t.Helper()

	fake := describeFake(t)

	n := fake.Nodes["10.0.0.21"]
	n.Arch, n.Platform = "amd64", "metal"
	n.Load = [3]float64{0.52, 0.48, 0.4}
	n.Mounts = []*machineapi.MountStat{
		{Filesystem: "/dev/sda6", MountedOn: "/var", Size: 100 << 30, Available: 75 << 30},
		{Filesystem: "/dev/sda6", MountedOn: "/var/lib/kubelet/pods/x", Size: 100 << 30, Available: 75 << 30},
		{Filesystem: "/dev/sda5", MountedOn: "/system/state", Size: 100 << 20, Available: 90 << 20},
		{Filesystem: "tmpfs", MountedOn: "/run", Size: 1 << 30, Available: 1 << 30},
	}
	n.Processes = []*machineapi.ProcessInfo{
		{Pid: 1, Command: "machined", ResidentMemory: 80 << 20, CpuTime: 12.4},
		{Pid: 900, Command: "kubelet", ResidentMemory: 120 << 20, CpuTime: 300},
		{Pid: 950, Command: "containerd", ResidentMemory: 60 << 20, CpuTime: 30},
	}

	n.Services[0].Events = &machineapi.ServiceEvents{Events: []*machineapi.ServiceEvent{
		{Msg: "Starting service", State: "Starting", Ts: timestamppb.New(now.Add(-10 * time.Minute))},
		{Msg: "Health check failed: token=abcdefghijklmnopqrstuvwx", State: "Running", Ts: timestamppb.New(now.Add(-2 * time.Minute))},
	}}

	fake.LogText = map[string]string{
		"kubelet":  numbered("kubelet", 30),
		"cri":      "cri waiting for network\n",
		"machined": "machined line 1\n",
	}

	fake.EventLog = map[string][]*machineapi.Event{
		"10.0.0.21": {
			talostest.Event(t, now.Add(-3*time.Minute), 1, "", &machineapi.ServiceStateEvent{
				Service: "kubelet", Action: machineapi.ServiceStateEvent_RUNNING, Message: "Started",
			}),
			talostest.Event(t, now.Add(-time.Minute), 2, "", &machineapi.MachineStatusEvent{
				Stage:  machineapi.MachineStatusEvent_BOOTING,
				Status: &machineapi.MachineStatusEvent_MachineStatus{},
			}),
		},
	}
	fake.EventsEnd = map[string]error{"10.0.0.21": io.EOF}

	mt := config.NewMachineType()
	mt.SetMachineType(machine.TypeWorker)
	require.NoError(t, n.St.Create(t.Context(), mt))

	hostname := network.NewHostnameStatus(network.NamespaceName, network.HostnameID)
	hostname.TypedSpec().Hostname = "worker-1"
	require.NoError(t, n.St.Create(t.Context(), hostname))

	return fake
}

func TestNodeDescribe(t *testing.T) {
	fake := nodeDescribeFake(t)
	tt := newNodeTools(t, fake, nodeContexts()...)

	result, err := tt.NodeDescribe(t.Context(), tools.NodeDescribeInput{LogLines: 3})
	require.NoError(t, err)

	assert.Equal(t, "prod", result.Cluster)
	assert.Equal(t, "10.0.0.21", result.Node)
	assert.Equal(t, "worker-1", result.Hostname)
	assert.Equal(t, "worker", result.Role)
	assert.Equal(t, "v1.14.1", result.TalosVersion)
	assert.Equal(t, "amd64", result.Arch)
	assert.Equal(t, "metal", result.Platform)
	assert.Equal(t, "booting", result.Stage)
	assert.False(t, result.Ready)
	assert.Equal(t, []string{"nodeReady: node not ready"}, result.UnmetConditions)
	assert.Equal(t, "5m", result.Uptime)
	assert.Empty(t, result.Warnings)

	assert.Equal(t, tools.NodeResources{
		CPUs:        2,
		LoadAverage: "0.52, 0.48, 0.40",
		Memory:      "1.0GiB of 4.0GiB (25%)",
		Processes:   "running=0, blocked=0",
		Disks: []tools.DiskUsage{
			{MountedOn: "/system/state", Device: "/dev/sda5", Size: "100.0MiB", Used: "10.0MiB", UsedPercent: "10%"},
			{MountedOn: "/var", Device: "/dev/sda6", Size: "100.0GiB", Used: "25.0GiB", UsedPercent: "25%"},
		},
		TopProcesses: []tools.ProcessUsage{
			{PID: 900, Command: "kubelet", Memory: "120.0MiB", CPUTime: "5m0s"},
			{PID: 1, Command: "machined", Memory: "80.0MiB", CPUTime: "12s"},
			{PID: 950, Command: "containerd", Memory: "60.0MiB", CPUTime: "30s"},
		},
	}, result.Resources)

	assert.Equal(t, []tools.ServiceStatus{
		{ID: "cri", State: "Waiting"},
		{
			ID: "kubelet", State: "Running", Health: "unhealthy: token=*** failed",
			LastEvent: "Health check failed: token=***", Since: "2026-10-01T11:58:00Z",
		},
	}, result.Services)

	require.Len(t, result.Events, 2)
	assert.Equal(t, "machine_status", result.Events[0].Type, "newest first")
	assert.Equal(t, "1h0m0s", result.EventsSince)

	// The unhealthy services' logs, in service order.
	require.Len(t, result.Logs, 2)
	assert.Equal(t, tools.ServiceLog{Service: "cri", Lines: []string{"cri waiting for network"}, Count: 1}, result.Logs[0])
	assert.Equal(t, "kubelet", result.Logs[1].Service)
	assert.Equal(t, 3, result.Logs[1].Count)
	assert.True(t, result.Logs[1].Truncated)
}

func TestNodeDescribeGolden(t *testing.T) {
	tt := newNodeTools(t, nodeDescribeFake(t), nodeContexts()...)

	res, text := callText(t, tt, tools.ToolNodeDescribe, map[string]any{"log_lines": 2})
	require.False(t, res.IsError, text)
	golden(t, "node_describe", text)
}

func TestNodeDescribeLogs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		healthy  bool
		logs     []string
		services []string
		warning  string
	}{
		{name: "healthy node shows machined", healthy: true, services: []string{"machined"}},
		{name: "requested", logs: []string{"kubelet", " kubelet ", "machined"}, services: []string{"kubelet", "machined"}},
		{
			name: "unknown service", logs: []string{"nope"},
			warning: `logs: unknown service "nope"; services: cri, kubelet, machined`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := nodeDescribeFake(t)

			n := fake.Nodes["10.0.0.21"]
			n.Services = append(n.Services, &machineapi.ServiceInfo{Id: "machined", State: "Running"})

			if tc.healthy {
				n.Services[0].Health = &machineapi.ServiceHealth{Healthy: true}
				n.Services[1].State = "Running"
			}

			tt := newNodeTools(t, fake, nodeContexts()...)

			result, err := tt.NodeDescribe(t.Context(), tools.NodeDescribeInput{Logs: tc.logs})
			require.NoError(t, err)

			services := make([]string, 0, len(result.Logs))
			for _, l := range result.Logs {
				services = append(services, l.Service)
			}

			assert.Equal(t, append([]string{}, tc.services...), services)

			if tc.warning != "" {
				assert.Contains(t, result.Warnings, tc.warning)
			}
		})
	}
}

func TestNodeDescribePartialFailure(t *testing.T) {
	fake := nodeDescribeFake(t)
	fake.ServiceListErr = status.Error(codes.Unavailable, "machined is busy")
	fake.LogsErr = status.Error(codes.NotFound, "log not found")
	fake.EventsErr = map[string]error{"10.0.0.21": status.Error(codes.Unavailable, "events gone")}

	tt := newNodeTools(t, fake, nodeContexts()...)

	result, err := tt.NodeDescribe(t.Context(), tools.NodeDescribeInput{})
	require.NoError(t, err)

	assert.Equal(t, []string{"services: machined is busy", "events: events gone"}, result.Warnings)
	assert.Empty(t, result.Services)
	assert.Empty(t, result.Events)
	assert.Equal(t, []tools.ServiceLog{{Service: "machined", Lines: []string{}, Error: "log not found"}}, result.Logs,
		"without a service list, machined is tried")
	assert.Equal(t, 2, result.Resources.CPUs)
}

func TestNodeDescribeUnreachable(t *testing.T) {
	fake := nodeDescribeFake(t)
	fake.Nodes["10.0.0.21"].Err = status.Error(codes.Unavailable, "connection refused")

	tt := newNodeTools(t, fake, nodeContexts()...)

	_, err := tt.NodeDescribe(t.Context(), tools.NodeDescribeInput{})
	require.EqualError(t, err, "cluster prod: endpoint 10.0.0.10 unreachable: connection refused")
	assert.Empty(t, fake.EventsRequests(), "nothing else is read from a node that doesn't answer")
}

func TestNodeDescribeInvalidInput(t *testing.T) {
	tt := newNodeTools(t, nodeDescribeFake(t), nodeContexts()...)

	for _, tc := range []struct {
		in       tools.NodeDescribeInput
		expected string
	}{
		{tools.NodeDescribeInput{LogLines: 201}, "invalid log_lines 201: must be between 1 and 200"},
		{tools.NodeDescribeInput{Logs: []string{"a", "b", "c", "d", "e", "f"}}, "invalid logs: at most 5 services, got 6"},
		{tools.NodeDescribeInput{EventsSince: "forever"}, `invalid since "forever": must be a duration between 1s and 720h0m0s, e.g. 15m or 2h`},
		{tools.NodeDescribeInput{EventLimit: -1}, "invalid limit -1: must be between 1 and 500"},
	} {
		_, err := tt.NodeDescribe(t.Context(), tc.in)
		require.EqualError(t, err, tc.expected)
	}
}
