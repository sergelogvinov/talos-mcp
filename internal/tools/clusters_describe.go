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
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/resources/cluster"
	"github.com/siderolabs/talos/pkg/machinery/resources/k8s"
	talosruntime "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ToolClustersDescribe is the name of the cluster describe tool.
const ToolClustersDescribe = "talos_clusters_describe"

// apid methods called by talos_clusters_describe, besides ServiceList,
// EtcdMemberList and COSI List.
const (
	apiVersion    = "/machine.MachineService/Version"
	apiSystemStat = "/machine.MachineService/SystemStat"
	apiMemory     = "/machine.MachineService/Memory"
	apiCOSIGet    = "/cosi.resource.State/Get"
)

// describeFanOut is how many nodes are queried at once.
const describeFanOut = 8

// clustersDescribeInput is the input of the talos_clusters_describe tool.
type clustersDescribeInput struct {
	Cluster string `json:"cluster,omitempty" jsonschema:"Cluster name (talosconfig context); default is current"`
}

// RegisterClustersDescribe registers the cluster describe tool.
func (t *TalosTools) RegisterClustersDescribe(srv *mcp.Server) {
	addTool(srv,
		&mcp.Tool{
			Name: ToolClustersDescribe,
			Description: "Describe a Talos cluster through the Talos API: every node with its role, Talos and kubelet " +
				"versions, reachability, machine stage and readiness, uptime, CPU and memory, and unhealthy services, " +
				"plus the etcd members and cluster name. Nodes that don't answer are listed as unreachable.",
			Annotations: &mcp.ToolAnnotations{
				IdempotentHint: true,
				ReadOnlyHint:   true,
				OpenWorldHint:  new(true),
			},
		},
		t.handlerClustersDescribe,
	)

	t.track(toolSpec{name: ToolClustersDescribe, minRole: talos.RoleReader, apis: []string{
		apiVersion, apiServiceList, apiSystemStat, apiMemory, apiEtcdMemberList, apiCOSIList, apiCOSIGet,
	}})
}

func (t *TalosTools) handlerClustersDescribe(ctx context.Context, _ *mcp.CallToolRequest, in clustersDescribeInput) (*mcp.CallToolResult, ClustersDescribeResult, error) {
	ctx, cancel := withTimeout(ctx, aggregateTimeout)
	defer cancel()

	result, err := t.ClustersDescribe(ctx, in.Cluster)
	if err != nil {
		return nil, ClustersDescribeResult{}, err
	}

	return textResult(result)
}

// ClustersDescribe returns a health and inventory snapshot of a cluster.
// The node list comes from Pool.Members, and each node is queried on its
// own with client.WithNode (machinery deprecates WithNodes). A node that
// fails only adds a warning. It never uses the discovery service.
func (t *TalosTools) ClustersDescribe(ctx context.Context, clusterName string) (*ClustersDescribeResult, error) {
	name, err := t.pool.Resolve(clusterName)
	if err != nil {
		return nil, err
	}

	if err := t.pool.Require(name, talos.RoleReader); err != nil {
		return nil, err
	}

	endpoints, _, err := t.pool.ContextInfo(name)
	if err != nil {
		return nil, err
	}

	targets, err := t.pool.ResolveAllNodes(ctx, name)
	if err != nil {
		return nil, err
	}

	if len(targets.Nodes) == 0 {
		return nil, fmt.Errorf("cluster %s: no nodes to describe%s", name, warningsNote(targets.Warnings))
	}

	c, err := t.pool.Client(ctx, name)
	if err != nil {
		return nil, err
	}

	d := &describe{
		t:      t,
		client: c,
		result: &ClustersDescribeResult{
			Cluster:    name,
			Endpoints:  endpoints,
			NodeSource: string(targets.Source),
			Nodes:      make([]NodeSummary, 0, len(targets.Nodes)+len(targets.Unaddressed)),
			Warnings:   targets.Warnings,
		},
	}

	for _, n := range targets.Nodes {
		node := NodeSummary{Address: n.Address, Role: n.MachineType}
		if n.Name != n.Address {
			node.Hostname = n.Name
		}

		d.result.Nodes = append(d.result.Nodes, node)
	}

	// Members without an address are listed, unreachable, after the others.
	for _, m := range targets.Unaddressed {
		d.result.Nodes = append(d.result.Nodes, NodeSummary{Hostname: m.Hostname, Role: m.MachineType})
	}

	nodeWarnings, firstErr := d.describeNodes(ctx, len(targets.Nodes))

	d.result.Count = len(d.result.Nodes)

	for _, node := range d.result.Nodes {
		if node.Reachable {
			d.result.Reachable++
		}
	}

	if d.result.Reachable == 0 {
		return nil, t.noNodeAnswered(ctx, name, "Version", firstErr, targets.ReadErr, nodeWarnings)
	}

	d.result.Warnings = append(d.result.Warnings, nodeWarnings...)

	d.clusterWide(ctx)

	d.result.KubernetesVersion = clusterKubernetesVersion(d.result.Nodes)

	return d.result, nil
}

