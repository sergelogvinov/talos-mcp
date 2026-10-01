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
)

// Client is the part of the Talos client the tools use (design §14). The pool
// hands it out instead of *client.Client, so tool tests can use a fake.
type Client interface {
	Version(ctx context.Context, callOptions ...grpc.CallOption) (*machineapi.VersionResponse, error)
	ServiceList(ctx context.Context, callOptions ...grpc.CallOption) (*machineapi.ServiceListResponse, error)
	Logs(ctx context.Context, namespace string, driver common.ContainerDriver, id string, follow bool, tailLines int32) (machineapi.MachineService_LogsClient, error)
	Dmesg(ctx context.Context, follow, tail bool) (machineapi.MachineService_DmesgClient, error)
	EventsWatchV2(ctx context.Context, ch chan<- client.EventResult, opts ...client.EventsOptionFunc) error
	Reboot(ctx context.Context, opts ...client.RebootMode) error
	EtcdMemberList(ctx context.Context, req *machineapi.EtcdMemberListRequest, callOptions ...grpc.CallOption) (*machineapi.EtcdMemberListResponse, error)
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

// NewTalosClient is the default ClientFactory. It does not dial: the gRPC
// connection is established on the first call.
func NewTalosClient(ctx context.Context, cfg *clientconfig.Config, contextName string) (Client, error) {
	c, err := client.New(ctx, client.WithConfig(cfg), client.WithContextName(contextName))
	if err != nil {
		return nil, err
	}

	return talosClient{Client: c}, nil
}
