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

// ClustersListResult is the structured output of the talos_clusters_list tool.
type ClustersListResult struct {
	Current  string           `json:"current" jsonschema:"Default cluster"`
	Count    int              `json:"count" jsonschema:"Number of configured clusters"`
	Clusters []ClusterSummary `json:"clusters" jsonschema:"Configured clusters"`
}

// ClusterSummary describes one configured cluster, without network calls.
type ClusterSummary struct {
	Name        string   `json:"name" jsonschema:"Cluster name (talosconfig context)"`
	Endpoints   []string `json:"endpoints" jsonschema:"Talos API endpoints"`
	Nodes       []string `json:"nodes,omitempty" jsonschema:"Default target nodes"`
	Current     bool     `json:"current" jsonschema:"Whether this is the default cluster"`
	CertExpires string   `json:"cert_expires,omitempty" jsonschema:"Client certificate expiry (RFC3339), set when it is expired or expires within 7 days"`
	Discovery   string   `json:"discovery,omitempty" jsonschema:"Discovery service endpoint, when configured"`
	Role        string   `json:"role" jsonschema:"Credential role: reader or operator"`
	Tools       []string `json:"tools" jsonschema:"Tools usable on this cluster with its credential"`
}

// ClustersMembersResult is the structured output of the talos_clusters_members tool.
type ClustersMembersResult struct {
	Cluster   string          `json:"cluster" jsonschema:"Cluster name (talosconfig context)"`
	ClusterID string          `json:"cluster_id" jsonschema:"Discovery cluster ID"`
	Endpoint  string          `json:"endpoint" jsonschema:"Discovery service endpoint queried"`
	Count     int             `json:"count" jsonschema:"Number of members"`
	Members   []MemberSummary `json:"members" jsonschema:"Members registered with the discovery service"`
	Warnings  []string        `json:"warnings,omitempty" jsonschema:"Warnings"`
}

// MemberSummary is one node as registered with the discovery service. The
// jsonschema text is also the text column header, so it stays short.
type MemberSummary struct {
	NodeID          string        `json:"node_id" jsonschema:"Node ID"`
	Hostname        string        `json:"hostname" jsonschema:"Hostname"`
	NodeName        string        `json:"nodename,omitempty" jsonschema:"Kubernetes node name"`
	Role            string        `json:"role" jsonschema:"Role (controlplane, worker; empty for KubeSpan-only)"`
	OperatingSystem string        `json:"operating_system,omitempty" jsonschema:"OS"`
	Addresses       []string      `json:"addresses" jsonschema:"Addresses"`
	Endpoints       []string      `json:"endpoints,omitempty" jsonschema:"KubeSpan endpoints"`
	APIServerPort   *int          `json:"apiserver_port,omitempty" jsonschema:"API server port"`
	KubeSpan        *KubeSpanInfo `json:"kubespan,omitempty" jsonschema:"KubeSpan"`
}

// KubeSpanInfo is the KubeSpan peer data of a member.
type KubeSpanInfo struct {
	Address             string   `json:"address" jsonschema:"WireGuard address"`
	PublicKey           string   `json:"public_key" jsonschema:"WireGuard public key"`
	AdditionalAddresses []string `json:"additional_addresses,omitempty" jsonschema:"Routed prefixes"`
}

// NodeLogsResult is the structured output of the talos_node_logs tool.
type NodeLogsResult struct {
	Cluster   string   `json:"cluster" jsonschema:"Cluster name"`
	Node      string   `json:"node" jsonschema:"Node address the logs were read from"`
	NodeName  string   `json:"node_name,omitempty" jsonschema:"Node hostname, when the node was given by name"`
	Service   string   `json:"service" jsonschema:"Service or container id"`
	Lines     []string `json:"lines" jsonschema:"Log lines, oldest first, sanitized"`
	Count     int      `json:"count" jsonschema:"Number of lines returned"`
	Truncated bool     `json:"truncated" jsonschema:"More lines were available than returned, or a line was cut at 4 KiB"`
	Warnings  []string `json:"warnings,omitempty" jsonschema:"Warnings"`
}

// NodeDmesgResult is the structured output of the talos_node_dmesg tool.
type NodeDmesgResult struct {
	Cluster   string   `json:"cluster" jsonschema:"Cluster name"`
	Node      string   `json:"node" jsonschema:"Node address the kernel log was read from"`
	NodeName  string   `json:"node_name,omitempty" jsonschema:"Node hostname, when the node was given by name"`
	Lines     []string `json:"lines" jsonschema:"Kernel log lines (<time> <facility>.<priority> <message>), oldest first, sanitized"`
	Count     int      `json:"count" jsonschema:"Number of lines returned"`
	Truncated bool     `json:"truncated" jsonschema:"More lines were available than returned, or a line was cut at 4 KiB"`
	Warnings  []string `json:"warnings,omitempty" jsonschema:"Warnings"`
}

