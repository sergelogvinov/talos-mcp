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

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// FakeNode is one node behind apid.
type FakeNode struct {
	// Err fails every call to the node, as when it is unreachable.
	Err error

	Version  string
	Services []*machineapi.ServiceInfo
	// BootTime is Unix seconds; CPUs the number of cores.
	BootTime        uint64
	CPUs            int
	MemTotalKiB     uint64
	MemAvailableKiB uint64

	// St serves the node's COSI resources.
	St state.State
}

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
	// St serves COSI resources (Members) when set. COSI calls aimed at a
	// node (client.WithNode) in Nodes use that node's St instead.
	St state.State

	// Nodes holds the replies of Version, ServiceList, SystemStat and
	// Memory per target node (client.WithNode). APIErr fails all of these
	// calls and COSI reads, as when the endpoint is down.
	Nodes  map[string]*FakeNode
	APIErr error

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

	// EventLog maps a node address to the events it streams after the hello
	// event. Then the stream stays open until ctx ends, as machined's does,
	// unless EventsEnd has an error (io.EOF for a clean end) for the node.
	// EventsErr maps a node address to the error Events returns for it.
	// A node in EventsSilent sends nothing, not even the hello, as a node
	// that is down or rebooting behind apid.
	EventLog     map[string][]*machineapi.Event
	EventsEnd    map[string]error
	EventsErr    map[string]error
	EventsSilent map[string]bool

	// RebootErr is returned by RebootWithResponse; ActorID is in its response.
	RebootErr error
	ActorID   string

	mu          sync.Mutex
	LogsCalls   []LogsCall
	DmesgNodes  []string
	EventsCalls []EventsCall
	RebootCalls []RebootCall
	EtcdCalls   []string // "MemberList@<node>" and "Status@<node>"
	closed      bool
}

// EventsCall records one Events call of a FakeClient.
type EventsCall struct {
	Node    string
	Request *machineapi.EventsRequest
}

// RebootCall records one RebootWithResponse call of a FakeClient.
type RebootCall struct {
	Node string
	Mode machineapi.RebootRequest_Mode
}

// Version answers for the node in ctx from Nodes.
func (f *FakeClient) Version(ctx context.Context, _ ...grpc.CallOption) (*machineapi.VersionResponse, error) {
	n, err := f.node(ctx)
	if err != nil {
		return nil, err
	}

	return &machineapi.VersionResponse{Messages: []*machineapi.Version{{Version: &machineapi.VersionInfo{Tag: n.Version}}}}, nil
}

// ServiceList answers from Nodes for a node in it, else returns Services.
func (f *FakeClient) ServiceList(ctx context.Context, _ ...grpc.CallOption) (*machineapi.ServiceListResponse, error) {
	if f.ServiceListErr != nil {
		return nil, f.ServiceListErr
	}

	if _, ok := f.Nodes[nodeFromContext(ctx)]; ok {
		n, err := f.node(ctx)
		if err != nil {
			return nil, err
		}

		return &machineapi.ServiceListResponse{Messages: []*machineapi.ServiceList{{Services: n.Services}}}, nil
	}

	services := make([]*machineapi.ServiceInfo, 0, len(f.Services))
	for _, id := range f.Services {
		services = append(services, &machineapi.ServiceInfo{Id: id, State: "Running"})
	}

	return &machineapi.ServiceListResponse{Messages: []*machineapi.ServiceList{{Services: services}}}, nil
}

// SystemStat answers for the node in ctx from Nodes.
func (f *FakeClient) SystemStat(ctx context.Context, _ ...grpc.CallOption) (*machineapi.SystemStatResponse, error) {
	n, err := f.node(ctx)
	if err != nil {
		return nil, err
	}

	msg := &machineapi.SystemStat{BootTime: n.BootTime}
	for range n.CPUs {
		msg.Cpu = append(msg.Cpu, &machineapi.CPUStat{})
	}

	return &machineapi.SystemStatResponse{Messages: []*machineapi.SystemStat{msg}}, nil
}

