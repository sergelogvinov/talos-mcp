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

package talos_test

import (
	"context"
	"testing"
	"time"

	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/sergelogvinov/talos-mcp/internal/talos/talostest"
	clientpb "github.com/siderolabs/discovery-api/api/v1alpha1/client/pb"
	serverpb "github.com/siderolabs/discovery-api/api/v1alpha1/server/pb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newDiscoveryPool(t *testing.T, dialer talos.DiscoveryDialer, clock func() time.Time, contexts ...talostest.Context) *talos.Pool {
	t.Helper()

	tc, err := config.ParseTalosConfig([]byte(talostest.TalosConfig(t, now, contexts...)), "")
	require.NoError(t, err)

	pool, err := talos.NewPool(tc, talos.WithClock(clock), talos.WithDiscoveryDialer(dialer))
	require.NoError(t, err)

	return pool
}

var discoveryContexts = []talostest.Context{
	{Name: "prod", Roles: []string{"os:reader"}, Discovery: "prod-id"},
	{Name: "staging", Roles: []string{"os:operator"}},
}

func TestPoolAffiliates(t *testing.T) {
	block := talostest.Cipher(t, talostest.Secret)
	wrong := talostest.Cipher(t, "ZmVkY2JhOTg3NjU0MzIxMGZlZGNiYTk4NzY1NDMyMTA=")

	fake := &talostest.FakeDiscovery{Affiliates: []*serverpb.Affiliate{
		{
			Id: "cp-affiliate",
			Data: talostest.EncryptAffiliate(t, block, &clientpb.Affiliate{
				NodeId:          "cp-1",
				Hostname:        "cp-1",
				Nodename:        "cp-1",
				MachineType:     "controlplane",
				OperatingSystem: "Talos (v1.14.2)",
				Addresses:       [][]byte{talostest.IP("10.0.0.11"), talostest.IP("2001:db8::11")},
				ControlPlane:    &clientpb.ControlPlane{ApiServerPort: 6443},
				Kubespan: &clientpb.KubeSpan{
					PublicKey:           "pubkey=",
					Address:             talostest.IP("fd71:4a6b:e5a3:9c02::11"),
					AdditionalAddresses: []*clientpb.IPPrefix{{Ip: talostest.IP("10.244.0.0"), Bits: 24}},
				},
			}),
			Endpoints: [][]byte{talostest.EncryptEndpoint(t, block, "203.0.113.11:51820")},
		},
		{
			// KubeSpan-only affiliate: no machine type, no addresses. Kept.
			Id:   "kubespan-only",
			Data: talostest.EncryptAffiliate(t, block, &clientpb.Affiliate{NodeId: "ext-1", Kubespan: &clientpb.KubeSpan{PublicKey: "extkey="}}),
		},
		{Id: "foreign", Data: talostest.EncryptAffiliate(t, wrong, &clientpb.Affiliate{NodeId: "other"})},
		{Id: "endpoints-only", Endpoints: [][]byte{talostest.EncryptEndpoint(t, block, "203.0.113.99:51820")}},
	}}

	dialer, endpoints := talostest.StartFakeDiscovery(t, fake)
	pool := newDiscoveryPool(t, dialer, func() time.Time { return now }, discoveryContexts...)

	list, err := pool.Affiliates(t.Context(), "")
	require.NoError(t, err)

	assert.Equal(t, "prod-id", list.ClusterID)
	assert.Equal(t, config.DefaultDiscoveryEndpoint, list.Endpoint)
	assert.Equal(t, []string{config.DefaultDiscoveryEndpoint}, *endpoints)
	assert.Equal(t, 1, list.Undecryptable)
	assert.Equal(t, []talos.Affiliate{
		{
			ID:              "cp-affiliate",
			NodeID:          "cp-1",
			Hostname:        "cp-1",
			NodeName:        "cp-1",
			MachineType:     "controlplane",
			OperatingSystem: "Talos (v1.14.2)",
			Addresses:       []string{"10.0.0.11", "2001:db8::11"},
			Endpoints:       []string{"203.0.113.11:51820"},
			APIServerPort:   6443,
			KubeSpan: &talos.AffiliateKubeSpan{
				Address:             "fd71:4a6b:e5a3:9c02::11",
				PublicKey:           "pubkey=",
				AdditionalAddresses: []string{"10.244.0.0/24"},
			},
		},
		{
			ID:       "kubespan-only",
			NodeID:   "ext-1",
			KubeSpan: &talos.AffiliateKubeSpan{PublicKey: "extkey="},
		},
	}, list.Affiliates)

	assert.Equal(t, 1, fake.Calls("List"))
	assert.Equal(t, []string{"prod-id"}, fake.ClusterIDs)

	for _, rpc := range []string{"Hello", "AffiliateUpdate", "AffiliateDelete", "Watch"} {
		assert.Zero(t, fake.Calls(rpc), "%s must never be called", rpc)
	}
}

