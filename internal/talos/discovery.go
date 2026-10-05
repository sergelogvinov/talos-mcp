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
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/sergelogvinov/talos-mcp/internal/config"
	clientpb "github.com/siderolabs/discovery-api/api/v1alpha1/client/pb"
	serverpb "github.com/siderolabs/discovery-api/api/v1alpha1/server/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// affiliatesTTL is how long discovery results are cached per cluster.
const affiliatesTTL = 30 * time.Second

// Errors returned for the discovery service.
var (
	ErrNoDiscovery          = errors.New("no discovery keys")
	ErrDiscoveryUnreachable = errors.New("discovery service unreachable")
	ErrDiscoveryDecrypt     = errors.New("cluster_secret does not decrypt affiliate data (wrong secret?)")
	ErrNoAffiliates         = errors.New("no affiliates registered for cluster_id")
)

// DiscoveryDialer connects to a discovery service endpoint (host:port).
// insecure means plain gRPC without TLS (an http:// endpoint).
type DiscoveryDialer func(ctx context.Context, endpoint string, insecure bool) (*grpc.ClientConn, error)

// Affiliate is one node as registered with the discovery service.
type Affiliate struct {
	ID              string
	NodeID          string
	Hostname        string
	NodeName        string
	MachineType     string
	OperatingSystem string
	Addresses       []string
	// Endpoints are the ip:port records stored next to the affiliate
	// (KubeSpan WireGuard endpoints reported by the node and its peers).
	Endpoints     []string
	APIServerPort int
	KubeSpan      *AffiliateKubeSpan
}

// AffiliateKubeSpan is the KubeSpan peer data of an affiliate.
type AffiliateKubeSpan struct {
	Address             string
	PublicKey           string
	AdditionalAddresses []string
}

// AffiliateList is the result of Pool.Affiliates.
type AffiliateList struct {
	ClusterID  string
	Endpoint   string
	Affiliates []Affiliate // sorted with the same order as SortMembers
	// Undecryptable counts records that failed to decrypt or unmarshal.
	Undecryptable int
}

// clone returns a deep copy, so callers can't change the cached list.
func (l *AffiliateList) clone() *AffiliateList {
	out := *l
	out.Affiliates = make([]Affiliate, len(l.Affiliates))

	for i, a := range l.Affiliates {
		a.Addresses = slices.Clone(a.Addresses)
		a.Endpoints = slices.Clone(a.Endpoints)

		if a.KubeSpan != nil {
			ks := *a.KubeSpan
			ks.AdditionalAddresses = slices.Clone(ks.AdditionalAddresses)
			a.KubeSpan = &ks
		}

		out.Affiliates[i] = a
	}

	return &out
}

// affiliateCache is the per-cluster cache entry. sem serializes fetches for
// one cluster only, and a waiting caller gives up when its context ends.
type affiliateCache struct {
	sem  chan struct{}
	at   time.Time
	list *AffiliateList
}

// WithDiscoveryDialer replaces the discovery service connection, for tests.
func WithDiscoveryDialer(d DiscoveryDialer) Option {
	return func(o *options) {
		o.dialDiscovery = d
	}
}

// dialDiscovery connects to a discovery service over TLS with the system
// roots, as Talos nodes do, or without TLS for an http:// endpoint. It does
// not block.
func dialDiscovery(_ context.Context, endpoint string, plaintext bool) (*grpc.ClientConn, error) {
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	if plaintext {
		creds = insecure.NewCredentials()
	}

	return grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
}

// Affiliates returns the affiliates of a cluster from the discovery service.
// It only calls the List RPC: the server never registers itself as an
// affiliate. Results are cached for 30s per cluster, and the returned list is
// a copy.
func (p *Pool) Affiliates(ctx context.Context, cluster string) (*AffiliateList, error) {
	name, err := p.Resolve(cluster)
	if err != nil {
		return nil, err
	}

	d, ok := p.discovery[name]
	if !ok {
		return nil, fmt.Errorf("cluster %s: %w; clusters with discovery: %s",
			name, ErrNoDiscovery, namesOrNone(p.ClustersWithDiscovery()))
	}

	entry := p.affiliateEntry(name)

	select {
	case entry.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("cluster %s: %w", name, ctx.Err())
	}
	defer func() { <-entry.sem }()

	if entry.list != nil && p.now().Sub(entry.at) < affiliatesTTL {
		return entry.list.clone(), nil
	}

	list, err := p.fetchAffiliates(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("cluster %s: %w", name, err)
	}

	entry.at, entry.list = p.now(), list

	return list.clone(), nil
}

