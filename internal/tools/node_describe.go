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
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/sergelogvinov/talos-mcp/pkg/formatter"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
)

// ToolNodeDescribe is the name of the node describe tool.
const ToolNodeDescribe = "talos_node_describe"

// apid methods called by talos_node_describe only.
const (
	apiLoadAvg   = "/machine.MachineService/LoadAvg"
	apiMounts    = "/machine.MachineService/Mounts"
	apiProcesses = "/machine.MachineService/Processes"
)

// Limits of talos_node_describe.
const (
	defaultDescribeLogLines   = 20
	maxDescribeLogLines       = 200
	maxDescribeLogServices    = 5
	defaultDescribeEventLimit = 20
	topProcesses              = 5
	// fallbackLogService is the service whose logs are shown when every
	// service is healthy.
	fallbackLogService = "machined"
)

// NodeDescribeInput is the input of the talos_node_describe tool.
type NodeDescribeInput struct {
	Cluster     string   `json:"cluster,omitempty" jsonschema:"Cluster name; default is current"`
	Node        string   `json:"node,omitempty" jsonschema:"Target node (IP or hostname); default is the single talosconfig node"`
	Logs        []string `json:"logs,omitempty" jsonschema:"Services to show the last log lines of, at most 5; default the unhealthy services, or machined when all are healthy"`
	LogLines    int      `json:"log_lines,omitempty" jsonschema:"Log lines per service (default 20, max 200)"`
	EventsSince string   `json:"events_since,omitempty" jsonschema:"Only events newer than this duration, e.g. 15m, 2h (default 1h)"`
	EventLimit  int      `json:"event_limit,omitempty" jsonschema:"Maximum number of events (default 20, max 500)"`
}

// RegisterNodeDescribe registers the node describe tool.
func (t *TalosTools) RegisterNodeDescribe(srv *mcp.Server) {
	addTool(srv,
		&mcp.Tool{
			Name: ToolNodeDescribe,
			Description: "Describe one Talos node: versions, platform, machine stage and readiness, uptime, " +
				"resource usage (CPU, load, memory, disks, top processes), the state and health of every service, " +
				"recent runtime events, and the last log lines of the unhealthy services (or of chosen ones). " +
				"Secrets in the output are masked.",
			Annotations: &mcp.ToolAnnotations{
				IdempotentHint: true,
				ReadOnlyHint:   true,
				OpenWorldHint:  new(true),
			},
		},
		t.handlerNodeDescribe,
	)

	t.track(toolSpec{name: ToolNodeDescribe, minRole: talos.RoleReader, apis: []string{
		apiVersion, apiServiceList, apiSystemStat, apiMemory, apiLoadAvg, apiMounts, apiProcesses,
		apiLogs, apiEvents, apiCOSIList, apiCOSIGet,
	}})
}

func (t *TalosTools) handlerNodeDescribe(ctx context.Context, _ *mcp.CallToolRequest, in NodeDescribeInput) (*mcp.CallToolResult, NodeDescribeResult, error) {
	ctx, cancel := withTimeout(ctx, aggregateTimeout)
	defer cancel()

	result, err := t.NodeDescribe(ctx, in)
	if err != nil {
		return nil, NodeDescribeResult{}, err
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: nodeDescribeText(result)}},
	}, *result, nil
}

// NodeDescribe returns the details of one node. Only a failed Version call
// fails the tool. Every later failure is a warning.
func (t *TalosTools) NodeDescribe(ctx context.Context, in NodeDescribeInput) (*NodeDescribeResult, error) {
	logLines, err := describeLogLines(in.LogLines)
	if err != nil {
		return nil, err
	}

	logServices, err := describeLogServices(in.Logs)
	if err != nil {
		return nil, err
	}

	since, err := eventsSince(in.EventsSince)
	if err != nil {
		return nil, err
	}

	limit := defaultDescribeEventLimit
	if in.EventLimit != 0 {
		if limit, err = eventLimit(in.EventLimit); err != nil {
			return nil, err
		}
	}

	target, err := t.resolveNodeTarget(ctx, in.Cluster, in.Node, talos.RoleReader)
	if err != nil {
		return nil, err
	}

	nodeCtx := target.nodeContext(ctx)

	version, err := target.client.Version(nodeCtx)
	if err != nil {
		return nil, t.talosError(nodeCtx, err, target.cluster, "Version")
	}

	// The events backlog is read while the other calls run: the read ends
	// only after the stream is idle for eventsIdleTimeout.
	events := make(chan nodeEvents, 1)

	go func() {
		events <- drainEvents(ctx, target.client, *target.node, limit, []client.EventsOptionFunc{client.WithTailDuration(since)})
	}()

	d := &nodeDescribe{
		t:      t,
		target: target,
		result: &NodeDescribeResult{
			Cluster:     target.cluster,
			Node:        target.node.Address,
			Hostname:    target.nodeName(),
			Role:        target.node.MachineType,
			Services:    []ServiceStatus{},
			Events:      []EventSummary{},
			EventsSince: since.String(),
			Logs:        []ServiceLog{},
			Warnings:    slices.Clone(target.node.Warnings),
		},
	}

	d.version(version)
	services := d.services(nodeCtx)
	d.resources(nodeCtx)

	ns := t.readNodeStatus(nodeCtx, target.client.State(), d.warn)
	d.result.Stage, d.result.Ready, d.result.UnmetConditions, d.result.KubernetesVersion = ns.stage, ns.ready, ns.unmet, ns.kubelet

	d.identity(nodeCtx)

	d.logs(nodeCtx, services, logServices, logLines)
	d.events(<-events, limit)

	return d.result, nil
}