func TestPoolAffiliatesCache(t *testing.T) {
	block := talostest.Cipher(t, talostest.Secret)
	fake := &talostest.FakeDiscovery{Affiliates: []*serverpb.Affiliate{
		{Id: "a", Data: talostest.EncryptAffiliate(t, block, &clientpb.Affiliate{NodeId: "a", MachineType: "worker"})},
	}}

	dialer, _ := talostest.StartFakeDiscovery(t, fake)

	clock := now
	pool := newDiscoveryPool(t, dialer, func() time.Time { return clock }, discoveryContexts...)

	for range 3 {
		_, err := pool.Affiliates(t.Context(), "prod")
		require.NoError(t, err)
	}

	assert.Equal(t, 1, fake.Calls("List"), "calls within 30s are served from the cache")

	clock = now.Add(31 * time.Second)

	_, err := pool.Affiliates(t.Context(), "prod")
	require.NoError(t, err)
	assert.Equal(t, 2, fake.Calls("List"), "the cache expires after 30s")
}

func TestPoolAffiliatesErrors(t *testing.T) {
	block := talostest.Cipher(t, talostest.Secret)
	wrong := talostest.Cipher(t, "ZmVkY2JhOTg3NjU0MzIxMGZlZGNiYTk4NzY1NDMyMTA=")

	for _, tt := range []struct {
		name          string
		cluster       string
		fake          *talostest.FakeDiscovery
		expectedError error
		expectedMsg   string
		expectedCalls int
	}{
		{
			name:          "cluster without discovery keys is rejected before any call",
			cluster:       "staging",
			fake:          &talostest.FakeDiscovery{},
			expectedError: talos.ErrNoDiscovery,
			expectedMsg:   "cluster staging: no discovery keys; clusters with discovery: prod",
		},
		{
			name:          "service unreachable",
			cluster:       "prod",
			fake:          &talostest.FakeDiscovery{Err: status.Error(codes.Unavailable, "connection refused")},
			expectedError: talos.ErrDiscoveryUnreachable,
			expectedMsg:   "cluster prod: discovery service unreachable: discovery.talos.dev:443: connection refused",
			expectedCalls: 1,
		},
		{
			name:          "wrong secret",
			cluster:       "prod",
			fake:          &talostest.FakeDiscovery{Affiliates: []*serverpb.Affiliate{{Id: "a", Data: talostest.EncryptAffiliate(t, wrong, &clientpb.Affiliate{NodeId: "a"})}}},
			expectedError: talos.ErrDiscoveryDecrypt,
			expectedMsg:   "cluster prod: cluster_secret does not decrypt affiliate data (wrong secret?): 1 records",
			expectedCalls: 1,
		},
		{
			name:          "no affiliates",
			cluster:       "prod",
			fake:          &talostest.FakeDiscovery{Affiliates: []*serverpb.Affiliate{{Id: "a", Endpoints: [][]byte{talostest.EncryptEndpoint(t, block, "203.0.113.1:51820")}}}},
			expectedError: talos.ErrNoAffiliates,
			expectedMsg:   "cluster prod: no affiliates registered for cluster_id",
			expectedCalls: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dialer, endpoints := talostest.StartFakeDiscovery(t, tt.fake)
			pool := newDiscoveryPool(t, dialer, func() time.Time { return now }, discoveryContexts...)

			_, err := pool.Affiliates(t.Context(), tt.cluster)
			require.ErrorIs(t, err, tt.expectedError)
			assert.EqualError(t, err, tt.expectedMsg)
			assert.NotContains(t, err.Error(), talostest.Secret)
			assert.Equal(t, tt.expectedCalls, tt.fake.Calls("List"))

			if tt.expectedCalls == 0 {
				assert.Empty(t, *endpoints, "no connection is made")
			}
		})
	}
}

