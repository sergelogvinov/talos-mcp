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
	"io"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/state"
	"github.com/sergelogvinov/talos-mcp/internal/talos/talostest"
	"github.com/sergelogvinov/talos-mcp/internal/tools"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/resources/cluster"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
)

var eventsAt = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

// eventID is the ID of event seq, sec seconds after eventsAt.
func eventID(sec, seq int) string {
	return talostest.EventID(eventsAt.Add(time.Duration(sec)*time.Second), seq)
}

// membersState serves Members for a control plane node, a worker, and a
// worker with only a link-local address.
func membersState(t *testing.T) state.State {
	t.Helper()

	st := emptyState()

	for _, m := range []struct {
		host  string
		typ   machine.Type
		addrs []string
	}{
		{"cp-1", machine.TypeControlPlane, []string{"10.0.0.11"}},
		{"worker-1", machine.TypeWorker, []string{"10.0.0.21"}},
		{"worker-2", machine.TypeWorker, []string{"fe80::1"}},
	} {
		member := cluster.NewMember(cluster.NamespaceName, m.host)
		member.TypedSpec().Hostname = m.host
		member.TypedSpec().MachineType = m.typ

		for _, a := range m.addrs {
			member.TypedSpec().Addresses = append(member.TypedSpec().Addresses, netip.MustParseAddr(a))
		}

		require.NoError(t, st.Create(t.Context(), member))
	}

	return st
}

// clusterEvents is the backlog of cp-1 (which ends its stream) and worker-1
// (which keeps it open, as machined does, so its read ends on idle).
func clusterEvents(t *testing.T) *talostest.FakeClient {
	t.Helper()

	at := func(s int) time.Time { return eventsAt.Add(time.Duration(s) * time.Second) }

	return &talostest.FakeClient{
		St: membersState(t),
		EventLog: map[string][]*machineapi.Event{
			"10.0.0.11": {
				talostest.Event(t, at(0), 1, "", &machineapi.SequenceEvent{Sequence: "boot", Action: machineapi.SequenceEvent_START}),
				talostest.Event(t, at(1), 2, "", &machineapi.PhaseEvent{Phase: "network", Action: machineapi.PhaseEvent_STOP}),
				talostest.Event(t, at(2), 3, "", &machineapi.ServiceStateEvent{
					Service: "etcd", Action: machineapi.ServiceStateEvent_RUNNING, Message: "Health check successful",
					Health: &machineapi.ServiceHealth{Healthy: true},
				}),
				talostest.Event(t, at(5), 4, "actor-1", &machineapi.MachineStatusEvent{
					Stage:  machineapi.MachineStatusEvent_RUNNING,
					Status: &machineapi.MachineStatusEvent_MachineStatus{Ready: true},
				}),
			},
			"10.0.0.21": {
				talostest.Event(t, at(3), 1, "", &machineapi.ServiceStateEvent{
					Service: "kubelet", Action: machineapi.ServiceStateEvent_FAILED, Message: "Condition failed",
					Health: &machineapi.ServiceHealth{LastMessage: "connection refused"},
				}),
				talostest.Event(t, at(4), 2, "", &machineapi.ConfigLoadErrorEvent{Error: "bad config:\n  password=hunter2"}),
				talostest.Event(t, at(6), 3, "", &machineapi.MachineStatusEvent{
					Stage: machineapi.MachineStatusEvent_BOOTING,
					Status: &machineapi.MachineStatusEvent_MachineStatus{UnmetConditions: []*machineapi.MachineStatusEvent_MachineStatus_UnmetCondition{
						{Name: "nodeReady", Reason: "node not ready"},
					}},
				}),
			},
		},
		EventsEnd: map[string]error{"10.0.0.11": io.EOF},
	}
}

