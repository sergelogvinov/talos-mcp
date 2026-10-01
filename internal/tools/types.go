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
