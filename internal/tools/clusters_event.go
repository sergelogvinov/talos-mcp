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
	"bytes"
	"cmp"
	"context"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"google.golang.org/grpc/status"
)

// ToolClustersEvent is the name of the cluster events tool.
const ToolClustersEvent = "talos_clusters_event"

const apiEvents = "/machine.MachineService/Events"

// Event window and limits (design §8.3).
const (
	defaultEventsSince = time.Hour
	maxEventsSince     = 30 * 24 * time.Hour
	defaultEventLimit  = 50
	maxEventLimit      = 500
	// eventsDrainTimeout bounds the read of one node's backlog.
	eventsDrainTimeout = 15 * time.Second
)

// Event read timings, variables so tests can shorten them.
var (
	// eventsHelloTimeout is how long a node has to open its stream (the
	// hello event): a node that is down or rebooting fails after it.
	eventsHelloTimeout = 5 * time.Second
	// eventsIdleTimeout ends a node's stream once the backlog is drained:
	// machined keeps the stream open for new events.
	eventsIdleTimeout = time.Second
	// eventsFanOut is how many nodes are read at once.
	eventsFanOut = 8
)

// ClustersEventInput is the input of the talos_clusters_event tool.
type ClustersEventInput struct {
	Cluster string `json:"cluster,omitempty" jsonschema:"Cluster name; default is current"`
	Node    string `json:"node,omitempty" jsonschema:"Limit to one node (IP or hostname); default all nodes"`
	Since   string `json:"since,omitempty" jsonschema:"Only events newer than this duration, e.g. 15m, 2h (default 1h)"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Maximum number of events to return (default 50, max 500)"`
	ActorID string `json:"actor_id,omitempty" jsonschema:"Only events of this Talos actor id, e.g. the one returned by talos_node_reboot"`
}

// xidEncoding decodes machined event IDs (github.com/rs/xid).
var xidEncoding = base32.NewEncoding("0123456789abcdefghijklmnopqrstuv").WithPadding(base32.NoPadding)

// RegisterClustersEvent registers the cluster events tool.
func (t *TalosTools) RegisterClustersEvent(srv *mcp.Server) {
	mcp.AddTool(srv,
		&mcp.Tool{
			Name: ToolClustersEvent,
			Description: "Return recent Talos runtime events of all nodes of a cluster, or of one node, newest first: " +
				"boot/reboot/upgrade sequences, phases and tasks, service state changes, machine status and config errors. " +
				"Use it to see what just happened on a node, or whether a reboot finished.",
			Annotations: &mcp.ToolAnnotations{
				IdempotentHint: true,
				ReadOnlyHint:   true,
				OpenWorldHint:  new(true),
			},
		},
		t.handlerClustersEvent,
	)

	t.track(toolSpec{name: ToolClustersEvent, minRole: talos.RoleReader, apis: []string{apiEvents, apiCOSIList}})
}

func (t *TalosTools) handlerClustersEvent(ctx context.Context, _ *mcp.CallToolRequest, in ClustersEventInput) (*mcp.CallToolResult, ClustersEventResult, error) {
	ctx, cancel := withTimeout(ctx, aggregateTimeout)
	defer cancel()

	result, err := t.ClustersEvent(ctx, in)
	if err != nil {
		return nil, ClustersEventResult{}, err
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: eventsText(result)}},
	}, *result, nil
}

