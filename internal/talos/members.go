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

package talos

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/siderolabs/talos/pkg/machinery/resources/cluster"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
)

// MemberSource tells where a member list came from.
type MemberSource string

// Member sources, in the order they are tried.
const (
	// MemberSourceMembers is the COSI Members resource, read through apid.
	MemberSourceMembers MemberSource = "members"
	// MemberSourceTalosconfig is the context's nodes, or else its endpoints.
	MemberSourceTalosconfig MemberSource = "talosconfig"
)

// Machine types reported for members.
const (
	MachineTypeControlPlane = "controlplane"
	MachineTypeWorker       = "worker"
)

// Errors for node resolution.
var (
	ErrUnknownNode   = errors.New("unknown node")
	ErrAmbiguousNode = errors.New("ambiguous node")
	ErrNoDefaultNode = errors.New("no default node")
)

// Member is one cluster node as seen through apid, or a bare talosconfig
// address when apid could not be reached.
type Member struct {
	NodeID   string
	Hostname string
	// NodeName is the Members resource ID, the Kubernetes node name.
	NodeName string
	// MachineType is controlplane, worker, or empty when unknown.
	MachineType     string
	OperatingSystem string
	// Addresses are IPs for the members source, and the talosconfig entries
	// (IPs or DNS names) for the talosconfig source.
	Addresses []string
}

// Name returns the hostname, or the first address when it has none.
func (m Member) Name() string {
	if m.Hostname != "" {
		return m.Hostname
	}

	if len(m.Addresses) > 0 {
		return m.Addresses[0]
	}

	return m.NodeID
}

// MemberList is the result of Pool.Members.
type MemberList struct {
	Members  []Member
	Source   MemberSource
	Warnings []string
	// ReadErr is the apid error that made Members fall back to the
	// talosconfig; it hints that the endpoints are down.
	ReadErr error
}

// Members returns the node list of a cluster. It reads the COSI Members
// resource through the endpoints. When the read fails or returns no members,
// it falls back to the talosconfig nodes, then endpoints, with a warning. It
// never uses the discovery service.
func (p *Pool) Members(ctx context.Context, cluster string) (*MemberList, error) {
	name, err := p.Resolve(cluster)
	if err != nil {
		return nil, err
	}

	members, err := p.cosiMembers(ctx, name)
	if err == nil && len(members) > 0 {
		return &MemberList{Members: members, Source: MemberSourceMembers}, nil
	}

	list := &MemberList{Members: p.talosconfigMembers(name), Source: MemberSourceTalosconfig, ReadErr: err}

	if err != nil {
		list.Warnings = append(list.Warnings, fmt.Sprintf("cluster %s: reading Members through apid failed, using talosconfig addresses: %v", name, err))
	} else {
		list.Warnings = append(list.Warnings, fmt.Sprintf("cluster %s: apid returned no Members, using talosconfig addresses", name))
	}

	return list, nil
}

func (p *Pool) cosiMembers(ctx context.Context, name string) ([]Member, error) {
	c, err := p.Client(ctx, name)
	if err != nil {
		return nil, err
	}

	items, err := safe.StateListAll[*cluster.Member](ctx, c.State())
	if err != nil {
		return nil, err
	}

	members := make([]Member, 0, items.Len())

	for res := range items.All() {
		spec := res.TypedSpec()

		m := Member{
			NodeID:          spec.NodeID,
			Hostname:        spec.Hostname,
			NodeName:        res.Metadata().ID(),
			MachineType:     normalizeMachineType(spec.MachineType.String()),
			OperatingSystem: spec.OperatingSystem,
			Addresses:       make([]string, 0, len(spec.Addresses)),
		}

		for _, addr := range spec.Addresses {
			m.Addresses = append(m.Addresses, addr.String())
		}

		members = append(members, m)
	}

	SortMembers(members)

	return members, nil
}

// talosconfigMembers returns the context nodes, or its endpoints when it has
// none, as members with unknown machine type.
func (p *Pool) talosconfigMembers(name string) []Member {
	c := p.cfg.Contexts[name]

	addrs := c.Nodes
	if len(addrs) == 0 {
		addrs = c.Endpoints
	}

	members := make([]Member, 0, len(addrs))

	for _, a := range addrs {
		members = append(members, Member{Addresses: []string{stripPort(a)}})
	}

	return members
}

// SortMembers orders members by machine type (control plane first, then
// workers, then unknown), then name, then node ID.
func SortMembers(members []Member) {
	slices.SortStableFunc(members, func(a, b Member) int {
		return compareMembers(a.MachineType, a.Name(), a.NodeID, b.MachineType, b.Name(), b.NodeID)
	})
}

// compareMembers is the member order shared by Members and Affiliates.
func compareMembers(typeA, nameA, idA, typeB, nameB, idB string) int {
	return cmp.Or(
		cmp.Compare(machineTypeRank(typeA), machineTypeRank(typeB)),
		strings.Compare(strings.ToLower(nameA), strings.ToLower(nameB)),
		strings.Compare(idA, idB),
	)
}

