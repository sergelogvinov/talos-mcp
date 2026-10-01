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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/sergelogvinov/talos-mcp/internal/talos/talostest"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const poolSecret = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

// fakeClient is a Client that only records Close. The embedded nil interface
// makes any other call panic, so a test that reaches the network fails loudly.
type fakeClient struct {
	talos.Client

	name   string
	closed bool
	err    error
}

func (f *fakeClient) Close() error {
	f.closed = true

	return f.err
}

// fakeFactory counts client creations per context and can fail on demand.
type fakeFactory struct {
	mu      sync.Mutex
	calls   map[string]int
	fail    map[string]error
	clients []*fakeClient
}

func newFakeFactory() *fakeFactory {
	return &fakeFactory{calls: map[string]int{}, fail: map[string]error{}}
}

func (f *fakeFactory) New(_ context.Context, _ *clientconfig.Config, name string) (talos.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls[name]++

	if err := f.fail[name]; err != nil {
		return nil, err
	}

	c := &fakeClient{name: name}
	f.clients = append(f.clients, c)

	return c, nil
}

// poolConfig is a talosconfig with a reader, an operator and an admin
// cluster. Only "prod" has a discovery block.
func poolConfig(t *testing.T) string {
	t.Helper()

	year := now.Add(365 * 24 * time.Hour)

	var b strings.Builder

	b.WriteString("context: prod\ncontexts:\n")

	for _, c := range []struct {
		name, crt, extra string
	}{
		{"prod", talostest.Crt(t, year, "os:reader"), "    nodes: [10.0.0.11]\n    discovery:\n      cluster_id: prod-id\n      cluster_secret: " + poolSecret + "\n"},
		{"staging", talostest.Crt(t, year, "os:operator"), ""},
		{"dev", talostest.Crt(t, year, "os:admin"), ""},
		{"backup", talostest.Crt(t, year, "os:etcd:backup"), ""},
	} {
		fmt.Fprintf(&b, "  %s:\n    endpoints: [%s.example.com]\n    ca: Y2E=\n    crt: %s\n    key: a2V5\n%s", c.name, c.name, c.crt, c.extra)
	}

	return b.String()
}

func newTestPool(t *testing.T, filter string, factory *fakeFactory) *talos.Pool {
	t.Helper()

	tc, err := config.ParseTalosConfig([]byte(poolConfig(t)), filter)
	require.NoError(t, err)

	pool, err := talos.NewPool(tc,
		talos.WithClientFactory(factory.New),
		talos.WithClock(func() time.Time { return now }),
	)
	require.NoError(t, err)

	return pool
}

func TestPoolOffline(t *testing.T) {
	pool := newTestPool(t, "", newFakeFactory())

	assert.Equal(t, []string{"dev", "prod", "staging"}, pool.List())
	assert.Equal(t, "prod", pool.Current())
	assert.Equal(t, []string{
		`context "backup" skipped: client certificate has no os:reader, os:operator or os:admin role (roles: os:etcd:backup)`,
		`context "dev" uses an os:admin credential; it gets the operator tool set, use a dedicated os:operator or os:reader credential`,
	}, pool.Warnings())

	assert.Equal(t, talos.RoleReader, pool.Role("prod"))
	assert.Equal(t, talos.RoleReader, pool.Role(""), "empty cluster is the current one")
	assert.Equal(t, talos.RoleOperator, pool.Role("staging"))
	assert.Equal(t, talos.RoleOperator, pool.Role("dev"))
	assert.Equal(t, talos.RoleNone, pool.Role("backup"))

	assert.Equal(t, []string{"dev", "prod", "staging"}, pool.ClustersWithRole(talos.RoleReader))
	assert.Equal(t, []string{"dev", "staging"}, pool.ClustersWithRole(talos.RoleOperator))
	assert.Equal(t, []string{"prod"}, pool.ClustersWithDiscovery())

	d := pool.Discovery("prod")
	require.NotNil(t, d)
	assert.Equal(t, "prod-id", d.ClusterID)
	assert.Equal(t, config.DefaultDiscoveryEndpoint, d.Endpoint)
	assert.Nil(t, pool.Discovery("staging"))

	cred := pool.Credential("dev")
	require.NotNil(t, cred)
	assert.True(t, cred.Admin)
	assert.Nil(t, pool.Credential("backup"))
}

