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
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/logger"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
)

// ToolNodeReboot is the name of the destructive node reboot tool.
const ToolNodeReboot = "talos_node_reboot"

// Reboot modes accepted by the tool.
const (
	rebootModeDefault    = "default"
	rebootModePowercycle = "powercycle"
)

// etcdProbeTimeout bounds the health probe of one etcd member.
const etcdProbeTimeout = 5 * time.Second

const (
	apiReboot         = "/machine.MachineService/Reboot"
	apiEtcdMemberList = "/machine.MachineService/EtcdMemberList"
	apiEtcdStatus     = "/machine.MachineService/EtcdStatus"
)

// NodeRebootInput is the input of the talos_node_reboot tool.
type NodeRebootInput struct {
	Cluster string `json:"cluster" jsonschema:"Cluster name; one of the clusters with an operator credential"`
	Node    string `json:"node" jsonschema:"Target node (IP or hostname); exactly one node, required"`
	Mode    string `json:"mode,omitempty" jsonschema:"default (graceful) or powercycle"`
}

// RegisterNodeReboot registers the reboot tool. It is only called with
// --allow-destructive and at least one operator cluster (design §9).
func (t *TalosTools) RegisterNodeReboot(srv *mcp.Server) {
	clusters := t.pool.ClustersWithRole(talos.RoleOperator)

	schema, err := jsonschema.For[NodeRebootInput](nil)
	if err != nil {
		panic(fmt.Sprintf("%s: input schema: %v", ToolNodeReboot, err))
	}

	// cluster is always required: the default context may be a reader one.
	schema.Properties["cluster"].Enum = toAny(clusters)
	schema.Properties["mode"].Enum = toAny([]string{"", rebootModeDefault, rebootModePowercycle})
	schema.Required = []string{"cluster", "node"}

	mcp.AddTool(srv,
		&mcp.Tool{
			Name: ToolNodeReboot,
			Description: "Reboot one Talos node and return once Talos accepts the request; it does not wait for the node. " +
				"Before rebooting a control plane node, etcd quorum is checked and the reboot is refused if quorum would be lost. " +
				"Available on: " + strings.Join(clusters, ", ") + ".",
			InputSchema: schema,
			Annotations: &mcp.ToolAnnotations{
				DestructiveHint: new(true),
				OpenWorldHint:   new(true),
			},
		},
		t.handlerNodeReboot,
	)

	t.track(toolSpec{
		name:    ToolNodeReboot,
		minRole: talos.RoleOperator,
		apis:    []string{apiReboot, apiEtcdMemberList, apiEtcdStatus, apiCOSIList},
	})
}

func (t *TalosTools) handlerNodeReboot(ctx context.Context, _ *mcp.CallToolRequest, in NodeRebootInput) (*mcp.CallToolResult, NodeRebootResult, error) {
	ctx, cancel := withTimeout(ctx, defaultTimeout)
	defer cancel()

	result, err := t.NodeReboot(ctx, in)
	if err != nil {
		return nil, NodeRebootResult{}, err
	}

	return textResult(result)
}

// NodeReboot reboots one node (design §8.7). The role check runs before any
// Talos call, the node must be named explicitly, and a control plane node is
// only rebooted when etcd keeps quorum without it.
func (t *TalosTools) NodeReboot(ctx context.Context, in NodeRebootInput) (*NodeRebootResult, error) {
	if strings.TrimSpace(in.Cluster) == "" {
		return nil, fmt.Errorf("cluster is required for %s; operator clusters: %s",
			ToolNodeReboot, strings.Join(t.pool.ClustersWithRole(talos.RoleOperator), ", "))
	}

	mode, opts, err := rebootMode(in.Mode)
	if err != nil {
		return nil, err
	}

	node := strings.TrimSpace(in.Node)

	switch {
	case node == "":
		return nil, fmt.Errorf("node is required for %s: name exactly one node", ToolNodeReboot)
	case strings.ContainsAny(node, ", "):
		return nil, fmt.Errorf("node %q names several nodes; %s reboots exactly one node per call", node, ToolNodeReboot)
	}

	target, err := t.resolveNodeTarget(ctx, in.Cluster, node, talos.RoleOperator)
	if err != nil {
		return nil, err
	}

	etcd, err := t.checkEtcdQuorum(ctx, target)
	if err != nil {
		return nil, err
	}

	logger.FromContext(ctx).Info("rebooting node", "cluster", target.cluster, "node", target.node.Address, "mode", mode)

	resp, err := target.client.RebootWithResponse(target.nodeContext(ctx), opts...)
	if err != nil {
		return nil, t.talosError(ctx, err, target.cluster, "Reboot")
	}

	result := &NodeRebootResult{
		Cluster:  target.cluster,
		Node:     target.node.Address,
		Hostname: target.nodeName(),
		Mode:     mode,
		Accepted: true,
		Etcd:     etcd,
		Hint:     t.rebootHint(target),
	}

	for _, msg := range resp.GetMessages() {
		if id := msg.GetActorId(); id != "" {
			result.ActorID = id

			break
		}
	}

	return result, nil
}