// ClustersEvent returns the recent machined events of a cluster (design
// §8.3). Each node's backlog since `since` is read until the stream goes
// idle; nodes that fail are reported in Warnings.
func (t *TalosTools) ClustersEvent(ctx context.Context, in ClustersEventInput) (*ClustersEventResult, error) {
	since, err := eventsSince(in.Since)
	if err != nil {
		return nil, err
	}

	limit, err := eventLimit(in.Limit)
	if err != nil {
		return nil, err
	}

	name, err := t.pool.Resolve(in.Cluster)
	if err != nil {
		return nil, err
	}

	if err := t.pool.Require(name, talos.RoleReader); err != nil {
		return nil, err
	}

	nodes, warnings, endpointErr, err := t.eventNodes(ctx, name, in.Node)
	if err != nil {
		return nil, err
	}

	c, err := t.pool.Client(ctx, name)
	if err != nil {
		return nil, err
	}

	opts := []client.EventsOptionFunc{client.WithTailDuration(since)}
	if in.ActorID != "" {
		opts = append(opts, client.WithActorID(in.ActorID))
	}

	results := make([]nodeEvents, len(nodes))
	sem := make(chan struct{}, eventsFanOut)

	var wg sync.WaitGroup

	for i, node := range nodes {
		wg.Go(func() {
			// A node still waiting for a slot when the call's deadline
			// passes is not read at all.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = nodeEvents{err: ctx.Err()}

				return
			}
			defer func() { <-sem }()

			if err := ctx.Err(); err != nil {
				results[i] = nodeEvents{err: err}

				return
			}

			results[i] = drainEvents(ctx, c, node, limit, opts)
		})
	}

	wg.Wait()

	result := &ClustersEventResult{
		Cluster:  name,
		Since:    since.String(),
		Nodes:    len(nodes),
		Events:   []EventSummary{},
		Warnings: warnings,
	}

	var (
		firstErr     error
		nodeWarnings []string
	)

	for i, r := range results {
		label := nodeLabel(nodes[i])

		switch {
		case errors.Is(r.err, context.DeadlineExceeded):
			firstErr = cmp.Or(firstErr, r.err)
			nodeWarnings = append(nodeWarnings, fmt.Sprintf("node %s: not read, the call's time limit was reached", label))
		case r.err != nil:
			firstErr = cmp.Or(firstErr, r.err)
			nodeWarnings = append(nodeWarnings, fmt.Sprintf("node %s: reading events failed: %s", label, grpcMessage(r.err)))
		case r.incomplete:
			nodeWarnings = append(nodeWarnings, fmt.Sprintf("node %s: events still arriving after %s, the list may be incomplete", label, eventsDrainTimeout))
		}

		result.Truncated = result.Truncated || r.dropped

		for _, ev := range r.raw {
			result.Events = append(result.Events, t.eventSummary(ev, nodes[i]))
		}
	}

	failed := 0

	for _, r := range results {
		if r.err != nil {
			failed++
		}
	}

	if failed == len(nodes) {
		return nil, t.noNodeAnswered(ctx, name, "Events", firstErr, endpointErr, nodeWarnings)
	}

	result.Warnings = append(result.Warnings, nodeWarnings...)

	slices.SortFunc(result.Events, func(a, b EventSummary) int {
		return cmp.Or(strings.Compare(b.Time, a.Time), strings.Compare(b.ID, a.ID))
	})

	if len(result.Events) > limit {
		result.Events = result.Events[:limit]
		result.Truncated = true
	}

	result.Count = len(result.Events)

	return result, nil
}

// eventNodes is the node argument resolved to the nodes to read: one node,
// or every cluster member when it is empty. endpointErr is set when the
// member list could not be read through the endpoints.
func (t *TalosTools) eventNodes(ctx context.Context, cluster, node string) (nodes []talos.ResolvedNode, warnings []string, endpointErr, err error) {
	if strings.TrimSpace(node) == "" {
		targets, err := t.pool.ResolveAllNodes(ctx, cluster)
		if err != nil {
			return nil, nil, nil, err
		}

		if len(targets.Nodes) == 0 {
			return nil, nil, nil, fmt.Errorf("cluster %s: no nodes to read events from%s", cluster, warningsNote(targets.Warnings))
		}

		return targets.Nodes, targets.Warnings, targets.ReadErr, nil
	}

	resolved, err := t.pool.ResolveNode(ctx, cluster, node)
	if err != nil {
		return nil, nil, nil, err
	}

	return []talos.ResolvedNode{*resolved}, resolved.Warnings, nil, nil
}

// nodeEvents is what was read from one node.
type nodeEvents struct {
	// raw are the newest events, oldest first; they are summarized after
	// the read, so dropped events cost nothing.
	raw []*machineapi.Event
	// dropped is set when older events were dropped to keep the limit.
	dropped bool
	// incomplete is set when the drain timeout ended the read.
	incomplete bool
	err        error
}

type eventRecv struct {
	ev  *machineapi.Event
	err error
}