func TestPoolResolve(t *testing.T) {
	pool := newTestPool(t, "", newFakeFactory())

	for _, tt := range []struct {
		name          string
		cluster       string
		expected      string
		expectedError string
	}{
		{name: "empty is current", cluster: "", expected: "prod"},
		{name: "known cluster", cluster: "staging", expected: "staging"},
		{name: "unknown cluster lists valid names", cluster: "qa", expectedError: `unknown cluster "qa": valid clusters are dev, prod, staging`},
		{name: "skipped context is unknown", cluster: "backup", expectedError: `unknown cluster "backup": valid clusters are dev, prod, staging`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pool.Resolve(tt.cluster)
			if tt.expectedError != "" {
				require.ErrorIs(t, err, talos.ErrUnknownCluster)
				assert.EqualError(t, err, tt.expectedError)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestPoolContextFilter(t *testing.T) {
	pool := newTestPool(t, "staging", newFakeFactory())

	assert.Equal(t, []string{"staging"}, pool.List())
	assert.Equal(t, "staging", pool.Current())
	assert.Empty(t, pool.ClustersWithDiscovery())
	assert.Empty(t, pool.Warnings())

	_, err := pool.Resolve("prod")
	require.ErrorIs(t, err, talos.ErrUnknownCluster)
}

func TestPoolContextInfo(t *testing.T) {
	pool := newTestPool(t, "", newFakeFactory())

	endpoints, nodes, err := pool.ContextInfo("")
	require.NoError(t, err)
	assert.Equal(t, []string{"prod.example.com"}, endpoints)
	assert.Equal(t, []string{"10.0.0.11"}, nodes)

	// The result is a copy.
	endpoints[0] = "changed"
	endpoints, _, err = pool.ContextInfo("prod")
	require.NoError(t, err)
	assert.Equal(t, []string{"prod.example.com"}, endpoints)

	_, nodes, err = pool.ContextInfo("staging")
	require.NoError(t, err)
	assert.Empty(t, nodes)

	_, _, err = pool.ContextInfo("qa")
	require.ErrorIs(t, err, talos.ErrUnknownCluster)
}

func TestPoolRequire(t *testing.T) {
	pool := newTestPool(t, "", newFakeFactory())

	for _, tt := range []struct {
		name          string
		cluster       string
		required      talos.Role
		expectedError string
	}{
		{name: "reader on reader cluster", cluster: "prod", required: talos.RoleReader},
		{name: "reader on operator cluster", cluster: "staging", required: talos.RoleReader},
		{name: "operator on operator cluster", cluster: "staging", required: talos.RoleOperator},
		{name: "operator on admin cluster", cluster: "dev", required: talos.RoleOperator},
		{
			name:          "operator on reader cluster",
			cluster:       "prod",
			required:      talos.RoleOperator,
			expectedError: "credential role denied: cluster prod uses an os:reader credential; os:operator is required",
		},
		{
			name:          "operator on current reader cluster",
			cluster:       "",
			required:      talos.RoleOperator,
			expectedError: "credential role denied: cluster prod uses an os:reader credential; os:operator is required",
		},
		{
			name:          "unknown cluster",
			cluster:       "qa",
			required:      talos.RoleReader,
			expectedError: `unknown cluster "qa": valid clusters are dev, prod, staging`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := pool.Require(tt.cluster, tt.required)
			if tt.expectedError == "" {
				require.NoError(t, err)

				return
			}

			assert.EqualError(t, err, tt.expectedError)
		})
	}
}

func TestPoolClientLazyAndCached(t *testing.T) {
	factory := newFakeFactory()
	pool := newTestPool(t, "", factory)

	assert.Empty(t, factory.calls, "NewPool creates no clients")

	c1, err := pool.Client(t.Context(), "")
	require.NoError(t, err)
	c2, err := pool.Client(t.Context(), "prod")
	require.NoError(t, err)

	assert.Same(t, c1, c2)
	assert.Equal(t, 1, factory.calls["prod"])
	assert.Equal(t, "prod", c1.(*fakeClient).name)

	_, err = pool.Client(t.Context(), "qa")
	require.ErrorIs(t, err, talos.ErrUnknownCluster)
}

func TestPoolClientErrorNotCached(t *testing.T) {
	factory := newFakeFactory()
	factory.fail["staging"] = errors.New("bad key")
	pool := newTestPool(t, "", factory)

	_, err := pool.Client(t.Context(), "staging")
	require.EqualError(t, err, "cluster staging: creating Talos client: bad key")

	delete(factory.fail, "staging")

	c, err := pool.Client(t.Context(), "staging")
	require.NoError(t, err)
	assert.NotNil(t, c)
	assert.Equal(t, 2, factory.calls["staging"], "the failed creation is retried")
}

func TestPoolClientConcurrent(t *testing.T) {
	factory := newFakeFactory()
	pool := newTestPool(t, "", factory)

	var wg sync.WaitGroup

	for range 20 {
		wg.Go(func() {
			_, err := pool.Client(t.Context(), "dev")
			assert.NoError(t, err)
		})
	}

	wg.Wait()

	assert.Equal(t, 1, factory.calls["dev"])
}

func TestPoolClose(t *testing.T) {
	factory := newFakeFactory()
	pool := newTestPool(t, "", factory)

	for _, name := range []string{"prod", "staging"} {
		_, err := pool.Client(t.Context(), name)
		require.NoError(t, err)
	}

	factory.clients[1].err = errors.New("boom")

	require.EqualError(t, pool.Close(), "cluster staging: boom")

	for _, c := range factory.clients {
		assert.True(t, c.closed, c.name)
	}

	_, err := pool.Client(t.Context(), "prod")
	require.ErrorIs(t, err, talos.ErrPoolClosed)
	require.NoError(t, pool.Close(), "a second Close is a no-op")
}

// TestPoolRealClient checks the default factory against a real certificate
// and key. Creating the client makes no network call.
func TestPoolRealClient(t *testing.T) {
	crt, key := talostest.CrtKey(t, time.Now().Add(time.Hour), "os:reader")

	input := fmt.Sprintf("context: prod\ncontexts:\n  prod:\n    endpoints: [127.0.0.1]\n    ca: %s\n    crt: %s\n    key: %s\n", crt, crt, key)

	path := filepath.Join(t.TempDir(), "talosconfig")
	require.NoError(t, os.WriteFile(path, []byte(input), 0o600))

	pool, err := talos.LoadPool(&config.Config{TalosConfig: path})
	require.NoError(t, err)

	c, err := pool.Client(t.Context(), "")
	require.NoError(t, err)
	assert.NotNil(t, c.State())

	require.NoError(t, pool.Close())
}