func TestClustersEvent(t *testing.T) {
	fake := clusterEvents(t)
	tt := newNodeTools(t, fake, nodeContexts()...)

	started := time.Now()

	result, err := tt.ClustersEvent(t.Context(), tools.ClustersEventInput{})
	require.NoError(t, err)

	assert.Less(t, time.Since(started), 5*time.Second, "an open stream ends on the idle timeout")

	assert.Equal(t, "prod", result.Cluster)
	assert.Equal(t, "1h0m0s", result.Since)
	assert.Equal(t, 2, result.Nodes)
	assert.Equal(t, 7, result.Count)
	assert.False(t, result.Truncated)
	assert.Equal(t, []string{"node worker-2 in cluster prod has no usable address, skipped"}, result.Warnings)

	assert.Equal(t, []tools.EventSummary{
		{
			Time: "2026-10-01T10:00:06Z", Node: "10.0.0.21", NodeName: "worker-1", Type: "machine_status",
			Summary: "stage booting, not ready: nodeReady (node not ready)", ID: eventID(6, 3),
		},
		{Time: "2026-10-01T10:00:05Z", Node: "10.0.0.11", NodeName: "cp-1", Type: "machine_status", Summary: "stage running, ready", ActorID: "actor-1", ID: eventID(5, 4)},
		{Time: "2026-10-01T10:00:04Z", Node: "10.0.0.21", NodeName: "worker-1", Type: "config_load_error", Summary: "bad config: password=***", ID: eventID(4, 2)},
		{
			Time: "2026-10-01T10:00:03Z", Node: "10.0.0.21", NodeName: "worker-1", Type: "service",
			Summary: "kubelet failed: Condition failed (unhealthy: connection refused)", ID: eventID(3, 1),
		},
		{Time: "2026-10-01T10:00:02Z", Node: "10.0.0.11", NodeName: "cp-1", Type: "service", Summary: "etcd running: Health check successful (healthy)", ID: eventID(2, 3)},
		{Time: "2026-10-01T10:00:01Z", Node: "10.0.0.11", NodeName: "cp-1", Type: "phase", Summary: "network stop", ID: eventID(1, 2)},
		{Time: "2026-10-01T10:00:00Z", Node: "10.0.0.11", NodeName: "cp-1", Type: "sequence", Summary: "boot start", ID: eventID(0, 1)},
	}, result.Events)

	calls := fake.EventsRequests()
	nodes := make([]string, 0, len(calls))

	for _, c := range calls {
		nodes = append(nodes, c.Node)
		assert.Equal(t, int32(3600), c.Request.GetTailSeconds())
		assert.Empty(t, c.Request.GetWithActorId())
	}

	slices.Sort(nodes)
	assert.Equal(t, []string{"10.0.0.11", "10.0.0.21"}, nodes, "one stream per node with an address")
}

func TestClustersEventGolden(t *testing.T) {
	tt := newNodeTools(t, clusterEvents(t), nodeContexts()...)

	res, text := callText(t, tt, tools.ToolClustersEvent, map[string]any{"limit": 5})
	require.False(t, res.IsError, text)
	golden(t, "clusters_event", text)
}

func TestClustersEventOneNode(t *testing.T) {
	fake := clusterEvents(t)
	tt := newNodeTools(t, fake, nodeContexts()...)

	result, err := tt.ClustersEvent(t.Context(), tools.ClustersEventInput{Node: "cp-1", Since: "15m", Limit: 2, ActorID: "actor-1"})
	require.NoError(t, err)

	assert.Equal(t, 1, result.Nodes)
	assert.True(t, result.Truncated)
	require.Len(t, result.Events, 2)
	assert.Equal(t, "machine_status", result.Events[0].Type)
	assert.Equal(t, "service", result.Events[1].Type)

	calls := fake.EventsRequests()
	require.Len(t, calls, 1)
	assert.Equal(t, "10.0.0.11", calls[0].Node)
	assert.Equal(t, int32(900), calls[0].Request.GetTailSeconds())
	assert.Equal(t, "actor-1", calls[0].Request.GetWithActorId())
}

func TestClustersEventTalosconfigFallback(t *testing.T) {
	fake := &talostest.FakeClient{
		St:        emptyState(),
		EventLog:  map[string][]*machineapi.Event{"10.0.0.21": {talostest.Event(t, eventsAt, 1, "", &machineapi.TaskEvent{Task: "upgrade", Action: machineapi.TaskEvent_START})}},
		EventsEnd: map[string]error{"10.0.0.21": io.EOF},
	}
	tt := newNodeTools(t, fake, nodeContexts()...)

	result, err := tt.ClustersEvent(t.Context(), tools.ClustersEventInput{})
	require.NoError(t, err)

	assert.Equal(t, []string{"cluster prod: apid returned no Members, using talosconfig addresses"}, result.Warnings)
	require.Len(t, result.Events, 1)
	assert.Equal(t, tools.EventSummary{Time: "2026-10-01T10:00:00Z", Node: "10.0.0.21", Type: "task", Summary: "upgrade start", ID: talostest.EventID(eventsAt, 1)}, result.Events[0])
}