// NodeRebootResult is the structured output of the talos_node_reboot tool.
type NodeRebootResult struct {
	Cluster  string `json:"cluster" jsonschema:"Cluster name"`
	Node     string `json:"node" jsonschema:"Node address the reboot was sent to"`
	Hostname string `json:"hostname,omitempty" jsonschema:"Node hostname, when known"`
	Mode     string `json:"mode" jsonschema:"Reboot mode: default or powercycle"`
	Accepted bool   `json:"accepted" jsonschema:"Talos accepted the reboot request"`
	ActorID  string `json:"actor_id,omitempty" jsonschema:"Talos actor id to correlate events"`
	Etcd     string `json:"etcd,omitempty" jsonschema:"Result of the etcd quorum check, for control plane nodes"`
	Hint     string `json:"hint" jsonschema:"Next step"`
}

// ClustersEventResult is the structured output of the talos_clusters_event tool.
type ClustersEventResult struct {
	Cluster   string         `json:"cluster" jsonschema:"Cluster name"`
	Since     string         `json:"since" jsonschema:"Time window of the events, e.g. 1h0m0s"`
	Nodes     int            `json:"nodes" jsonschema:"Number of nodes queried"`
	Count     int            `json:"count" jsonschema:"Number of events returned"`
	Truncated bool           `json:"truncated" jsonschema:"More events were found than returned"`
	Events    []EventSummary `json:"events" jsonschema:"Events, newest first"`
	Warnings  []string       `json:"warnings,omitempty" jsonschema:"Warnings, such as nodes that could not be read"`
}

// EventSummary is one machined runtime event.
type EventSummary struct {
	Time     string `json:"time" jsonschema:"RFC3339, UTC, second precision"`
	Node     string `json:"node" jsonschema:"Node address"`
	NodeName string `json:"node_name,omitempty" jsonschema:"Node hostname, when known"`
	Type     string `json:"type" jsonschema:"sequence, phase, task, service, machine_status, config_load_error, config_validation_error, address, restart"`
	Summary  string `json:"summary" jsonschema:"One-line description, sanitized"`
	ActorID  string `json:"actor_id,omitempty" jsonschema:"Talos actor id, shared by the events of one API request"`
	ID       string `json:"id" jsonschema:"Event id"`
}

// ClustersDescribeResult is the structured output of the
// talos_clusters_describe tool. It has no discovery service data.
type ClustersDescribeResult struct {
	Cluster           string              `json:"cluster" jsonschema:"Cluster"`
	ClusterName       string              `json:"cluster_name,omitempty" jsonschema:"Cluster name from Talos"`
	KubernetesVersion string              `json:"kubernetes_version,omitempty" jsonschema:"Kubernetes version (control plane kubelets)"`
	Endpoints         []string            `json:"endpoints" jsonschema:"Talos API endpoints"`
	NodeSource        string              `json:"node_source" jsonschema:"Node list source (members or talosconfig)"`
	Count             int                 `json:"count" jsonschema:"Nodes"`
	Reachable         int                 `json:"reachable" jsonschema:"Reachable nodes"`
	Nodes             []NodeSummary       `json:"nodes" jsonschema:"Nodes"`
	Etcd              []EtcdMemberSummary `json:"etcd,omitempty" jsonschema:"etcd members"`
	Warnings          []string            `json:"warnings,omitempty" jsonschema:"Warnings"`
}

// NodeSummary is one node of talos_clusters_describe. The jsonschema text
// is also the text column header, so it stays short.
type NodeSummary struct {
	Hostname          string   `json:"hostname" jsonschema:"Hostname"`
	Address           string   `json:"address" jsonschema:"Address"`
	Role              string   `json:"role" jsonschema:"Role"`
	TalosVersion      string   `json:"talos_version,omitempty" jsonschema:"Talos"`
	KubernetesVersion string   `json:"kubernetes_version,omitempty" jsonschema:"Kubelet"`
	Reachable         bool     `json:"reachable" jsonschema:"Reachable"`
	Stage             string   `json:"stage,omitempty" jsonschema:"Stage"`
	Ready             bool     `json:"ready" jsonschema:"Ready"`
	Uptime            string   `json:"uptime,omitempty" jsonschema:"Uptime"`
	Resources         string   `json:"resources,omitempty" jsonschema:"Resources"`
	UnhealthyServices []string `json:"unhealthy_services,omitempty" jsonschema:"Unhealthy services"`
	UnmetConditions   []string `json:"unmet_conditions,omitempty" jsonschema:"Not ready because"`
}