// nodeDescribe is the state of one talos_node_describe call.
type nodeDescribe struct {
	t      *TalosTools
	target *nodeTarget
	result *NodeDescribeResult
}

func (d *nodeDescribe) warn(what string, err error) {
	d.result.Warnings = append(d.result.Warnings, fmt.Sprintf("%s: %s", what, grpcMessage(err)))
}

// version fills the Talos version, architecture and platform.
func (d *nodeDescribe) version(resp *machineapi.VersionResponse) {
	for _, msg := range resp.GetMessages() {
		d.result.TalosVersion = msg.GetVersion().GetTag()
		d.result.Arch = msg.GetVersion().GetArch()
		d.result.Platform = msg.GetPlatform().GetName()
	}
}

// identity reads the hostname and role when they are still unknown, as for
// a node given by an address that the Members lookup does not resolve.
func (d *nodeDescribe) identity(ctx context.Context) {
	st := d.target.client.State()

	if d.result.Hostname == "" {
		hostname, err := safe.StateGetByID[*network.HostnameStatus](ctx, st, network.HostnameID)

		switch {
		case err == nil:
			d.result.Hostname = hostname.TypedSpec().Hostname
		case !notFound(err):
			d.warn("hostname", err)
		}
	}

	if d.result.Role == "" {
		mt, err := safe.StateGetByID[*config.MachineType](ctx, st, config.MachineTypeID)

		switch {
		case err == nil:
			d.result.Role = mt.MachineType().String()
		case !notFound(err):
			d.warn("machine type", err)
		}
	}
}

// services lists every service with its state and health, sorted by id,
// and returns the raw list.
func (d *nodeDescribe) services(ctx context.Context) []*machineapi.ServiceInfo {
	resp, err := d.target.client.ServiceList(ctx)
	if err != nil {
		d.warn("services", err)

		return nil
	}

	var all []*machineapi.ServiceInfo

	for _, msg := range resp.GetMessages() {
		all = append(all, msg.GetServices()...)
	}

	slices.SortFunc(all, func(a, b *machineapi.ServiceInfo) int { return strings.Compare(a.GetId(), b.GetId()) })

	for _, s := range all {
		status := ServiceStatus{ID: s.GetId(), State: s.GetState()}

		if h := s.GetHealth(); h != nil && !h.GetUnknown() {
			status.Health = "healthy"
			if !h.GetHealthy() {
				status.Health = d.t.sanitizer.Sanitize("unhealthy: " + h.GetLastMessage())
			}
		}

		if evs := s.GetEvents().GetEvents(); len(evs) > 0 {
			last := evs[len(evs)-1]
			status.LastEvent = strings.Join(strings.Fields(d.t.sanitizer.Sanitize(last.GetMsg())), " ")

			if ts := last.GetTs(); ts != nil {
				status.Since = ts.AsTime().UTC().Format(time.RFC3339)
			}
		}

		d.result.Services = append(d.result.Services, status)
	}

	return all
}

