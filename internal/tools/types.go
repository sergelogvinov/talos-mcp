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