// EtcdMemberSummary is one etcd member.
type EtcdMemberSummary struct {
	Hostname string `json:"hostname" jsonschema:"Hostname"`
	ID       string `json:"id" jsonschema:"Member ID"`
	Learner  bool   `json:"learner" jsonschema:"Learner"`
}

// NodeDescribeResult is the structured output of the talos_node_describe
// tool.
type NodeDescribeResult struct {
	Cluster           string          `json:"cluster" jsonschema:"Cluster"`
	Node              string          `json:"node" jsonschema:"Node address"`
	Hostname          string          `json:"hostname,omitempty" jsonschema:"Hostname"`
	Role              string          `json:"role,omitempty" jsonschema:"Role"`
	TalosVersion      string          `json:"talos_version,omitempty" jsonschema:"Talos version"`
	KubernetesVersion string          `json:"kubernetes_version,omitempty" jsonschema:"Kubelet version"`
	Arch              string          `json:"arch,omitempty" jsonschema:"Architecture"`
	Platform          string          `json:"platform,omitempty" jsonschema:"Platform"`
	Stage             string          `json:"stage,omitempty" jsonschema:"Machine stage"`
	Ready             bool            `json:"ready" jsonschema:"Ready"`
	UnmetConditions   []string        `json:"unmet_conditions,omitempty" jsonschema:"Not ready because"`
	BootTime          string          `json:"boot_time,omitempty" jsonschema:"Boot time (RFC3339, UTC)"`
	Uptime            string          `json:"uptime,omitempty" jsonschema:"Uptime"`
	Warnings          []string        `json:"warnings,omitempty" jsonschema:"Warnings, such as data that could not be read"`
	Resources         NodeResources   `json:"resources" jsonschema:"Resource usage"`
	Services          []ServiceStatus `json:"services" jsonschema:"Services"`
	EventsSince       string          `json:"events_since,omitempty" jsonschema:"Time window of the events, e.g. 1h0m0s"`
	EventsTruncated   bool            `json:"events_truncated,omitempty" jsonschema:"More events were found than returned"`
	Events            []EventSummary  `json:"events" jsonschema:"Recent events of the node, newest first"`
	Logs              []ServiceLog    `json:"logs" jsonschema:"Last log lines of services"`
}

// NodeResources is the resource usage of a node.
type NodeResources struct {
	CPUs         int            `json:"cpus,omitempty" jsonschema:"CPU cores"`
	LoadAverage  string         `json:"load_average,omitempty" jsonschema:"Load average (1m, 5m, 15m)"`
	Memory       string         `json:"memory,omitempty" jsonschema:"Memory used"`
	Swap         string         `json:"swap,omitempty" jsonschema:"Swap used"`
	Processes    string         `json:"processes,omitempty" jsonschema:"Processes"`
	Disks        []DiskUsage    `json:"disks,omitempty" jsonschema:"Disks"`
	TopProcesses []ProcessUsage `json:"top_processes,omitempty" jsonschema:"Top processes by memory"`
}

// DiskUsage is one block device filesystem. The jsonschema text is also the
// text column header, so it stays short.
type DiskUsage struct {
	MountedOn   string `json:"mounted_on" jsonschema:"Mounted on"`
	Device      string `json:"device" jsonschema:"Device"`
	Size        string `json:"size" jsonschema:"Size"`
	Used        string `json:"used" jsonschema:"Used"`
	UsedPercent string `json:"used_percent" jsonschema:"Use%"`
}

// ProcessUsage is one process of a node.
type ProcessUsage struct {
	PID     int32  `json:"pid" jsonschema:"PID"`
	Command string `json:"command" jsonschema:"Command"`
	Memory  string `json:"memory" jsonschema:"Memory (RSS)"`
	CPUTime string `json:"cpu_time" jsonschema:"CPU time"`
}

// ServiceStatus is one Talos service of a node.
type ServiceStatus struct {
	ID        string `json:"id" jsonschema:"Service"`
	State     string `json:"state" jsonschema:"State"`
	Health    string `json:"health,omitempty" jsonschema:"Health"`
	LastEvent string `json:"last_event,omitempty" jsonschema:"Last event"`
	Since     string `json:"since,omitempty" jsonschema:"Since"`
}

// ServiceLog is the tail of one service log.
type ServiceLog struct {
	Service   string   `json:"service" jsonschema:"Service id"`
	Lines     []string `json:"lines" jsonschema:"Log lines, oldest first, sanitized"`
	Count     int      `json:"count" jsonschema:"Number of lines returned"`
	Truncated bool     `json:"truncated" jsonschema:"More lines were available than returned, or a line was cut at 4 KiB"`
	Error     string   `json:"error,omitempty" jsonschema:"Why the log could not be read"`
}