// resources fills the resource usage from SystemStat, Memory, LoadAvg,
// Mounts and Processes.
func (d *nodeDescribe) resources(ctx context.Context) {
	c, r := d.target.client, &d.result.Resources

	if resp, err := c.SystemStat(ctx); err != nil {
		d.warn("system stats", err)
	} else if msgs := resp.GetMessages(); len(msgs) > 0 {
		stat := msgs[0]
		r.CPUs = len(stat.GetCpu())
		r.Processes = fmt.Sprintf("running=%d, blocked=%d", stat.GetProcessRunning(), stat.GetProcessBlocked())

		if boot := stat.GetBootTime(); boot > 0 {
			at := time.Unix(int64(boot), 0) //nolint:gosec
			d.result.BootTime = at.UTC().Format(time.RFC3339)
			d.result.Uptime = formatUptime(d.t.pool.Now().Sub(at))
		}
	}

	if resp, err := c.LoadAvg(ctx); err != nil {
		d.warn("load average", err)
	} else if msgs := resp.GetMessages(); len(msgs) > 0 {
		r.LoadAverage = fmt.Sprintf("%.2f, %.2f, %.2f", msgs[0].GetLoad1(), msgs[0].GetLoad5(), msgs[0].GetLoad15())
	}

	if resp, err := c.Memory(ctx); err != nil {
		d.warn("memory", err)
	} else if msgs := resp.GetMessages(); len(msgs) > 0 {
		mem := msgs[0].GetMeminfo()
		r.Memory = usage(mem.GetMemtotal()*1024, mem.GetMemavailable()*1024)
		r.Swap = usage(mem.GetSwaptotal()*1024, mem.GetSwapfree()*1024)
	}

	if resp, err := c.Mounts(ctx); err != nil {
		d.warn("disks", err)
	} else {
		for _, msg := range resp.GetMessages() {
			r.Disks = append(r.Disks, diskUsage(msg.GetStats())...)
		}
	}

	if resp, err := c.Processes(ctx); err != nil {
		d.warn("processes", err)
	} else {
		for _, msg := range resp.GetMessages() {
			r.TopProcesses = append(r.TopProcesses, d.topProcesses(msg.GetProcesses())...)
		}
	}
}

// logs reads the last lines of the chosen services: the requested ones, or
// the unhealthy ones, or machined when all are healthy.
func (d *nodeDescribe) logs(ctx context.Context, services []*machineapi.ServiceInfo, requested []string, lines int) {
	ids := make([]string, 0, len(services))
	for _, s := range services {
		ids = append(ids, s.GetId())
	}

	chosen := requested
	if len(chosen) == 0 {
		for _, s := range services {
			if serviceUnhealthy(s) && len(chosen) < maxDescribeLogServices {
				chosen = append(chosen, s.GetId())
			}
		}
	}

	if len(chosen) == 0 && (len(ids) == 0 || slices.Contains(ids, fallbackLogService)) {
		chosen = []string{fallbackLogService}
	}

	for _, id := range chosen {
		// An unknown id is only known as such when the list was read.
		if len(ids) > 0 && !slices.Contains(ids, id) {
			d.result.Warnings = append(d.result.Warnings, fmt.Sprintf("logs: unknown service %q; services: %s", id, strings.Join(ids, ", ")))

			continue
		}

		log := ServiceLog{Service: id, Lines: []string{}}

		got, truncated, err := d.t.tailLog(ctx, d.target.client, constants.SystemContainerdNamespace, common.ContainerDriver_CONTAINERD, id, lines, "")
		if err != nil {
			log.Error = grpcMessage(err)
		} else {
			log.Lines, log.Count, log.Truncated = got, len(got), truncated
		}

		d.result.Logs = append(d.result.Logs, log)
	}
}

// events adds the events read from the node, newest first, at most limit.
func (d *nodeDescribe) events(r nodeEvents, limit int) {
	switch {
	case r.err != nil:
		d.warn("events", r.err)
	case r.incomplete:
		d.result.Warnings = append(d.result.Warnings, fmt.Sprintf("events: still arriving after %s, the list may be incomplete", eventsDrainTimeout))
	}

	for _, ev := range r.raw {
		d.result.Events = append(d.result.Events, d.t.eventSummary(ev, *d.target.node))
	}

	slices.SortFunc(d.result.Events, func(a, b EventSummary) int {
		return cmp.Or(strings.Compare(b.Time, a.Time), strings.Compare(b.ID, a.ID))
	})

	d.result.EventsTruncated = r.dropped || len(d.result.Events) > limit
	if len(d.result.Events) > limit {
		d.result.Events = d.result.Events[:limit]
	}
}

// topProcesses returns the processes using the most memory. The command
// line is left out: it can hold secrets that the sanitizer doesn't know.
func (d *nodeDescribe) topProcesses(procs []*machineapi.ProcessInfo) []ProcessUsage {
	procs = slices.Clone(procs)
	slices.SortFunc(procs, func(a, b *machineapi.ProcessInfo) int {
		return cmp.Or(cmp.Compare(b.GetResidentMemory(), a.GetResidentMemory()), cmp.Compare(a.GetPid(), b.GetPid()))
	})

	out := make([]ProcessUsage, 0, topProcesses)

	for _, p := range procs[:min(topProcesses, len(procs))] {
		out = append(out, ProcessUsage{
			PID:     p.GetPid(),
			Command: d.t.sanitizer.Sanitize(p.GetCommand()),
			Memory:  humanBytes(p.GetResidentMemory()),
			CPUTime: time.Duration(p.GetCpuTime() * float64(time.Second)).Round(time.Second).String(),
		})
	}

	return out
}