func machineTypeRank(t string) int {
	switch t {
	case MachineTypeControlPlane:
		return 0
	case MachineTypeWorker:
		return 1
	default:
		return 2
	}
}

// normalizeMachineType maps a Talos or affiliate machine type to
// controlplane, worker, or empty when unknown.
func normalizeMachineType(t string) string {
	switch t {
	case "controlplane", "init":
		return MachineTypeControlPlane
	case "worker", "join":
		return MachineTypeWorker
	default:
		return ""
	}
}

// ResolvedNode is the target of a node tool.
type ResolvedNode struct {
	// Address is passed to apid as the target node.
	Address string
	// Name is the hostname when known, else the address.
	Name string
	// MachineType is controlplane or worker when the node was found in the
	// Members resource, empty when unknown (an IP, or the talosconfig source).
	MachineType string
	Warnings    []string
}

// ResolveNode maps a node tool's `node` argument to an address:
// an IP is used as is, a hostname, node name or node ID is looked up in
// Pool.Members, and an empty node falls back to the context's single default
// node. A :port suffix and IPv6 brackets are ignored.
func (p *Pool) ResolveNode(ctx context.Context, cluster, node string) (*ResolvedNode, error) {
	name, err := p.Resolve(cluster)
	if err != nil {
		return nil, err
	}

	node = stripPort(strings.TrimSpace(node))

	if addr, err := netip.ParseAddr(node); err == nil {
		return &ResolvedNode{Address: addr.String(), Name: addr.String()}, nil
	}

	defaults := p.cfg.Contexts[name].Nodes

	if node == "" && len(defaults) == 1 {
		addr := stripPort(defaults[0])

		return &ResolvedNode{Address: addr, Name: addr}, nil
	}

	list, err := p.Members(ctx, name)
	if err != nil {
		return nil, err
	}

	if node == "" {
		return nil, fmt.Errorf("%w: cluster %s has %d default nodes in talosconfig, set node to one of %s%s",
			ErrNoDefaultNode, name, len(defaults), memberNames(list.Members), list.note())
	}

	var matches []Member

	for _, m := range list.Members {
		if m.matches(node) {
			matches = append(matches, m)
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w %q in cluster %s: known nodes are %s%s", ErrUnknownNode, node, name, memberNames(list.Members), list.note())
	case 1:
	default:
		return nil, fmt.Errorf("%w %q in cluster %s: matches %s; use an address instead", ErrAmbiguousNode, node, name, memberDetails(matches))
	}

	addr := p.selectAddress(name, matches[0].Addresses)
	if addr == "" {
		return nil, fmt.Errorf("node %q in cluster %s has no usable address", node, name)
	}

	return &ResolvedNode{Address: addr, Name: matches[0].Name(), MachineType: matches[0].MachineType, Warnings: list.Warnings}, nil
}

// NodeTargets is the result of Pool.ResolveAllNodes.
type NodeTargets struct {
	// Nodes are the targets in member order, one per address.
	Nodes []ResolvedNode
	// Unaddressed are the members without a usable address.
	Unaddressed []Member
	Source      MemberSource
	Warnings    []string
	// ReadErr is MemberList.ReadErr.
	ReadErr error
}

// ResolveAllNodes returns every member of a cluster as a node tool target,
// with the address picked by SelectAddress, or by SelectDiscoveryAddress when
// the cluster has a discovery block. Members without a usable address are
// left out with a warning. Members that resolve to the same address (such as
// "10.0.0.1" and "10.0.0.1:50000" in the talosconfig fallback) become one
// target, so each node is queried once.
func (p *Pool) ResolveAllNodes(ctx context.Context, cluster string) (*NodeTargets, error) {
	list, err := p.Members(ctx, cluster)
	if err != nil {
		return nil, err
	}

	name, err := p.Resolve(cluster)
	if err != nil {
		return nil, err
	}

	targets := &NodeTargets{
		Nodes:    make([]ResolvedNode, 0, len(list.Members)),
		Source:   list.Source,
		Warnings: slices.Clone(list.Warnings),
		ReadErr:  list.ReadErr,
	}
	seen := map[string]string{}

	for _, m := range list.Members {
		addr := p.selectAddress(name, m.Addresses)

		switch first, dup := seen[addr]; {
		case addr == "":
			targets.Unaddressed = append(targets.Unaddressed, m)
			targets.Warnings = append(targets.Warnings, fmt.Sprintf("node %s in cluster %s has no usable address, skipped", m.Name(), name))
		case dup:
			if first != m.Name() {
				targets.Warnings = append(targets.Warnings, fmt.Sprintf("nodes %s and %s in cluster %s share address %s, queried once", first, m.Name(), name, addr))
			}
		default:
			seen[addr] = m.Name()
			targets.Nodes = append(targets.Nodes, ResolvedNode{Address: addr, Name: m.Name(), MachineType: m.MachineType})
		}
	}

	return targets, nil
}

// matches reports whether node names this member: its hostname, node name,
// node ID or one of its addresses, ignoring case.
func (m Member) matches(node string) bool {
	if strings.EqualFold(m.Hostname, node) || strings.EqualFold(m.NodeName, node) || strings.EqualFold(m.NodeID, node) {
		return true
	}

	return slices.ContainsFunc(m.Addresses, func(a string) bool { return strings.EqualFold(a, node) })
}

// note explains, for an error message, that the member list came from the
// talosconfig fallback and why.
func (l *MemberList) note() string {
	if l.Source != MemberSourceTalosconfig || len(l.Warnings) == 0 {
		return ""
	}

	return " (" + strings.Join(l.Warnings, "; ") + ")"
}

// SelectAddress picks the target address from a member's addresses.
// Link-local addresses are never used. It prefers, in order: an
// address of the preferred IP family, any other IP or DNS name, and last an
// address that looks like a KubeSpan or SideroLink ULA. The ULA check only
// looks at two bytes, so a site ULA subnet can match it; those addresses are
// still used when nothing else is left.
func SelectAddress(addrs []string, preferV6 bool) string {
	var fallback, ula string

	for _, a := range addrs {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			if fallback == "" {
				fallback = a
			}

			continue
		}

		switch {
		case ip.IsLinkLocalUnicast():
			continue
		case network.IsULA(ip, network.ULAKubeSpan) || network.IsULA(ip, network.ULASideroLink):
			if ula == "" {
				ula = a
			}

			continue
		case ip.Is6() == preferV6:
			return a
		}

		if fallback == "" {
			fallback = a
		}
	}

	if fallback != "" {
		return fallback
	}

	return ula
}

