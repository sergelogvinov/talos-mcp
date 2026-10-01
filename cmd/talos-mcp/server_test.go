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

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/talos/talostest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a log sink that the server goroutines and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// testServer is an MCP server serving on a free local port.
type testServer struct {
	url  string
	logs *syncBuffer
	// stop starts the shutdown, and wait returns serveHTTP's result.
	stop context.CancelFunc
	wait func() error
}

// startServer serves the MCP server for a reader talosconfig.
func startServer(t *testing.T) *testServer {
	t.Helper()

	path := filepath.Join(t.TempDir(), "talosconfig")
	require.NoError(t, os.WriteFile(path, []byte(talostest.TalosConfig(t, time.Now(),
		talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Nodes: []string{"10.0.0.11"}},
	)), 0o600))

	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))

	srv, pool, err := newMCPServer(&config.Config{TalosConfig: path, Extensions: defaultExtensions}, log)
	require.NoError(t, err)

	t.Cleanup(func() { pool.Close() }) //nolint:errcheck

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- serveHTTP(ctx, ln, newHTTPHandler(srv, log), log) }()

	wait := sync.OnceValue(func() error { return <-done })

	t.Cleanup(func() {
		stop()
		wait() //nolint:errcheck
	})

	return &testServer{url: "http://" + ln.Addr().String(), logs: logs, stop: stop, wait: wait}
}

func TestServerHealthz(t *testing.T) {
	ts := startServer(t)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.url+"/healthz", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ok", string(body))
}

func TestServerMCP(t *testing.T) {
	ts := startServer(t)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "dev"}, nil)

	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: ts.url + "/mcp"}, nil)
	require.NoError(t, err)

	defer session.Close() //nolint:errcheck

	tools, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)

	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}

	assert.Contains(t, names, "talos_clusters_list")
	assert.NotContains(t, names, "talos_node_reboot", "destructive tools are off by default")

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "talos_clusters_list",
		Arguments: map[string]any{},
	})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Len(t, res.Content, 1)
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, "Default cluster: prod")

	assert.Contains(t, ts.logs.String(), "msg=calling method=tools/call name=talos_clusters_list",
		"the logging middleware sees calls made over HTTP")

	// The client still holds its session and standing SSE stream, so
	// shutdown has to end them.
	start := time.Now()

	ts.stop()

	require.NoError(t, ts.wait())
	assert.Less(t, time.Since(start), shutdownTimeout/2, "the standing stream does not hold up shutdown")
	assert.NotContains(t, ts.logs.String(), "level=WARN")
	assert.Contains(t, ts.logs.String(), "shutting down MCP server")
}

func TestServeHTTPShutdown(t *testing.T) {
	streamStarted, streamEnded := make(chan struct{}), make(chan struct{})
	callStarted, release := make(chan struct{}), make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			close(streamStarted)
			<-r.Context().Done()
			close(streamEnded)

			return
		}

		close(callStarted)
		<-release
		w.Write([]byte("done")) //nolint:errcheck
	})

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	url := "http://" + ln.Addr().String()
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- serveHTTP(ctx, ln, handler, slog.New(slog.DiscardHandler)) }()

	send := func(method string) <-chan string {
		body := make(chan string, 1)

		go func() {
			defer close(body)

			req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
			if err != nil {
				return
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}

			defer resp.Body.Close() //nolint:errcheck

			data, _ := io.ReadAll(resp.Body) //nolint:errcheck
			body <- string(data)
		}()

		return body
	}

	send(http.MethodGet)
	<-streamStarted

	call := send(http.MethodPost)
	<-callStarted

	stop()

	select {
	case <-streamEnded:
	case <-time.After(time.Second):
		t.Fatal("the standing stream did not end when shutdown started")
	}

	select {
	case err := <-done:
		t.Fatalf("server stopped before the call in flight finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	assert.Equal(t, "done", <-call, "the call in flight finishes within the grace period")
	require.NoError(t, <-done)
}

func TestServerInvalidPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "talosconfig")
	require.NoError(t, os.WriteFile(path, []byte(talostest.TalosConfig(t, time.Now(), talostest.Context{Name: "prod", Roles: []string{"os:reader"}})), 0o600))

	f := DefaultFlags()
	f.TalosConfig = path
	f.Port = 70000

	require.EqualError(t, runServer(t.Context(), f), "invalid port 70000: must be between 1 and 65535")
}