// diskUsage lists the block device filesystems, one entry per device: the
// shortest mount point wins over its bind mounts.
func diskUsage(stats []*machineapi.MountStat) []DiskUsage {
	byDevice := map[string]*machineapi.MountStat{}

	for _, s := range stats {
		if !strings.HasPrefix(s.GetFilesystem(), "/dev/") || s.GetSize() == 0 {
			continue
		}

		if prev, ok := byDevice[s.GetFilesystem()]; !ok || len(s.GetMountedOn()) < len(prev.GetMountedOn()) {
			byDevice[s.GetFilesystem()] = s
		}
	}

	out := make([]DiskUsage, 0, len(byDevice))

	for _, s := range byDevice {
		used := s.GetSize() - min(s.GetAvailable(), s.GetSize())
		out = append(out, DiskUsage{
			MountedOn:   s.GetMountedOn(),
			Device:      s.GetFilesystem(),
			Size:        humanBytes(s.GetSize()),
			Used:        humanBytes(used),
			UsedPercent: fmt.Sprintf("%d%%", used*100/s.GetSize()),
		})
	}

	slices.SortFunc(out, func(a, b DiskUsage) int { return strings.Compare(a.MountedOn, b.MountedOn) })

	return out
}

// serviceUnhealthy reports whether a service is not running, or fails its
// health check.
func serviceUnhealthy(s *machineapi.ServiceInfo) bool {
	h := s.GetHealth()

	return s.GetState() != "Running" || (h != nil && !h.GetUnknown() && !h.GetHealthy())
}

// usage renders "2.0GiB of 8.0GiB (25%)" from a total and the available
// bytes, or nothing without a total.
func usage(total, available uint64) string {
	if total == 0 {
		return ""
	}

	used := total - min(available, total)

	return fmt.Sprintf("%s of %s (%d%%)", humanBytes(used), humanBytes(total), used*100/total)
}

// humanBytes renders bytes with a binary unit and one decimal.
func humanBytes(b uint64) string {
	const unit = 1024

	if b < unit {
		return fmt.Sprintf("%dB", b)
	}

	v, exp := float64(b)/unit, 0
	for v >= unit && exp < 4 {
		v /= unit
		exp++
	}

	return fmt.Sprintf("%.1f%ciB", v, "KMGTP"[exp])
}

// describeLogLines validates the log_lines argument: 0 means
// defaultDescribeLogLines.
func describeLogLines(n int) (int, error) {
	switch {
	case n == 0:
		return defaultDescribeLogLines, nil
	case n < 0 || n > maxDescribeLogLines:
		return 0, fmt.Errorf("invalid log_lines %d: must be between 1 and %d", n, maxDescribeLogLines)
	default:
		return n, nil
	}
}

// describeLogServices validates the logs argument: trimmed, without
// duplicates, at most maxDescribeLogServices.
func describeLogServices(ids []string) ([]string, error) {
	var out []string

	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}

	if len(out) > maxDescribeLogServices {
		return nil, fmt.Errorf("invalid logs: at most %d services, got %d", maxDescribeLogServices, len(out))
	}

	return out, nil
}

// nodeDescribeText renders the node fields, resources and services with the
// formatter, then the events as aligned lines and each log as raw lines.
func nodeDescribeText(r *NodeDescribeResult) string {
	base := *r
	base.Events, base.EventsSince, base.EventsTruncated, base.Logs = nil, "", false, nil

	var b bytes.Buffer

	b.WriteString(formatter.ToText(&base))

	fmt.Fprintf(&b, "\n\n### Events since %s: %d", r.EventsSince, len(r.Events))

	if r.EventsTruncated {
		b.WriteString(" (truncated)")
	}

	b.WriteByte('\n')

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, e := range r.Events {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", cmp.Or(e.Time, "-"), e.Type, e.Summary)
	}

	tw.Flush() //nolint:errcheck

	for _, l := range r.Logs {
		fmt.Fprintf(&b, "\n### %s logs: ", l.Service)

		if l.Error != "" {
			fmt.Fprintf(&b, "error: %s\n", l.Error)

			continue
		}

		fmt.Fprintf(&b, "%d lines", l.Count)

		if l.Truncated {
			b.WriteString(" (truncated)")
		}

		b.WriteByte('\n')

		for _, line := range l.Lines {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	return strings.TrimRight(b.String(), "\n")
}
