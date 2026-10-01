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

package talostest

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/netip"
	"sync"
	"testing"

	clientpb "github.com/siderolabs/discovery-api/api/v1alpha1/client/pb"
	serverpb "github.com/siderolabs/discovery-api/api/v1alpha1/server/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// FakeDiscovery is an in-process discovery Cluster service. It records every
// RPC, so tests can check the client only ever calls List.
type FakeDiscovery struct {
	serverpb.UnimplementedClusterServer

	// Mu guards the fields below; hold it to change them while serving.
	Mu         sync.Mutex
	calls      map[string]int
	ClusterIDs []string
	Affiliates []*serverpb.Affiliate
	Err        error

	// Block, when set, makes List for BlockClusterID wait until it is closed
	// or the call is canceled.
	BlockClusterID string
	Block          chan struct{}
}

// Calls returns how often an RPC was called.
func (f *FakeDiscovery) Calls(rpc string) int {
	f.Mu.Lock()
	defer f.Mu.Unlock()

	return f.calls[rpc]
}

// Hello records the call and fails: the server must never call it.
func (f *FakeDiscovery) Hello(context.Context, *serverpb.HelloRequest) (*serverpb.HelloResponse, error) {
	f.record("Hello")

	return nil, status.Error(codes.Unimplemented, "test: Hello must not be called")
}

// AffiliateUpdate records the call and fails: the server must never call it.
func (f *FakeDiscovery) AffiliateUpdate(context.Context, *serverpb.AffiliateUpdateRequest) (*serverpb.AffiliateUpdateResponse, error) {
	f.record("AffiliateUpdate")

	return nil, status.Error(codes.Unimplemented, "test: AffiliateUpdate must not be called")
}

// AffiliateDelete records the call and fails: the server must never call it.
func (f *FakeDiscovery) AffiliateDelete(context.Context, *serverpb.AffiliateDeleteRequest) (*serverpb.AffiliateDeleteResponse, error) {
	f.record("AffiliateDelete")

	return nil, status.Error(codes.Unimplemented, "test: AffiliateDelete must not be called")
}

// Watch records the call and fails: the server must never call it.
func (f *FakeDiscovery) Watch(*serverpb.WatchRequest, grpc.ServerStreamingServer[serverpb.WatchResponse]) error {
	f.record("Watch")

	return status.Error(codes.Unimplemented, "test: Watch must not be called")
}

// List records the call and returns Affiliates, or Err when set.
func (f *FakeDiscovery) List(ctx context.Context, req *serverpb.ListRequest) (*serverpb.ListResponse, error) {
	f.record("List")

	if f.Block != nil && req.GetClusterId() == f.BlockClusterID {
		select {
		case <-f.Block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	f.Mu.Lock()
	defer f.Mu.Unlock()

	f.ClusterIDs = append(f.ClusterIDs, req.GetClusterId())

	if f.Err != nil {
		return nil, f.Err
	}

	return &serverpb.ListResponse{Affiliates: f.Affiliates}, nil
}

func (f *FakeDiscovery) record(rpc string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()

	f.calls[rpc]++
}

// StartFakeDiscovery serves fake over bufconn and returns a dialer for it,
// plus the endpoints the dialer was asked for.
func StartFakeDiscovery(t testing.TB, fake *FakeDiscovery) (func(context.Context, string, bool) (*grpc.ClientConn, error), *[]string) {
	t.Helper()

	fake.calls = map[string]int{}

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	serverpb.RegisterClusterServer(srv, fake)

	go srv.Serve(lis) //nolint:errcheck

	t.Cleanup(srv.Stop)

	var (
		mu        sync.Mutex
		endpoints []string
	)

	dialer := func(_ context.Context, endpoint string, _ bool) (*grpc.ClientConn, error) {
		mu.Lock()
		endpoints = append(endpoints, endpoint)
		mu.Unlock()

		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
	}

	return dialer, &endpoints
}

// Cipher returns the AES block cipher for a base64 cluster_secret.
func Cipher(t testing.TB, secret string) cipher.Block {
	t.Helper()

	key, err := base64.StdEncoding.DecodeString(secret)
	must(t, err)

	block, err := aes.NewCipher(key)
	must(t, err)

	return block
}

// EncryptAffiliate seals an affiliate the way Talos nodes do: AES-GCM with a
// random 12-byte nonce prepended to the ciphertext.
func EncryptAffiliate(t testing.TB, block cipher.Block, a *clientpb.Affiliate) []byte {
	t.Helper()

	gcm, err := cipher.NewGCM(block)
	must(t, err)

	data, err := a.MarshalVT()
	must(t, err)

	nonce := make([]byte, gcm.NonceSize())
	_, err = rand.Read(nonce)
	must(t, err)

	return gcm.Seal(nonce, nonce, data, nil)
}

// EncryptEndpoint encrypts an endpoint record as AES-ECB over
// [len][proto][zero padding], like discovery-client.
func EncryptEndpoint(t testing.TB, block cipher.Block, addrPort string) []byte {
	t.Helper()

	ap := netip.MustParseAddrPort(addrPort)

	data, err := (&clientpb.Endpoint{Ip: ap.Addr().AsSlice(), Port: uint32(ap.Port())}).MarshalVT()
	must(t, err)

	data = append([]byte{byte(len(data))}, data...)

	if pad := len(data) % block.BlockSize(); pad != 0 {
		data = append(data, bytes.Repeat([]byte{0}, block.BlockSize()-pad)...)
	}

	for i := 0; i < len(data); i += block.BlockSize() {
		block.Encrypt(data[i:i+block.BlockSize()], data[i:i+block.BlockSize()])
	}

	return data
}

// IP returns the raw bytes of an IP address, as stored in affiliate data.
func IP(s string) []byte {
	return netip.MustParseAddr(s).AsSlice()
}

func must(t testing.TB, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}