// Memory answers for the node in ctx from Nodes.
func (f *FakeClient) Memory(ctx context.Context, _ ...grpc.CallOption) (*machineapi.MemoryResponse, error) {
	n, err := f.node(ctx)
	if err != nil {
		return nil, err
	}

	return &machineapi.MemoryResponse{Messages: []*machineapi.Memory{{
		Meminfo: &machineapi.MemInfo{Memtotal: n.MemTotalKiB, Memavailable: n.MemAvailableKiB},
	}}}, nil
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

// Events streams a hello event, then EventLog[node]; see EventLog.
func (f *FakeClient) Events(ctx context.Context, opts ...client.EventsOptionFunc) (machineapi.MachineService_EventsClient, error) {
	var req machineapi.EventsRequest
	for _, opt := range opts {
		opt(&req)
	}

	node := nodeFromContext(ctx)

	f.mu.Lock()
	f.EventsCalls = append(f.EventsCalls, EventsCall{Node: node, Request: &req})
	f.mu.Unlock()

	if err := f.EventsErr[node]; err != nil {
		return nil, err
	}

	if f.EventsSilent[node] {
		return &fakeEventStream{ctx: ctx}, nil
	}

	events := append([]*machineapi.Event{{}}, f.EventLog[node]...)

	return &fakeEventStream{ctx: ctx, events: events, end: f.EventsEnd[node]}, nil
}

// EventsRequests returns a copy of the recorded Events calls.
func (f *FakeClient) EventsRequests() []EventsCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.EventsCalls)
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

// State returns St, routing calls aimed at a node in Nodes to its St.
func (f *FakeClient) State() state.State {
	if f.St == nil {
		panic("talostest: State not faked")
	}

	return state.WrapCore(&nodeState{CoreState: f.St, f: f})
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

// node returns the Nodes entry of the node in ctx, or the error apid would
// return for it.
func (f *FakeClient) node(ctx context.Context) (*FakeNode, error) {
	if f.APIErr != nil {
		return nil, f.APIErr
	}

	node := nodeFromContext(ctx)

	n := f.Nodes[node]
	switch {
	case n == nil:
		return nil, status.Errorf(codes.Unavailable, "unknown node %q", node)
	case n.Err != nil:
		return nil, n.Err
	default:
		return n, nil
	}
}

func (f *FakeClient) recordEtcd(ctx context.Context, call string) string {
	node := nodeFromContext(ctx)

	f.mu.Lock()
	f.EtcdCalls = append(f.EtcdCalls, call+"@"+node)
	f.mu.Unlock()

	return node
}

// nodeState routes COSI reads aimed at a node with its own St.
type nodeState struct {
	state.CoreState

	f *FakeClient
}

// Get reads from the target node's state.
func (s *nodeState) Get(ctx context.Context, ptr resource.Pointer, opts ...state.GetOption) (resource.Resource, error) {
	st, err := s.core(ctx)
	if err != nil {
		return nil, err
	}

	return st.Get(ctx, ptr, opts...)
}

// List reads from the target node's state.
func (s *nodeState) List(ctx context.Context, kind resource.Kind, opts ...state.ListOption) (resource.List, error) {
	st, err := s.core(ctx)
	if err != nil {
		return resource.List{}, err
	}

	return st.List(ctx, kind, opts...)
}

func (s *nodeState) core(ctx context.Context) (state.CoreState, error) {
	if s.f.APIErr != nil {
		return nil, s.f.APIErr
	}

	node := nodeFromContext(ctx)
	if node == "" {
		return s.CoreState, nil
	}

	n := s.f.Nodes[node]

	switch {
	case n == nil:
		return s.CoreState, nil
	case n.Err != nil:
		return nil, n.Err
	case n.St == nil:
		return s.CoreState, nil
	default:
		return n.St, nil
	}
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

// fakeEventStream replays events, then returns end, or blocks until ctx
// ends when end is nil.
type fakeEventStream struct {
	grpc.ClientStream

	ctx    context.Context //nolint:containedctx
	events []*machineapi.Event
	end    error
}

func (s *fakeEventStream) Recv() (*machineapi.Event, error) {
	if len(s.events) > 0 {
		ev := s.events[0]
		s.events = s.events[1:]

		return ev, nil
	}

	if s.end != nil {
		return nil, s.end
	}

	<-s.ctx.Done()

	return nil, status.FromContextError(s.ctx.Err()).Err()
}

func (s *fakeEventStream) CloseSend() error { return nil }