// Address ranks of SelectDiscoveryAddress; lower is better.
const (
	rankPrivateIPv4 = iota
	rankPrivateIPv6
	rankKubeSpan
	rankPublicIPv6
	rankPublicIPv4
	rankOther
	rankUnusable
)

// SelectDiscoveryAddress picks the target address of a node in a cluster
// with a discovery service, where nodes usually share a local network or a
// KubeSpan mesh. It prefers, in order: a private IPv4, a private IPv6 (ULA),
// a KubeSpan address, a public IPv6, a public IPv4, then anything else such
// as a DNS name. Link-local and SideroLink addresses are never used: the
// SideroLink tunnel is only reachable through Omni.
// On a tie the first address wins.
func SelectDiscoveryAddress(addrs []string) string {
	best, bestRank := "", rankUnusable

	for _, a := range addrs {
		if r := discoveryAddressRank(a); r < bestRank {
			best, bestRank = a, r
		}
	}

	return best
}

func discoveryAddressRank(a string) int {
	ip, err := netip.ParseAddr(a)
	if err != nil {
		return rankOther
	}

	switch {
	case ip.IsLinkLocalUnicast():
		return rankUnusable
	case network.IsULA(ip, network.ULAKubeSpan):
		return rankKubeSpan
	case network.IsULA(ip, network.ULASideroLink):
		return rankUnusable
	case ip.IsPrivate() && ip.Is4():
		return rankPrivateIPv4
	case ip.IsPrivate():
		return rankPrivateIPv6
	case ip.IsGlobalUnicast() && ip.Is6():
		return rankPublicIPv6
	case ip.IsGlobalUnicast():
		return rankPublicIPv4
	default:
		return rankOther
	}
}

// selectAddress picks a member's target address for a cluster: the
// SelectDiscoveryAddress order when the cluster has a discovery block, else
// SelectAddress with the endpoints' IP family.
func (p *Pool) selectAddress(cluster string, addrs []string) string {
	if _, ok := p.discovery[cluster]; ok {
		return SelectDiscoveryAddress(addrs)
	}

	return SelectAddress(addrs, p.PreferIPv6(cluster))
}

// PreferIPv6 reports whether node addresses of a cluster should be IPv6,
// because its first IP endpoint is (see SelectAddress).
func (p *Pool) PreferIPv6(cluster string) bool {
	endpoints, _, err := p.ContextInfo(cluster)
	if err != nil {
		return false
	}

	return preferIPv6(endpoints)
}

// preferIPv6 reports whether the first IP endpoint is IPv6. DNS endpoints
// count as IPv4.
func preferIPv6(endpoints []string) bool {
	for _, e := range endpoints {
		if ip, err := netip.ParseAddr(stripPort(e)); err == nil {
			return ip.Is6()
		}
	}

	return false
}

// stripPort removes a :port suffix from a talosconfig endpoint or node.
func stripPort(s string) string {
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}

	return strings.Trim(s, "[]")
}

// memberDetails lists members with their node ID and addresses, so an
// ambiguous match can be told apart.
func memberDetails(members []Member) string {
	parts := make([]string, 0, len(members))
	for _, m := range members {
		parts = append(parts, fmt.Sprintf("%s (node ID %s, addresses %s)", m.Name(), m.NodeID, strings.Join(m.Addresses, ", ")))
	}

	return strings.Join(parts, "; ")
}

func memberNames(members []Member) string {
	if len(members) == 0 {
		return "none"
	}

	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.Name())
	}

	return strings.Join(names, ", ")
}
