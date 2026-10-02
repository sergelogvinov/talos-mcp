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
	"strings"
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
	ln   net.Listener
	logs *syncBuffer
	// stop starts the shutdown, and wait returns serveMCP's result.
	stop context.CancelFunc
	wait func() error
	// hangStarted receives when a test_hang call starts, and hangEnded the
	// error of its context when it returns.
	hangStarted chan struct{}
	hangEnded   chan error
}

// startServer serves the MCP server for a reader talosconfig, plus a
// test_hang tool that runs until its context is canceled.
func startServer(t *testing.T, noHostCheck bool) *testServer {
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

	ts := &testServer{logs: logs, hangStarted: make(chan struct{}, 1), hangEnded: make(chan error, 1)}

	mcp.AddTool(srv, &mcp.Tool{Name: "test_hang"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		ts.hangStarted <- struct{}{}
		<-ctx.Done()
		ts.hangEnded <- ctx.Err()

		return nil, nil, ctx.Err()
	})

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- serveMCP(ctx, srv, ln, noHostCheck, log) }()

	wait := sync.OnceValue(func() error { return <-done })

	t.Cleanup(func() {
		stop()
		wait() //nolint:errcheck
	})

	ts.url, ts.ln, ts.stop, ts.wait = "http://"+ln.Addr().String(), ln, stop, wait

	return ts
}

// connect opens an MCP client session to the server.
func (ts *testServer) connect(t *testing.T) *mcp.ClientSession {
	t.Helper()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "dev"}, nil)

	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: ts.url + "/mcp"}, nil)
	require.NoError(t, err)

	t.Cleanup(func() { session.Close() }) //nolint:errcheck

	return session
}

// shortenShutdown sets the shutdown timeouts for one test.
func shortenShutdown(t *testing.T, shutdown, abort time.Duration) {
	t.Helper()

	oldShutdown, oldAbort := shutdownTimeout, abortTimeout
	shutdownTimeout, abortTimeout = shutdown, abort

	t.Cleanup(func() { shutdownTimeout, abortTimeout = oldShutdown, oldAbort })
}

func TestServerHealthz(t *testing.T) {
	ts := startServer(t, false)

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
	ts := startServer(t, false)
	session := ts.connect(t)

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

func TestServerAbortsHungCall(t *testing.T) {
	shortenShutdown(t, 200*time.Millisecond, 2*time.Second)

	ts := startServer(t, false)
	session := ts.connect(t)

	type callResult struct {
		res *mcp.CallToolResult
		err error
	}

	called := make(chan callResult, 1)

	go func() {
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "test_hang", Arguments: map[string]any{}})
		called <- callResult{res, err}
	}()

	<-ts.hangStarted

	start := time.Now()

	ts.stop()

	require.NoError(t, ts.wait())

	select {
	case err := <-ts.hangEnded:
		require.ErrorIs(t, err, context.Canceled, "the hung call is canceled after the shutdown timeout")
	default:
		t.Fatal("serveMCP returned while the hung call was still running")
	}

	assert.Less(t, time.Since(start), time.Second)
	assert.Contains(t, ts.logs.String(), "canceling tool calls still running after the shutdown timeout")
	assert.NotContains(t, ts.logs.String(), "still running after they were canceled")
	assert.NotContains(t, ts.logs.String(), "MCP sessions still open")

	// The canceled handler returns an error, which reaches the client as an
	// error result if it is written before the connection closes.
	if r := <-called; r.err == nil {
		assert.True(t, r.res.IsError, "the client gets no successful result")
	}
}

func TestServerHostCheck(t *testing.T) {
	for _, tt := range []struct {
		name        string
		noHostCheck bool
		expected    int
	}{
		{name: "on by default", expected: http.StatusForbidden},
		{name: "disabled for a sidecar proxy", noHostCheck: true, expected: http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts := startServer(t, tt.noHostCheck)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url+"/mcp", strings.NewReader(
				`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"proxy","version":"0"}}}`))
			require.NoError(t, err)

			// A proxy that forwards to 127.0.0.1 keeps the external Host.
			req.Host = "talos-mcp.example.com"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)

			resp.Body.Close() //nolint:errcheck,gosec

			assert.Equal(t, tt.expected, resp.StatusCode)
		})
	}
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

	go func() { done <- serveHTTP(ctx, ln, handler, newCallTracker(), slog.New(slog.DiscardHandler)) }()

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

func TestServerServeError(t *testing.T) {
	ts := startServer(t, false)
	session := ts.connect(t)

	go session.CallTool(t.Context(), &mcp.CallToolParams{Name: "test_hang", Arguments: map[string]any{}}) //nolint:errcheck

	<-ts.hangStarted

	// The listener fails under Serve, with no signal.
	require.NoError(t, ts.ln.Close())

	require.ErrorIs(t, ts.wait(), net.ErrClosed)

	select {
	case err := <-ts.hangEnded:
		require.ErrorIs(t, err, context.Canceled)
	default:
		t.Fatal("serveMCP returned while a tool call was still running")
	}
}

// talosconfigFile writes a reader talosconfig and returns its path.
func talosconfigFile(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "talosconfig")
	require.NoError(t, os.WriteFile(path, []byte(talostest.TalosConfig(t, time.Now(), talostest.Context{Name: "prod", Roles: []string{"os:reader"}})), 0o600))

	return path
}

func TestServerInvalidListenAddress(t *testing.T) {
	t.Setenv(envListenAddress, ":9090 ")
	t.Setenv(envTalosConfig, talosconfigFile(t))

	cmd := newServerCmd(DefaultFlags())
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs(nil)
	require.EqualError(t, cmd.ExecuteContext(t.Context()), `invalid listen port "9090 ": must be a number between 1 and 65535`)
}

func TestServerListenDefault(t *testing.T) {
	t.Setenv(envListenAddress, "")
	assert.Equal(t, "127.0.0.1:8080", DefaultFlags().ListenAddress, "the server is local unless asked otherwise")

	t.Setenv(envListenAddress, ":8080")
	assert.Equal(t, ":8080", DefaultFlags().ListenAddress)
}