// drainEvents reads the event backlog of one node and keeps the newest keep
// events. machined sends an empty hello event, then the backlog, then keeps
// the stream open. A node without a hello within eventsHelloTimeout fails;
// after it, the read ends after eventsIdleTimeout without an event.
//
//nolint:gocyclo
func drainEvents(parent context.Context, c talos.Client, node talos.ResolvedNode, keep int, opts []client.EventsOptionFunc) nodeEvents {
	ctx, cancel := context.WithTimeout(parent, eventsDrainTimeout)
	defer cancel()

	stream, err := c.Events(client.WithNode(ctx, node.Address), opts...)
	if err != nil {
		return nodeEvents{err: err}
	}

	if err := stream.CloseSend(); err != nil {
		return nodeEvents{err: err}
	}

	recv := make(chan eventRecv)

	go func() {
		for {
			ev, err := stream.Recv()

			select {
			case recv <- eventRecv{ev, err}:
			case <-ctx.Done():
				return
			}

			if err != nil {
				return
			}
		}
	}()

	// The timer first waits for the hello, then for each next message.
	idle := time.NewTimer(eventsHelloTimeout)
	defer idle.Stop()

	var (
		out   nodeEvents
		hello bool
	)

	for {
		select {
		case r := <-recv:
			switch {
			case errors.Is(r.err, io.EOF):
				return out
			case r.err != nil:
				if parent.Err() == nil && ctx.Err() != nil {
					out.incomplete = true
				} else {
					out.err = r.err
				}

				return out
			}

			//nolint:staticcheck // apid still reports node errors in Metadata.Error.
			if md := r.ev.GetMetadata(); md.GetError() != "" {
				out.err = errors.New(md.GetError()) //nolint:staticcheck
				if md.GetStatus() != nil {
					out.err = status.FromProto(md.GetStatus()).Err()
				}

				return out
			}

			hello = true

			idle.Reset(eventsIdleTimeout)

			if r.ev.GetData() == nil {
				continue // the hello event
			}

			out.raw = append(out.raw, r.ev)
			if len(out.raw) > keep {
				out.raw = out.raw[1:]
				out.dropped = true
			}
		case <-idle.C:
			if !hello {
				out.err = fmt.Errorf("no answer within %s; the node may be down or rebooting", eventsHelloTimeout)
			}

			return out
		case <-ctx.Done():
			if parent.Err() != nil {
				out.err = parent.Err()
			} else {
				out.incomplete = true
			}

			return out
		}
	}
}

// eventSummary turns a machined event into its one-line summary.
func (t *TalosTools) eventSummary(ev *machineapi.Event, node talos.ResolvedNode) EventSummary {
	s := EventSummary{
		Node:    node.Address,
		ActorID: ev.GetActorId(),
		ID:      ev.GetId(),
	}

	if node.Name != node.Address {
		s.NodeName = node.Name
	}

	if at, ok := eventTime(ev.GetId()); ok {
		s.Time = at.UTC().Format(time.RFC3339)
	}

	s.Type, s.Summary = describeEvent(ev)
	// Sanitize before joining the lines: the YAML key rules match at the
	// start of a line, so a joined config error would keep its secrets.
	s.Summary = strings.Join(strings.Fields(t.sanitizer.Sanitize(s.Summary)), " ")

	return s
}