func TestPoolAffiliatesErrorNotCached(t *testing.T) {
	block := talostest.Cipher(t, talostest.Secret)
	fake := &talostest.FakeDiscovery{Err: status.Error(codes.Unavailable, "down")}

	dialer, _ := talostest.StartFakeDiscovery(t, fake)
	pool := newDiscoveryPool(t, dialer, func() time.Time { return now }, discoveryContexts...)

	_, err := pool.Affiliates(t.Context(), "prod")
	require.Error(t, err)

	fake.Mu.Lock()
	fake.Err = nil
	fake.Affiliates = []*serverpb.Affiliate{{Id: "a", Data: talostest.EncryptAffiliate(t, block, &clientpb.Affiliate{NodeId: "a"})}}
	fake.Mu.Unlock()

	list, err := pool.Affiliates(t.Context(), "prod")
	require.NoError(t, err)
	assert.Len(t, list.Affiliates, 1)
}

func TestPoolAffiliatesReturnsCopy(t *testing.T) {
	block := talostest.Cipher(t, talostest.Secret)
	fake := &talostest.FakeDiscovery{Affiliates: []*serverpb.Affiliate{
		{Id: "a", Data: talostest.EncryptAffiliate(t, block, &clientpb.Affiliate{NodeId: "a", Addresses: [][]byte{talostest.IP("10.0.0.1")}})},
	}}

	dialer, _ := talostest.StartFakeDiscovery(t, fake)
	pool := newDiscoveryPool(t, dialer, func() time.Time { return now }, discoveryContexts...)

	first, err := pool.Affiliates(t.Context(), "prod")
	require.NoError(t, err)

	first.Affiliates[0].Addresses[0] = "changed"
	first.Affiliates = nil

	second, err := pool.Affiliates(t.Context(), "prod")
	require.NoError(t, err)
	require.Len(t, second.Affiliates, 1)
	assert.Equal(t, []string{"10.0.0.1"}, second.Affiliates[0].Addresses, "the cached list is not changed by callers")
	assert.Equal(t, 1, fake.Calls("List"))
}

func TestPoolAffiliatesPerClusterLock(t *testing.T) {
	block := talostest.Cipher(t, talostest.Secret)
	release := make(chan struct{})

	fake := &talostest.FakeDiscovery{
		Affiliates:     []*serverpb.Affiliate{{Id: "a", Data: talostest.EncryptAffiliate(t, block, &clientpb.Affiliate{NodeId: "a"})}},
		BlockClusterID: "slow-id",
		Block:          release,
	}

	dialer, _ := talostest.StartFakeDiscovery(t, fake)
	pool := newDiscoveryPool(t, dialer, func() time.Time { return now },
		talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Discovery: "prod-id"},
		talostest.Context{Name: "slow", Roles: []string{"os:reader"}, Discovery: "slow-id"},
	)

	slowDone := make(chan error, 1)

	go func() {
		_, err := pool.Affiliates(context.Background(), "slow")
		slowDone <- err
	}()

	require.Eventually(t, func() bool { return fake.Calls("List") == 1 }, 5*time.Second, 10*time.Millisecond)

	// Another cluster is not blocked by the slow one.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := pool.Affiliates(ctx, "prod")
	require.NoError(t, err)

	// A second caller for the slow cluster waits, and gives up with its context.
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer waitCancel()

	_, err = pool.Affiliates(waitCtx, "slow")
	require.ErrorIs(t, err, context.DeadlineExceeded)

	close(release)
	require.NoError(t, <-slowDone)
}
