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

package talos

import (
	"context"

	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Client is the part of the Talos client the tools use (design §14). The pool
// hands it out instead of *client.Client, so tool tests can use a fake.
type Client interface { //nolint:interfacebloat // mirrors the client.Client methods the tools call
	Version(ctx context.Context, callOptions ...grpc.CallOption) (*machineapi.VersionResponse, error)
	ServiceList(ctx context.Context, callOptions ...grpc.CallOption) (*machineapi.ServiceListResponse, error)
	SystemStat(ctx context.Context, callOptions ...grpc.CallOption) (*machineapi.SystemStatResponse, error)
	Memory(ctx context.Context, callOptions ...grpc.CallOption) (*machineapi.MemoryResponse, error)
	LoadAvg(ctx context.Context, callOptions ...grpc.CallOption) (*machineapi.LoadAvgResponse, error)
	Mounts(ctx context.Context, callOptions ...grpc.CallOption) (*machineapi.MountsResponse, error)
	Processes(ctx context.Context, callOptions ...grpc.CallOption) (*machineapi.ProcessesResponse, error)
	Logs(ctx context.Context, namespace string, driver common.ContainerDriver, id string, follow bool, tailLines int32) (machineapi.MachineService_LogsClient, error)
	Dmesg(ctx context.Context, follow, tail bool) (machineapi.MachineService_DmesgClient, error)
	// Events is the raw stream, not EventsWatchV2: that one ends the stream
	// on the first event type this machinery version doesn't know.
	Events(ctx context.Context, opts ...client.EventsOptionFunc) (machineapi.MachineService_EventsClient, error)
	RebootWithResponse(ctx context.Context, opts ...client.RebootMode) (*machineapi.RebootResponse, error)
	EtcdMemberList(ctx context.Context, req *machineapi.EtcdMemberListRequest, callOptions ...grpc.CallOption) (*machineapi.EtcdMemberListResponse, error)
	EtcdStatus(ctx context.Context, callOptions ...grpc.CallOption) (*machineapi.EtcdStatusResponse, error)
	// State is the COSI state, for resource Get/List (client.Client.COSI).
	State() state.State
	Close() error
}

// ClientFactory creates a Talos client for one talosconfig context.
type ClientFactory func(ctx context.Context, cfg *clientconfig.Config, contextName string) (Client, error)

// talosClient adapts *client.Client to Client.
type talosClient struct {
	*client.Client
}

// State returns the client's COSI state.
func (c talosClient) State() state.State {
	return c.COSI
}

// SystemStat calls the SystemStat API; machinery has no wrapper for it. Like
// the wrappers, it moves per-node errors from the reply into the error.
func (c talosClient) SystemStat(ctx context.Context, callOptions ...grpc.CallOption) (*machineapi.SystemStatResponse, error) {
	return client.FilterMessages(c.MachineClient.SystemStat(ctx, &emptypb.Empty{}, callOptions...))
}

// LoadAvg calls the LoadAvg API; machinery has no wrapper for it either.
func (c talosClient) LoadAvg(ctx context.Context, callOptions ...grpc.CallOption) (*machineapi.LoadAvgResponse, error) {
	return client.FilterMessages(c.MachineClient.LoadAvg(ctx, &emptypb.Empty{}, callOptions...))
}

// NewTalosClient is the default ClientFactory. It does not dial: the gRPC
// connection is established on the first call.
func NewTalosClient(ctx context.Context, cfg *clientconfig.Config, contextName string) (Client, error) {
	c, err := client.New(ctx, client.WithConfig(cfg), client.WithContextName(contextName))
	if err != nil {
		return nil, err
	}

	return talosClient{Client: c}, nil
}