// describeEvent returns the type and summary of an event payload.
//
//nolint:gocyclo
func describeEvent(ev *machineapi.Event) (string, string) {
	decoded, err := client.UnmarshalEvent(ev)
	if err != nil {
		typeURL := ev.GetData().GetTypeUrl()

		return typeURL[strings.LastIndex(typeURL, ".")+1:], "event not supported by this server version"
	}

	switch p := decoded.Payload.(type) {
	case *machineapi.SequenceEvent:
		summary := p.GetSequence() + " " + action(p.GetAction().String())
		if e := p.GetError(); e != nil {
			summary += " with error: " + e.GetMessage()
		}

		return "sequence", summary
	case *machineapi.PhaseEvent:
		return "phase", p.GetPhase() + " " + action(p.GetAction().String())
	case *machineapi.TaskEvent:
		return "task", p.GetTask() + " " + action(p.GetAction().String())
	case *machineapi.ServiceStateEvent:
		summary := p.GetService() + " " + action(p.GetAction().String())
		if m := p.GetMessage(); m != "" {
			summary += ": " + m
		}

		if h := p.GetHealth(); h != nil && !h.GetUnknown() {
			if h.GetHealthy() {
				summary += " (healthy)"
			} else {
				summary += " (unhealthy: " + h.GetLastMessage() + ")"
			}
		}

		return "service", summary
	case *machineapi.MachineStatusEvent:
		summary := "stage " + action(p.GetStage().String())
		if st := p.GetStatus(); st.GetReady() {
			summary += ", ready"
		} else {
			unmet := make([]string, 0, len(st.GetUnmetConditions()))
			for _, c := range st.GetUnmetConditions() {
				unmet = append(unmet, c.GetName()+" ("+c.GetReason()+")")
			}

			summary += ", not ready"
			if len(unmet) > 0 {
				summary += ": " + strings.Join(unmet, ", ")
			}
		}

		return "machine_status", summary
	case *machineapi.ConfigLoadErrorEvent:
		return "config_load_error", p.GetError()
	case *machineapi.ConfigValidationErrorEvent:
		return "config_validation_error", p.GetError()
	case *machineapi.AddressEvent:
		return "address", "hostname " + p.GetHostname() + ", addresses " + strings.Join(p.GetAddresses(), ", ")
	case *machineapi.RestartEvent:
		return "restart", fmt.Sprintf("restart requested, command %d", p.GetCmd())
	default:
		return "unknown", decoded.TypeURL
	}
}

// action lowercases a proto enum name: START -> start, SHUTTING_DOWN ->
// shutting down.
func action(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "_", " ")
}

// eventTime decodes the creation time from a machined event ID, an xid.
func eventTime(id string) (time.Time, bool) {
	if len(id) != 20 {
		return time.Time{}, false
	}

	raw, err := xidEncoding.DecodeString(id)
	if err != nil || len(raw) != 12 {
		return time.Time{}, false
	}

	return time.Unix(int64(binary.BigEndian.Uint32(raw[:4])), 0), true
}

// eventsSince validates the since argument: empty means one hour.
func eventsSince(since string) (time.Duration, error) {
	if since == "" {
		return defaultEventsSince, nil
	}

	d, err := time.ParseDuration(since)
	if err != nil || d < time.Second || d > maxEventsSince {
		return 0, fmt.Errorf("invalid since %q: must be a duration between 1s and %s, e.g. 15m or 2h", since, maxEventsSince)
	}

	return d, nil
}

// eventLimit validates the limit argument: 0 means defaultEventLimit.
func eventLimit(limit int) (int, error) {
	switch {
	case limit == 0:
		return defaultEventLimit, nil
	case limit < 0 || limit > maxEventLimit:
		return 0, fmt.Errorf("invalid limit %d: must be between 1 and %d", limit, maxEventLimit)
	default:
		return limit, nil
	}
}

// nodeLabel is a node as shown in messages: "name (address)" or "address".
func nodeLabel(n talos.ResolvedNode) string {
	if n.Name != "" && n.Name != n.Address {
		return n.Name + " (" + n.Address + ")"
	}

	return n.Address
}

// warningsNote appends warnings to an error message.
func warningsNote(warnings []string) string {
	if len(warnings) == 0 {
		return ""
	}

	return " (" + strings.Join(warnings, "; ") + ")"
}

// eventsText renders the result as a header line, warnings, and one aligned
// line per event.
func eventsText(r *ClustersEventResult) string {
	var b bytes.Buffer

	fmt.Fprintf(&b, "events on %s since %s: %d events from %d nodes", r.Cluster, r.Since, r.Count, r.Nodes)

	if r.Truncated {
		b.WriteString(" (truncated)")
	}

	for _, w := range r.Warnings {
		b.WriteString("\nwarning: " + w)
	}

	if len(r.Events) == 0 {
		return b.String()
	}

	b.WriteByte('\n')

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, e := range r.Events {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", cmp.Or(e.Time, "-"), cmp.Or(e.NodeName, e.Node), e.Type, e.Summary)
	}

	tw.Flush() //nolint:errcheck

	return strings.TrimRight(b.String(), "\n")
}