// rebootHint tells the agent how to follow the reboot: with the events tool
// when it is registered, else with the machined logs.
func (t *TalosTools) rebootHint(target *nodeTarget) string {
	if t.isRegistered(ToolClustersEvent) {
		return fmt.Sprintf("The node reboots now and is unreachable for a while. Follow it with %s cluster=%s node=%s since=10m; "+
			"the reboot is done when a boot sequence stops and machine_status reports running, ready.",
			ToolClustersEvent, target.cluster, target.node.Address)
	}

	return fmt.Sprintf("The node reboots now and is unreachable for a while. Check it with %s cluster=%s node=%s service=machined.",
		ToolNodeLogs, target.cluster, target.node.Address)
}

// rebootMode maps the mode argument to Talos reboot options.
func rebootMode(mode string) (string, []client.RebootMode, error) {
	switch mode {
	case "", rebootModeDefault:
		return rebootModeDefault, []client.RebootMode{client.WithRebootMode(machineapi.RebootRequest_DEFAULT)}, nil
	case rebootModePowercycle:
		return rebootModePowercycle, []client.RebootMode{client.WithPowerCycle}, nil
	default:
		return "", nil, fmt.Errorf("invalid mode %q: must be default or powercycle", mode)
	}
}

// checkEtcdQuorum refuses a reboot that would leave etcd without a healthy
// majority. It returns a short summary for the result, empty for a node that
// is not an etcd member. A node that Members reports as a worker is not
// checked; otherwise etcd membership decides, so a control plane node given
// by IP is checked too. If membership can't be read, the reboot is refused.
func (t *TalosTools) checkEtcdQuorum(ctx context.Context, target *nodeTarget) (string, error) {
	if target.node.MachineType == talos.MachineTypeWorker {
		return "", nil
	}

	resp, err := target.client.EtcdMemberList(ctx, &machineapi.EtcdMemberListRequest{})
	if err != nil {
		return "", fmt.Errorf("refusing to reboot %s: cannot verify etcd quorum: %w",
			target.label(), t.talosError(ctx, err, target.cluster, "EtcdMemberList"))
	}

	var members []*machineapi.EtcdMember

	for _, msg := range resp.GetMessages() {
		members = append(members, msg.GetMembers()...)
	}

	self := slices.IndexFunc(members, func(m *machineapi.EtcdMember) bool { return isTarget(m, target.node) })
	if self < 0 {
		// Not an etcd member: a worker, or a node given by IP.
		return "", nil
	}

	if members[self].GetIsLearner() {
		return "target is an etcd learner; quorum is not affected", nil
	}

	var (
		voters, healthy int
		selfHealthy     bool
		unhealthy       []string
	)

	for i, m := range members {
		if m.GetIsLearner() {
			continue
		}

		voters++

		if err := t.probeEtcdMember(ctx, target.client, m); err != nil {
			unhealthy = append(unhealthy, fmt.Sprintf("%s: %v", memberName(m), err))

			continue
		}

		healthy++

		if i == self {
			selfHealthy = true
		}
	}

	quorum := voters/2 + 1

	after := healthy
	if selfHealthy {
		after--
	}

	summary := fmt.Sprintf("etcd has %d voting members, %d healthy; %d would stay healthy, quorum needs %d", voters, healthy, after, quorum)
	if len(unhealthy) > 0 {
		summary += " (unhealthy: " + strings.Join(unhealthy, "; ") + ")"
	}

	if after < quorum {
		return "", fmt.Errorf("refusing to reboot %s: it would break etcd quorum: %s", target.label(), summary)
	}

	return summary, nil
}

// probeEtcdMember asks the member's node for its etcd status.
func (t *TalosTools) probeEtcdMember(ctx context.Context, c talos.Client, m *machineapi.EtcdMember) error {
	host := memberHost(m)
	if host == "" {
		return fmt.Errorf("no member address")
	}

	ctx, cancel := context.WithTimeout(ctx, etcdProbeTimeout)
	defer cancel()

	resp, err := c.EtcdStatus(client.WithNode(ctx, host))
	if err != nil {
		return fmt.Errorf("status: %s", grpcMessage(err))
	}

	for _, msg := range resp.GetMessages() {
		if errs := msg.GetMemberStatus().GetErrors(); len(errs) > 0 {
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
	}

	return nil
}

// isTarget reports whether an etcd member is the node being rebooted, by
// hostname or by an address in its peer or client URLs.
func isTarget(m *machineapi.EtcdMember, node *talos.ResolvedNode) bool {
	if node.Name != "" && strings.EqualFold(m.GetHostname(), node.Name) {
		return true
	}

	for _, u := range slices.Concat(m.GetPeerUrls(), m.GetClientUrls()) {
		if urlHost(u) == node.Address {
			return true
		}
	}

	return false
}

// memberHost returns the node address of an etcd member, from its peer URLs
// first, skipping loopback.
func memberHost(m *machineapi.EtcdMember) string {
	for _, u := range slices.Concat(m.GetPeerUrls(), m.GetClientUrls()) {
		if h := urlHost(u); h != "" {
			if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
				continue
			}

			return h
		}
	}

	return ""
}

func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}

	return u.Hostname()
}

func memberName(m *machineapi.EtcdMember) string {
	if m.GetHostname() != "" {
		return m.GetHostname()
	}

	return memberHost(m)
}