func TestClustersEventNodeErrors(t *testing.T) {
	unsupported := &machineapi.Event{Id: talostest.EventID(eventsAt, 9), Data: &anypb.Any{TypeUrl: "talos/runtime/machine.FutureEvent"}}

	fake := clusterEvents(t)
	fake.EventLog["10.0.0.11"] = []*machineapi.Event{unsupported}
	fake.EventsErr = map[string]error{"10.0.0.21": status.Error(codes.Unavailable, "connection refused")}

	tt := newNodeTools(t, fake, nodeContexts()...)

	result, err := tt.ClustersEvent(t.Context(), tools.ClustersEventInput{})
	require.NoError(t, err)

	assert.Contains(t, result.Warnings, "node worker-1 (10.0.0.21): reading events failed: connection refused")
	require.Len(t, result.Events, 1)
	assert.Equal(t, "FutureEvent", result.Events[0].Type)
	assert.Equal(t, "event not supported by this server version", result.Events[0].Summary)
}

func TestClustersEventMetadataError(t *testing.T) {
	fake := clusterEvents(t)
	// apid reports a node failure in the deprecated Metadata.Error.
	fake.EventLog["10.0.0.21"] = []*machineapi.Event{{Metadata: &common.Metadata{Error: "node is down"}}} //nolint:staticcheck

	tt := newNodeTools(t, fake, nodeContexts()...)

	result, err := tt.ClustersEvent(t.Context(), tools.ClustersEventInput{})
	require.NoError(t, err)

	assert.Contains(t, result.Warnings, "node worker-1 (10.0.0.21): reading events failed: node is down")
	assert.Equal(t, 4, result.Count, "cp-1 events are still returned")
}

func TestClustersEventAllNodesFail(t *testing.T) {
	fake := clusterEvents(t)
	fake.EventsErr = map[string]error{
		"10.0.0.11": status.Error(codes.PermissionDenied, "denied"),
		"10.0.0.21": status.Error(codes.PermissionDenied, "denied"),
	}

	tt := newNodeTools(t, fake, nodeContexts()...)

	_, err := tt.ClustersEvent(t.Context(), tools.ClustersEventInput{})
	require.EqualError(t, err, "cluster prod: credential role reader lacks permission for Events")
}

func TestClustersEventInvalidInput(t *testing.T) {
	tt := newNodeTools(t, &talostest.FakeClient{}, nodeContexts()...)

	for _, tc := range []struct {
		in            tools.ClustersEventInput
		expectedError string
	}{
		{tools.ClustersEventInput{Since: "yesterday"}, `invalid since "yesterday": must be a duration between 1s and 720h0m0s, e.g. 15m or 2h`},
		{tools.ClustersEventInput{Since: "-1h"}, `invalid since "-1h": must be a duration between 1s and 720h0m0s, e.g. 15m or 2h`},
		{tools.ClustersEventInput{Limit: 501}, "invalid limit 501: must be between 1 and 500"},
		{tools.ClustersEventInput{Cluster: "nope"}, `unknown cluster "nope": valid clusters are prod, staging`},
	} {
		_, err := tt.ClustersEvent(t.Context(), tc.in)
		require.EqualError(t, err, tc.expectedError)
	}

	assert.Empty(t, (&talostest.FakeClient{}).EventsRequests())
}

