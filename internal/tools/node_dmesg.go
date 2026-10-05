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
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	"github.com/siderolabs/talos/pkg/machinery/client"
)

// ToolNodeDmesg is the name of the node dmesg tool.
const ToolNodeDmesg = "talos_node_dmesg"

// NodeDmesgInput is the input of the talos_node_dmesg tool.
type NodeDmesgInput struct {
	Cluster string `json:"cluster,omitempty" jsonschema:"Cluster name; default is current"`
	Node    string `json:"node,omitempty" jsonschema:"Target node (IP or hostname); default is the single talosconfig node"`
	Tail    int    `json:"tail,omitempty" jsonschema:"Number of lines from the end (default 100, max 1000)"`
	Grep    string `json:"grep,omitempty" jsonschema:"Case-insensitive substring filter applied before tail"`
}

// kmsgLine matches a machined Dmesg message:
// "<facility>: <priority>: [<RFC3339Nano>]: <message>".
var kmsgLine = regexp.MustCompile(`^\s*([^:\s]+):\s*([^:\s]+):\s*\[([^\]]+)\]:\s?(.*)$`)

// RegisterNodeDmesg registers the node dmesg tool.
func (t *TalosTools) RegisterNodeDmesg(srv *mcp.Server) {
	addTool(srv,
		&mcp.Tool{
			Name:        ToolNodeDmesg,
			Description: "Return the last lines of the kernel log (dmesg) of one node. Secrets in the output are masked.",
			Annotations: &mcp.ToolAnnotations{
				IdempotentHint: true,
				ReadOnlyHint:   true,
				OpenWorldHint:  new(true),
			},
		},
		t.handlerNodeDmesg,
	)

	t.track(toolSpec{name: ToolNodeDmesg, minRole: talos.RoleReader, apis: []string{apiDmesg, apiCOSIList}})
}

func (t *TalosTools) handlerNodeDmesg(ctx context.Context, _ *mcp.CallToolRequest, in NodeDmesgInput) (*mcp.CallToolResult, NodeDmesgResult, error) {
	ctx, cancel := withTimeout(ctx, defaultTimeout)
	defer cancel()

	result, err := t.NodeDmesg(ctx, in)
	if err != nil {
		return nil, NodeDmesgResult{}, err
	}

	node := result.Node
	if result.NodeName != "" {
		node = result.NodeName + " (" + result.Node + ")"
	}

	return linesResult(linesHeader("dmesg", result.Cluster, node, result.Count, result.Truncated, result.Warnings), result.Lines, result)
}

// NodeDmesg returns the tail of the kernel ring buffer of one node. The
// whole buffer is streamed, and the last tail lines are kept.
func (t *TalosTools) NodeDmesg(ctx context.Context, in NodeDmesgInput) (*NodeDmesgResult, error) {
	tail, err := tailOrDefault(in.Tail)
	if err != nil {
		return nil, err
	}

	target, err := t.resolveNodeTarget(ctx, in.Cluster, in.Node, talos.RoleReader)
	if err != nil {
		return nil, err
	}

	nodeCtx := target.nodeContext(ctx)

	stream, err := target.client.Dmesg(nodeCtx, false, false)
	if err != nil {
		return nil, t.talosError(nodeCtx, err, target.cluster, "Dmesg")
	}

	r, err := client.ReadStream(newlineStream{stream})
	if err != nil {
		return nil, t.talosError(nodeCtx, err, target.cluster, "Dmesg")
	}
	defer r.Close() //nolint:errcheck

	w := newLineWindow(tail, in.Grep, t.sanitizer.Sanitize)

	if err := readLines(r, func(line string, cut bool) { w.add(formatKmsg(line), cut) }); err != nil {
		return nil, t.talosError(nodeCtx, err, target.cluster, "Dmesg")
	}

	lines, truncated := w.lines()

	return &NodeDmesgResult{
		Cluster:   target.cluster,
		Node:      target.node.Address,
		NodeName:  target.nodeName(),
		Lines:     lines,
		Count:     len(lines),
		Truncated: truncated,
		Warnings:  target.node.Warnings,
	}, nil
}

// formatKmsg rewrites a machined Dmesg message as
// "<RFC3339 time> <facility>.<priority> <message>". Lines in another format
// are returned unchanged.
func formatKmsg(line string) string {
	line = strings.TrimRight(line, "\r\n")

	m := kmsgLine.FindStringSubmatch(line)
	if m == nil {
		return line
	}

	ts := m[3]
	if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		ts = parsed.UTC().Format(time.RFC3339)
	}

	return ts + " " + m[1] + "." + m[2] + " " + m[4]
}

// newlineStream ends every Dmesg message with a newline: machined sends one
// message per frame without one, and readLines splits on newlines.
type newlineStream struct {
	client.MachineStream
}

func (s newlineStream) Recv() (*common.Data, error) {
	data, err := s.MachineStream.Recv()
	if err != nil || data == nil || len(data.Bytes) == 0 || data.Bytes[len(data.Bytes)-1] == '\n' {
		return data, err
	}

	data.Bytes = append(data.Bytes, '\n')

	return data, nil
}
