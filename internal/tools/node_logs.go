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
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// ToolNodeLogs is the name of the node logs tool.
const ToolNodeLogs = "talos_node_logs"

// NodeLogsInput is the input of the talos_node_logs tool.
type NodeLogsInput struct {
	Cluster    string `json:"cluster,omitempty" jsonschema:"Cluster name; default is current"`
	Node       string `json:"node,omitempty" jsonschema:"Target node (IP or hostname); default is the single talosconfig node"`
	Service    string `json:"service" jsonschema:"Service id, e.g. kubelet, etcd, apid, machined, containerd, cri; or container id with kubernetes=true"`
	Kubernetes bool   `json:"kubernetes,omitempty" jsonschema:"Read logs of a Kubernetes container (k8s.io namespace) instead of a Talos service"`
	Tail       int    `json:"tail,omitempty" jsonschema:"Number of lines from the end (default 100, max 1000)"`
	Grep       string `json:"grep,omitempty" jsonschema:"Case-insensitive substring filter applied before tail"`
}

// RegisterNodeLogs registers the node logs tool.
func (t *TalosTools) RegisterNodeLogs(srv *mcp.Server) {
	addTool(srv,
		&mcp.Tool{
			Name: ToolNodeLogs,
			Description: "Return the last lines of a Talos service's logs on one node (kubelet, etcd, apid, machined, containerd, ...), " +
				"or of a Kubernetes container with kubernetes=true. Secrets in the output are masked.",
			Annotations: &mcp.ToolAnnotations{
				IdempotentHint: true,
				ReadOnlyHint:   true,
				OpenWorldHint:  new(true),
			},
		},
		t.handlerNodeLogs,
	)

	t.track(toolSpec{name: ToolNodeLogs, minRole: talos.RoleReader, apis: []string{apiLogs, apiServiceList, apiCOSIList}})
}

func (t *TalosTools) handlerNodeLogs(ctx context.Context, _ *mcp.CallToolRequest, in NodeLogsInput) (*mcp.CallToolResult, NodeLogsResult, error) {
	ctx, cancel := withTimeout(ctx, defaultTimeout)
	defer cancel()

	result, err := t.NodeLogs(ctx, in)
	if err != nil {
		return nil, NodeLogsResult{}, err
	}

	what := result.Service + " logs"
	if in.Kubernetes {
		what = "container " + result.Service + " logs"
	}

	node := result.Node
	if result.NodeName != "" {
		node = result.NodeName + " (" + result.Node + ")"
	}

	return linesResult(linesHeader(what, result.Cluster, node, result.Count, result.Truncated, result.Warnings), result.Lines, result)
}

// NodeLogs returns the tail of a service or container log on one node
// (design §8.5).
func (t *TalosTools) NodeLogs(ctx context.Context, in NodeLogsInput) (*NodeLogsResult, error) {
	tail, err := tailOrDefault(in.Tail)
	if err != nil {
		return nil, err
	}

	service := strings.TrimSpace(in.Service)
	if service == "" {
		return nil, fmt.Errorf("service is required, e.g. kubelet, etcd, apid, machined")
	}

	target, err := t.resolveNodeTarget(ctx, in.Cluster, in.Node, talos.RoleReader)
	if err != nil {
		return nil, err
	}

	namespace, driver := constants.SystemContainerdNamespace, common.ContainerDriver_CONTAINERD
	if in.Kubernetes {
		namespace, driver = constants.K8sContainerdNamespace, common.ContainerDriver_CRI
	}

	nodeCtx := target.nodeContext(ctx)

	lines, truncated, err := t.tailLog(nodeCtx, target.client, namespace, driver, service, tail, in.Grep)
	if err != nil {
		return nil, t.logsError(nodeCtx, target, err, service, in.Kubernetes)
	}

	return &NodeLogsResult{
		Cluster:   target.cluster,
		Node:      target.node.Address,
		NodeName:  target.nodeName(),
		Service:   service,
		Lines:     lines,
		Count:     len(lines),
		Truncated: truncated,
		Warnings:  target.node.Warnings,
	}, nil
}

// tailLog returns the last tail lines of a service or container log that
// match grep, sanitized, and whether lines were dropped or cut. ctx aims at
// the node.
func (t *TalosTools) tailLog(ctx context.Context, c talos.Client, namespace string, driver common.ContainerDriver, id string, tail int, grep string) ([]string, bool, error) {
	// Ask for one line more than needed, to know whether more were available.
	window := grepWindow(tail, grep)

	stream, err := c.Logs(ctx, namespace, driver, id, false, int32(window+1))
	if err != nil {
		return nil, false, err
	}

	r, err := client.ReadStream(stream)
	if err != nil {
		return nil, false, err
	}
	defer r.Close() //nolint:errcheck

	var raw []rawLine

	if err := readLines(r, func(line string, cut bool) { raw = append(raw, rawLine{line, cut}) }); err != nil {
		return nil, false, err
	}

	more := len(raw) > window
	if more {
		raw = raw[len(raw)-window:]
	}

	w := newLineWindow(tail, grep, t.sanitizer.Sanitize)
	for _, l := range raw {
		w.add(l.text, l.cut)
	}

	lines, truncated := w.lines()

	return lines, truncated || more, nil
}

type rawLine struct {
	text string
	cut  bool
}

// logsError maps a Logs failure. For a Talos service that the node doesn't
// run, the error lists the node's services (design §8.5).
func (t *TalosTools) logsError(ctx context.Context, target *nodeTarget, err error, service string, kubernetes bool) error {
	mapped := t.talosError(ctx, err, target.cluster, "Logs")

	if kubernetes {
		return mapped
	}

	resp, lerr := target.client.ServiceList(ctx)
	if lerr != nil {
		return mapped
	}

	var ids []string

	for _, msg := range resp.GetMessages() {
		for _, svc := range msg.GetServices() {
			ids = append(ids, svc.GetId())
		}
	}

	slices.Sort(ids)

	if slices.Contains(ids, service) {
		return mapped
	}

	return fmt.Errorf("unknown service %q on %s/%s; services: %s (use kubernetes=true for a container id)",
		service, target.cluster, target.label(), strings.Join(ids, ", "))
}
