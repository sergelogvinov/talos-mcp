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

package talostest

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// LogsCall records one Logs call of a FakeClient.
type LogsCall struct {
	Node      string
	Namespace string
	Driver    common.ContainerDriver
	ID        string
	Follow    bool
	TailLines int32
}

// FakeClient is a talos.Client for tool tests. Calls that a test doesn't
// set up return an error or panic, so unexpected network use fails loudly.
type FakeClient struct {
	// St serves COSI resources (Members) when set.
	St state.State

	// LogText maps a service or container id to its full log text. Chunk
	// splits the text into stream messages of this size (default: one).
	LogText map[string]string
	Chunk   int
	// LogsErr is returned by Logs when set; LogsStreamErr ends the stream.
	LogsErr       error
	LogsStreamErr error

	// Kmsg is the ring buffer, one message per entry, as machined sends it.
	Kmsg     []string
	DmesgErr error

	// Services are the ids returned by ServiceList.
	Services       []string
	ServiceListErr error

	// EtcdMembers is returned by EtcdMemberList, or EtcdMemberListErr.
	EtcdMembers       []*machineapi.EtcdMember
	EtcdMemberListErr error
	// EtcdUnhealthy maps a node address to the error EtcdStatus returns for
	// it; EtcdErrors to the member status errors it reports.
	EtcdUnhealthy map[string]error
	EtcdErrors    map[string][]string

	// RebootErr is returned by RebootWithResponse; ActorID is in its response.
	RebootErr error
	ActorID   string

	mu          sync.Mutex
	LogsCalls   []LogsCall
	DmesgNodes  []string
	RebootCalls []RebootCall
	EtcdCalls   []string // "MemberList@<node>" and "Status@<node>"
	closed      bool
}

// RebootCall records one RebootWithResponse call of a FakeClient.
type RebootCall struct {
	Node string
	Mode machineapi.RebootRequest_Mode
}

// Version is not used by the tool tests.
func (f *FakeClient) Version(context.Context, ...grpc.CallOption) (*machineapi.VersionResponse, error) {
	panic("talostest: Version not faked")
}

// ServiceList returns Services.
func (f *FakeClient) ServiceList(context.Context, ...grpc.CallOption) (*machineapi.ServiceListResponse, error) {
	if f.ServiceListErr != nil {
		return nil, f.ServiceListErr
	}

	services := make([]*machineapi.ServiceInfo, 0, len(f.Services))
	for _, id := range f.Services {
		services = append(services, &machineapi.ServiceInfo{Id: id, State: "Running"})
	}

	return &machineapi.ServiceListResponse{Messages: []*machineapi.ServiceList{{Services: services}}}, nil
}

// Logs streams the last tailLines lines of LogText[id] (all when negative).
func (f *FakeClient) Logs(ctx context.Context, namespace string, driver common.ContainerDriver, id string, follow bool, tailLines int32) (machineapi.MachineService_LogsClient, error) {
	f.mu.Lock()
	f.LogsCalls = append(f.LogsCalls, LogsCall{Node: nodeFromContext(ctx), Namespace: namespace, Driver: driver, ID: id, Follow: follow, TailLines: tailLines})
	f.mu.Unlock()

	if f.LogsErr != nil {
		return nil, f.LogsErr
	}

	end := io.EOF
	if f.LogsStreamErr != nil {
		end = f.LogsStreamErr
	}

	text, ok := f.LogText[id]
	if !ok {
		return &fakeStream{err: end}, nil
	}

	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if tailLines >= 0 && int(tailLines) < len(lines) {
		lines = lines[len(lines)-int(tailLines):]
	}

	data := []byte(strings.Join(lines, ""))
	chunk := f.Chunk

	if chunk <= 0 {
		chunk = len(data)
	}

	var msgs [][]byte
	for len(data) > 0 {
		n := min(chunk, len(data))
		msgs = append(msgs, data[:n])
		data = data[n:]
	}

	return &fakeStream{msgs: msgs, err: end}, nil
}