// describe is the state of one talos_clusters_describe call.
type describe struct {
	t      *TalosTools
	client talos.Client
	result *ClustersDescribeResult
}

// describeNodes queries the first n entries of result.Nodes (the ones with
// an address), describeFanOut at a time. Each goroutine writes only its own
// entry. It returns the warnings in node order and the first node error.
func (d *describe) describeNodes(ctx context.Context, n int) ([]string, error) {
	warnings := make([][]string, n)
	errs := make([]error, n)
	sem := make(chan struct{}, describeFanOut)

	var wg sync.WaitGroup

	for i := range n {
		node := &d.result.Nodes[i]

		wg.Go(func() {
			// A node still waiting for a slot when the call's deadline
			// passes is not queried at all.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				warnings[i], errs[i] = []string{fmt.Sprintf("node %s: not queried, the call's time limit was reached", nodeSummaryLabel(node))}, ctx.Err()

				return
			}
			defer func() { <-sem }()

			warnings[i], errs[i] = d.describeNode(ctx, node)
		})
	}

	wg.Wait()

	var (
		all   []string
		first error
	)

	for i := range n {
		all = append(all, warnings[i]...)
		first = cmp.Or(first, errs[i])
	}

	return all, first
}

// describeNode fills one node. A failed Version call marks the node
// unreachable and is returned; later failures are only warnings.
func (d *describe) describeNode(ctx context.Context, node *NodeSummary) ([]string, error) {
	ctx = client.WithNode(ctx, node.Address)
	label := nodeSummaryLabel(node)

	var warnings []string

	warn := func(what string, err error) {
		warnings = append(warnings, fmt.Sprintf("node %s: %s: %s", label, what, grpcMessage(err)))
	}

	version, err := d.client.Version(ctx)
	if err != nil {
		warn("unreachable", err)

		return warnings, err
	}

	node.Reachable = true

	for _, msg := range version.GetMessages() {
		node.TalosVersion = msg.GetVersion().GetTag()
	}

	if resp, err := d.client.ServiceList(ctx); err != nil {
		warn("services", err)
	} else {
		for _, msg := range resp.GetMessages() {
			node.UnhealthyServices = append(node.UnhealthyServices, d.unhealthyServices(msg.GetServices())...)
		}
	}

	var (
		stat *machineapi.SystemStat
		mem  *machineapi.MemInfo
	)

	if resp, err := d.client.SystemStat(ctx); err != nil {
		warn("system stats", err)
	} else if msgs := resp.GetMessages(); len(msgs) > 0 {
		stat = msgs[0]
	}

	if resp, err := d.client.Memory(ctx); err != nil {
		warn("memory", err)
	} else if msgs := resp.GetMessages(); len(msgs) > 0 {
		mem = msgs[0].GetMeminfo()
	}

	if stat.GetBootTime() > 0 {
		node.Uptime = formatUptime(d.t.pool.Now().Sub(time.Unix(int64(stat.GetBootTime()), 0))) //nolint:gosec
	}

	node.Resources = formatResources(stat, mem)

	ns := d.t.readNodeStatus(ctx, d.client.State(), warn)
	node.Stage, node.Ready, node.UnmetConditions, node.KubernetesVersion = ns.stage, ns.ready, ns.unmet, ns.kubelet

	return warnings, nil
}

// nodeStatus is the machine status and kubelet version of a node.
type nodeStatus struct {
	stage   string
	ready   bool
	unmet   []string
	kubelet string
}

// readNodeStatus reads the MachineStatus and KubeletStatus of the node in
// ctx. A resource the node doesn't have is not a warning.
func (t *TalosTools) readNodeStatus(ctx context.Context, st state.State, warn func(what string, err error)) nodeStatus {
	var ns nodeStatus

	ms, err := safe.StateGetByID[*talosruntime.MachineStatus](ctx, st, talosruntime.MachineStatusID)

	switch {
	case err == nil:
		spec := ms.TypedSpec()
		ns.stage = spec.Stage.String()
		ns.ready = spec.Status.Ready

		for _, c := range spec.Status.UnmetConditions {
			ns.unmet = append(ns.unmet, t.sanitizer.Sanitize(c.Name+": "+c.Reason))
		}
	case !notFound(err):
		warn("machine status", err)
	}

	kubelet, err := safe.StateGetByID[*k8s.KubeletStatus](ctx, st, k8s.KubeletID)

	switch {
	case err == nil:
		ns.kubelet = imageTag(kubelet.TypedSpec().Image)
	case !notFound(err):
		warn("kubelet status", err)
	}

	return ns
}