// affiliateEntry returns the cache entry of a cluster, creating it.
func (p *Pool) affiliateEntry(name string) *affiliateCache {
	p.affMu.Lock()
	defer p.affMu.Unlock()

	entry, ok := p.affiliates[name]
	if !ok {
		entry = &affiliateCache{sem: make(chan struct{}, 1)}
		p.affiliates[name] = entry
	}

	return entry
}

func (p *Pool) fetchAffiliates(ctx context.Context, d *config.DiscoveryConfig) (*AffiliateList, error) {
	key, err := base64.StdEncoding.DecodeString(d.ClusterSecret)
	if err != nil {
		return nil, fmt.Errorf("decoding cluster_secret: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cluster_secret: %w", err)
	}

	conn, err := p.dialDiscovery(ctx, d.Endpoint, d.Insecure)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrDiscoveryUnreachable, d.Endpoint, err)
	}
	defer conn.Close() //nolint:errcheck

	resp, err := serverpb.NewClusterClient(conn).List(ctx, &serverpb.ListRequest{ClusterId: d.ClusterID})
	if err != nil {
		switch status.Code(err) { //nolint:exhaustive
		case codes.Unavailable, codes.DeadlineExceeded:
			return nil, fmt.Errorf("%w: %s: %s", ErrDiscoveryUnreachable, d.Endpoint, status.Convert(err).Message())
		default:
			return nil, fmt.Errorf("discovery service %s: %w", d.Endpoint, err)
		}
	}

	list, err := decryptAffiliates(block, resp.GetAffiliates())
	if err != nil {
		return nil, err
	}

	list.ClusterID = d.ClusterID
	list.Endpoint = d.Endpoint

	return list, nil
}

// decryptAffiliates decrypts the affiliate records of a List response
// (see discovery_cipher.go). Records without data are skipped; records that
// don't decrypt are counted.
func decryptAffiliates(block cipher.Block, records []*serverpb.Affiliate) (*AffiliateList, error) {
	gcm, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}

	list := &AffiliateList{}
	withData := 0

	for _, rec := range records {
		if len(rec.GetData()) == 0 {
			// No affiliate data (yet?): the node only registered endpoints.
			continue
		}

		withData++

		pb, err := openAffiliate(gcm, rec.GetData())
		if err != nil {
			list.Undecryptable++

			continue
		}

		var endpoints []string

		for _, raw := range rec.GetEndpoints() {
			ep, err := openEndpoint(block, raw)
			if err != nil {
				continue
			}

			if ip, ok := netip.AddrFromSlice(ep.GetIp()); ok {
				endpoints = append(endpoints, netip.AddrPortFrom(ip.Unmap(), uint16(ep.GetPort())).String())
			}
		}

		list.Affiliates = append(list.Affiliates, newAffiliate(rec.GetId(), pb, endpoints))
	}

	switch {
	case withData == 0:
		return nil, ErrNoAffiliates
	case len(list.Affiliates) == 0:
		return nil, fmt.Errorf("%w: %d records", ErrDiscoveryDecrypt, list.Undecryptable)
	}

	slices.SortStableFunc(list.Affiliates, func(a, b Affiliate) int {
		return compareMembers(a.MachineType, a.Hostname, a.NodeID, b.MachineType, b.Hostname, b.NodeID)
	})

	return list, nil
}

func newAffiliate(id string, pb *clientpb.Affiliate, endpoints []string) Affiliate {
	a := Affiliate{
		ID:              id,
		NodeID:          pb.GetNodeId(),
		Hostname:        pb.GetHostname(),
		NodeName:        pb.GetNodename(),
		MachineType:     normalizeMachineType(pb.GetMachineType()),
		OperatingSystem: pb.GetOperatingSystem(),
		Endpoints:       endpoints,
	}

	for _, raw := range pb.GetAddresses() {
		if ip, ok := netip.AddrFromSlice(raw); ok {
			a.Addresses = append(a.Addresses, ip.Unmap().String())
		}
	}

	if cp := pb.GetControlPlane(); cp != nil {
		a.APIServerPort = int(cp.GetApiServerPort())
	}

	if ks := pb.GetKubespan(); ks != nil {
		info := &AffiliateKubeSpan{PublicKey: ks.GetPublicKey()}

		if ip, ok := netip.AddrFromSlice(ks.GetAddress()); ok {
			info.Address = ip.Unmap().String()
		}

		for _, prefix := range ks.GetAdditionalAddresses() {
			if ip, ok := netip.AddrFromSlice(prefix.GetIp()); ok {
				info.AdditionalAddresses = append(info.AdditionalAddresses, netip.PrefixFrom(ip.Unmap(), int(prefix.GetBits())).String())
			}
		}

		a.KubeSpan = info
	}

	return a
}

func namesOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}

	return strings.Join(names, ", ")
}