func TestClustersEventSanitizesMultiLineErrors(t *testing.T) {
	fake := clusterEvents(t)
	fake.EventLog["10.0.0.11"] = []*machineapi.Event{talostest.Event(t, eventsAt, 1, "", &machineapi.ConfigValidationErrorEvent{
		Error: "invalid config:\nmachine:\n  registries:\n    password: hunter2pass\ncluster:\n  secret: mysecretvalue",
	})}

	tt := newNodeTools(t, fake, nodeContexts()...)

	result, err := tt.ClustersEvent(t.Context(), tools.ClustersEventInput{Node: "10.0.0.11"})
	require.NoError(t, err)
	require.Len(t, result.Events, 1)

	summary := result.Events[0].Summary
	assert.NotContains(t, summary, "hunter2pass")
	assert.NotContains(t, summary, "mysecretvalue")
	assert.NotContains(t, summary, "\n", "the summary is one line")
}

func TestClustersEventSilentNode(t *testing.T) {
	tools.SetEventTimings(t, 100*time.Millisecond, 100*time.Millisecond, 8)

	fake := clusterEvents(t)
	fake.EventsSilent = map[string]bool{"10.0.0.21": true}

	tt := newNodeTools(t, fake, nodeContexts()...)

	result, err := tt.ClustersEvent(t.Context(), tools.ClustersEventInput{})
	require.NoError(t, err)

	assert.Contains(t, result.Warnings, "node worker-1 (10.0.0.21): reading events failed: no answer within 100ms; the node may be down or rebooting")
	assert.Equal(t, 4, result.Count, "cp-1 events are still returned")
}

func TestClustersEventAllNodesSilent(t *testing.T) {
	tools.SetEventTimings(t, 100*time.Millisecond, 100*time.Millisecond, 8)

	fake := clusterEvents(t)
	fake.EventsSilent = map[string]bool{"10.0.0.11": true, "10.0.0.21": true}

	tt := newNodeTools(t, fake, nodeContexts()...)

	_, err := tt.ClustersEvent(t.Context(), tools.ClustersEventInput{})
	require.EqualError(t, err, "cluster prod: no node answered: "+
		"node cp-1 (10.0.0.11): reading events failed: no answer within 100ms; the node may be down or rebooting; "+
		"node worker-1 (10.0.0.21): reading events failed: no answer within 100ms; the node may be down or rebooting")
}

func TestClustersEventDeadlineSkipsWaitingNodes(t *testing.T) {
	tools.SetEventTimings(t, time.Minute, time.Minute, 1)

	fake := clusterEvents(t)
	fake.EventsSilent = map[string]bool{"10.0.0.11": true, "10.0.0.21": true}

	tt := newNodeTools(t, fake, nodeContexts()...)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err := tt.ClustersEvent(ctx, tools.ClustersEventInput{})
	require.ErrorContains(t, err, "not read, the call's time limit was reached")
	assert.Len(t, fake.EventsRequests(), 1, "the node waiting for a slot is never read")
}

func TestClustersEventDuplicateAddress(t *testing.T) {
	fake := &talostest.FakeClient{
		St:        emptyState(),
		EventLog:  map[string][]*machineapi.Event{"10.0.0.11": {talostest.Event(t, eventsAt, 1, "", &machineapi.TaskEvent{Task: "upgrade"})}},
		EventsEnd: map[string]error{"10.0.0.11": io.EOF},
	}

	tt := newNodeTools(t, fake, talostest.Context{
		Name: "prod", Roles: []string{"os:reader"}, Endpoints: []string{"10.0.0.10"}, Nodes: []string{"10.0.0.11", "10.0.0.11:50000"},
	})

	result, err := tt.ClustersEvent(t.Context(), tools.ClustersEventInput{})
	require.NoError(t, err)

	assert.Equal(t, 1, result.Nodes)
	assert.Equal(t, 1, result.Count, "the node is read once")
	assert.Len(t, fake.EventsRequests(), 1)
}

func TestNodeRebootHintUsesEvents(t *testing.T) {
	fake := &talostest.FakeClient{EtcdMembers: threeEtcd(), ActorID: "actor-1"}
	tt, _ := newRebootTools(t, fake, true, rebootContexts()...)
	newServer(t, tt)

	result, err := tt.NodeReboot(t.Context(), tools.NodeRebootInput{Cluster: "staging", Node: "10.0.0.21"})
	require.NoError(t, err)
	assert.Contains(t, result.Hint, tools.ToolClustersEvent+" cluster=staging node=10.0.0.21")
}