// Dmesg streams the ring buffer, one message per entry.
func (f *FakeClient) Dmesg(ctx context.Context, _, _ bool) (machineapi.MachineService_DmesgClient, error) {
	f.mu.Lock()
	f.DmesgNodes = append(f.DmesgNodes, nodeFromContext(ctx))
	f.mu.Unlock()

	if f.DmesgErr != nil {
		return nil, f.DmesgErr
	}

	msgs := make([][]byte, 0, len(f.Kmsg))
	for _, m := range f.Kmsg {
		msgs = append(msgs, []byte(m))
	}

	return &fakeStream{msgs: msgs, err: io.EOF}, nil
}

// EventsWatchV2 is not used by the tool tests yet.
func (f *FakeClient) EventsWatchV2(context.Context, chan<- client.EventResult, ...client.EventsOptionFunc) error {
	panic("talostest: EventsWatchV2 not faked")
}

// RebootWithResponse records the call and returns ActorID, or RebootErr.
func (f *FakeClient) RebootWithResponse(ctx context.Context, opts ...client.RebootMode) (*machineapi.RebootResponse, error) {
	var req machineapi.RebootRequest
	for _, opt := range opts {
		opt(&req)
	}

	f.mu.Lock()
	f.RebootCalls = append(f.RebootCalls, RebootCall{Node: nodeFromContext(ctx), Mode: req.GetMode()})
	f.mu.Unlock()

	if f.RebootErr != nil {
		return nil, f.RebootErr
	}

	return &machineapi.RebootResponse{Messages: []*machineapi.Reboot{{ActorId: f.ActorID}}}, nil
}

// EtcdMemberList returns EtcdMembers.
func (f *FakeClient) EtcdMemberList(ctx context.Context, _ *machineapi.EtcdMemberListRequest, _ ...grpc.CallOption) (*machineapi.EtcdMemberListResponse, error) {
	f.recordEtcd(ctx, "MemberList")

	if f.EtcdMemberListErr != nil {
		return nil, f.EtcdMemberListErr
	}

	return &machineapi.EtcdMemberListResponse{Messages: []*machineapi.EtcdMembers{{Members: f.EtcdMembers}}}, nil
}

// EtcdStatus reports the member status of the node in ctx.
func (f *FakeClient) EtcdStatus(ctx context.Context, _ ...grpc.CallOption) (*machineapi.EtcdStatusResponse, error) {
	node := f.recordEtcd(ctx, "Status")

	if err := f.EtcdUnhealthy[node]; err != nil {
		return nil, err
	}

	return &machineapi.EtcdStatusResponse{Messages: []*machineapi.EtcdStatus{{
		MemberStatus: &machineapi.EtcdMemberStatus{Errors: f.EtcdErrors[node]},
	}}}, nil
}

// Reboots returns a copy of the recorded reboot calls.
func (f *FakeClient) Reboots() []RebootCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.RebootCalls)
}

// State returns St.
func (f *FakeClient) State() state.State {
	if f.St == nil {
		panic("talostest: State not faked")
	}

	return f.St
}

// Close records that the client was closed.
func (f *FakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.closed = true

	return nil
}

// Calls returns a copy of the recorded Logs calls.
func (f *FakeClient) Calls() []LogsCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.LogsCalls)
}

func (f *FakeClient) recordEtcd(ctx context.Context, call string) string {
	node := nodeFromContext(ctx)

	f.mu.Lock()
	f.EtcdCalls = append(f.EtcdCalls, call+"@"+node)
	f.mu.Unlock()

	return node
}

// nodeFromContext returns the target node set with client.WithNode.
func nodeFromContext(ctx context.Context) string {
	md, _ := metadata.FromOutgoingContext(ctx)
	if v := md.Get("node"); len(v) > 0 {
		return v[0]
	}

	return ""
}

// fakeStream replays msgs, then returns err (io.EOF for a clean end).
type fakeStream struct {
	grpc.ClientStream

	msgs [][]byte
	err  error
}

func (s *fakeStream) Recv() (*common.Data, error) {
	if len(s.msgs) == 0 {
		return nil, s.err
	}

	msg := s.msgs[0]
	s.msgs = s.msgs[1:]

	return &common.Data{Bytes: msg}, nil
}

func (s *fakeStream) CloseSend() error { return nil }