// clusterWide reads the etcd members and the cluster name through a
// reachable control plane node. When no control plane node is known
// (talosconfig source), etcd is skipped and the name is read from any
// reachable node.
func (d *describe) clusterWide(ctx context.Context) {
	var controlPlane, anyNode string

	for _, node := range d.result.Nodes {
		if !node.Reachable {
			continue
		}

		anyNode = cmp.Or(anyNode, node.Address)

		if node.Role == talos.MachineTypeControlPlane {
			controlPlane = node.Address

			break
		}
	}

	if controlPlane != "" {
		d.etcd(client.WithNode(ctx, controlPlane), controlPlane)
	}

	addr := cmp.Or(controlPlane, anyNode)

	info, err := safe.StateGetByID[*cluster.Info](client.WithNode(ctx, addr), d.client.State(), cluster.InfoID)

	switch {
	case err == nil:
		d.result.ClusterName = info.TypedSpec().ClusterName
	case !notFound(err):
		d.warnf("cluster info (through %s): %s", addr, grpcMessage(err))
	}
}

// etcd lists the etcd members through one control plane node.
func (d *describe) etcd(ctx context.Context, addr string) {
	resp, err := d.client.EtcdMemberList(ctx, &machineapi.EtcdMemberListRequest{})
	if err != nil {
		d.warnf("etcd members (through %s): %s", addr, grpcMessage(err))

		return
	}

	for _, msg := range resp.GetMessages() {
		for _, m := range msg.GetMembers() {
			d.result.Etcd = append(d.result.Etcd, EtcdMemberSummary{
				Hostname: m.GetHostname(),
				ID:       fmt.Sprintf("%x", m.GetId()),
				Learner:  m.GetIsLearner(),
			})
		}
	}

	slices.SortFunc(d.result.Etcd, func(a, b EtcdMemberSummary) int { return strings.Compare(a.Hostname, b.Hostname) })
}

func (d *describe) warnf(format string, args ...any) {
	d.result.Warnings = append(d.result.Warnings, fmt.Sprintf(format, args...))
}

// unhealthyServices lists the services that are not running, or whose
// health check fails.
func (d *describe) unhealthyServices(services []*machineapi.ServiceInfo) []string {
	var out []string

	for _, s := range services {
		h := s.GetHealth()

		switch {
		case s.GetState() != "Running":
			out = append(out, s.GetId()+" ("+s.GetState()+")")
		case h != nil && !h.GetUnknown() && !h.GetHealthy():
			out = append(out, d.t.sanitizer.Sanitize(s.GetId()+" (unhealthy: "+h.GetLastMessage()+")"))
		}
	}

	slices.Sort(out)

	return out
}

// nodeSummaryLabel is "hostname (address)", or the address.
func nodeSummaryLabel(n *NodeSummary) string {
	return nodeLabel(talos.ResolvedNode{Address: n.Address, Name: cmp.Or(n.Hostname, n.Address)})
}

// clusterKubernetesVersion returns the kubelet version of the control plane
// nodes, or of all nodes when no control plane version is known. Different
// versions are sorted and joined with commas.
func clusterKubernetesVersion(nodes []NodeSummary) string {
	var versions []string

	for _, role := range []string{talos.MachineTypeControlPlane, ""} {
		for _, n := range nodes {
			if n.KubernetesVersion != "" && (role == "" || n.Role == role) {
				versions = append(versions, n.KubernetesVersion)
			}
		}

		if len(versions) > 0 {
			break
		}
	}

	slices.Sort(versions)

	return strings.Join(slices.Compact(versions), ", ")
}

// imageTag returns the tag of an image reference, e.g. v1.34.1 for
// ghcr.io/siderolabs/kubelet:v1.34.1@sha256:...
func imageTag(image string) string {
	image, _, _ = strings.Cut(image, "@")

	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image[i:], "/") {
		return ""
	}

	return image[i+1:]
}

// formatUptime renders a duration as 3d4h5m, 4h5m or 5m.
func formatUptime(d time.Duration) string {
	if d < 0 {
		return ""
	}

	minutes := int(d / time.Minute)
	days, hours, minutes := minutes/(24*60), minutes/60%24, minutes%60

	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh%dm", days, hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// formatResources renders "cpu=4, memory=7.6GiB (used=2.1GiB)" from the
// values that are known.
func formatResources(stat *machineapi.SystemStat, mem *machineapi.MemInfo) string {
	var parts []string

	if n := len(stat.GetCpu()); n > 0 {
		parts = append(parts, fmt.Sprintf("cpu=%d", n))
	}

	if total := mem.GetMemtotal(); total > 0 {
		used := total - min(mem.GetMemavailable(), total)
		parts = append(parts, fmt.Sprintf("memory=%s (used=%s)", gib(total), gib(used)))
	}

	return strings.Join(parts, ", ")
}

// gib renders KiB as GiB with one decimal.
func gib(kib uint64) string {
	return fmt.Sprintf("%.1fGiB", float64(kib)/(1024*1024))
}

// notFound reports whether a COSI read failed because the resource does not
// exist, or because the node's Talos version does not know its type. In the
// second case the access policy answers PermissionDenied "resource type %q
// is not supported" (KubeletStatus is new in Talos v1.14.0).
func notFound(err error) bool {
	if state.IsNotFoundError(err) || status.Code(err) == codes.NotFound {
		return true
	}

	return status.Code(err) == codes.PermissionDenied && strings.HasSuffix(status.Convert(err).Message(), "is not supported")
}
