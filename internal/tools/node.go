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

	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/siderolabs/talos/pkg/machinery/client"
)

// nodeTarget is a resolved node tool target.
type nodeTarget struct {
	cluster string
	node    *talos.ResolvedNode
	client  talos.Client
}

// nodeContext aims ctx at the target node for apid (client.WithNode).
func (n *nodeTarget) nodeContext(ctx context.Context) context.Context {
	return client.WithNode(ctx, n.node.Address)
}

// label is the node as shown in text output: "name (address)" or "address".
func (n *nodeTarget) label() string {
	if n.node.Name != "" && n.node.Name != n.node.Address {
		return n.node.Name + " (" + n.node.Address + ")"
	}

	return n.node.Address
}

// nodeName is the hostname for results, empty when it is just the address.
func (n *nodeTarget) nodeName() string {
	if n.node.Name == n.node.Address {
		return ""
	}

	return n.node.Name
}

// resolveNodeTarget runs the per-call checks of a node tool (design §9):
// cluster, minimum role, then node resolution (§6.1), and returns the
// cluster's client.
func (t *TalosTools) resolveNodeTarget(ctx context.Context, cluster, node string, required talos.Role) (*nodeTarget, error) {
	name, err := t.pool.Resolve(cluster)
	if err != nil {
		return nil, err
	}

	if err := t.pool.Require(name, required); err != nil {
		return nil, err
	}

	resolved, err := t.pool.ResolveNode(ctx, name, node)
	if err != nil {
		return nil, err
	}

	c, err := t.pool.Client(ctx, name)
	if err != nil {
		return nil, err
	}

	return &nodeTarget{
		cluster: name,
		node:    resolved,
		client:  c,
	}, nil
}
