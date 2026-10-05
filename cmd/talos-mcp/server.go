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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/logger"
	"github.com/spf13/cobra"
)

// These are variables, not constants, so tests can shorten them.
var (
	// shutdownTimeout bounds the graceful shutdown of the HTTP server.
	shutdownTimeout = 5 * time.Second
	// abortTimeout bounds the wait for tool calls to return once they are
	// canceled after shutdownTimeout.
	abortTimeout = 2 * time.Second
)

const (
	// readHeaderTimeout bounds how long a client may take to send headers.
	readHeaderTimeout = 10 * time.Second
	// sessionTimeout closes MCP sessions that send no request for this long,
	// so clients that go away without a DELETE do not leak sessions.
	sessionTimeout = time.Hour
)

// newServerCmd creates the `server` subcommand that runs the MCP server over streamable HTTP.
func newServerCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run MCP server over streamable HTTP",
		Long:  "Run MCP server over streamable HTTP, served on /mcp with a health check on /healthz",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServer(cmd.Context(), flags)
		},
	}

	flags.AddServerFlags(cmd.Flags())

	return cmd
}

// runServer loads the talosconfig, creates the Talos pool, and serves MCP
// over streamable HTTP until ctx is canceled.
func runServer(ctx context.Context, f *Flags) error {
	addr, err := f.listenAddress()
	if err != nil {
		return err
	}

	cfg, err := f.Config()
	if err != nil {
		return err
	}

	log, err := newLogger(cfg)
	if err != nil {
		return err
	}

	srv, pool, err := newMCPServer(cfg, log)
	if err != nil {
		return err
	}
	defer pool.Close() //nolint:errcheck

	if skipped := pool.Skipped(); cfg.RequireAllContexts && len(skipped) > 0 {
		return fmt.Errorf("--%s: talosconfig contexts skipped: %s", flagRequireAll, strings.Join(skipped, ", "))
	}

	if plain := pool.PlaintextKeys(); len(plain) > 0 {
		log.Info("talosconfig keys are stored unencrypted; consider talos-mcp config encrypt", "clusters", strings.Join(plain, ","))
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}

	log.Info("starting MCP server", "address", ln.Addr().String(), "localhostProtection", !cfg.NoHostCheck)

	return serveMCP(ctx, srv, ln, cfg.NoHostCheck, log)
}

// serveMCP serves srv on ln until ctx is canceled. When it returns, every
// tool call has returned or was abandoned after abortTimeout, and the
// sessions are closed, so the caller can close the Talos pool.
func serveMCP(ctx context.Context, srv *mcp.Server, ln net.Listener, noHostCheck bool, log *slog.Logger) error {
	calls := newCallTracker()
	srv.AddReceivingMiddleware(calls.middleware)

	defer closeSessions(srv, log)

	return serveHTTP(ctx, ln, newHTTPHandler(srv, noHostCheck, log), calls, log)
}

// newHTTPHandler serves MCP on /mcp and a health check on /healthz. Every
// request carries the logger in its context for the logging middleware.
//
// noHostCheck turns off the SDK's DNS rebinding protection, which rejects a
// request that arrives on a loopback address with a non-localhost Host
// header. A sidecar proxy that forwards to 127.0.0.1 sends such requests.
func newHTTPHandler(srv *mcp.Server, noHostCheck bool, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Logger:                     log,
		SessionTimeout:             sessionTimeout,
		DisableLocalhostProtection: noHostCheck,
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok")) //nolint:errcheck
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(logger.Inject(r.Context(), log)))
	})
}

// serveHTTP serves handler on ln until ctx is canceled, then shuts down.
//
// Shutdown stops accepting connections and ends open SSE streams (GET
// requests) at once, since they never finish on their own. Tool calls in
// flight get shutdownTimeout to finish. After that, the call tracker (calls)
// cancels them, they get abortTimeout to return, and the connections are
// closed.
// A Serve error stops the server the same way, without the grace period.
func serveHTTP(ctx context.Context, ln net.Listener, handler http.Handler, calls *callTracker, log *slog.Logger) error {
	// The HTTP request contexts do not inherit the signal context. A POST
	// waits on its request context for the call's response, so a signal must
	// not cancel it, or the result of a call that finishes in the grace
	// period is lost. Tool handlers do not use these contexts: the SDK runs
	// them on the session context, and the call tracker cancels them.
	baseCtx, cancelBase := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelBase()

	streamsCtx, cancelStreams := context.WithCancel(baseCtx)
	defer cancelStreams()

	httpServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				reqCtx, cancel := context.WithCancel(r.Context())
				defer cancel()

				stop := context.AfterFunc(streamsCtx, cancel)
				defer stop()

				r = r.WithContext(reqCtx) //nolint:contextcheck // reqCtx is derived from r.Context()
			}

			handler.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: readHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}
	httpServer.RegisterOnShutdown(cancelStreams)

	// abort cancels the tool calls, waits for them to return, and closes
	// every connection.
	abort := func() error {
		calls.abort()

		if !calls.wait(abortTimeout) {
			log.Warn("tool calls still running after they were canceled", "timeout", abortTimeout)
		}

		cancelBase()

		return httpServer.Close()
	}

	errCh := make(chan error, 1)

	go func() {
		errCh <- httpServer.Serve(ln)
	}()

	select {
	case err := <-errCh:
		abort() //nolint:errcheck // the Serve error is the one to report

		return err
	case <-ctx.Done():
	}

	log.Info("shutting down MCP server")

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	err := httpServer.Shutdown(shutdownCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		log.Warn("canceling tool calls still running after the shutdown timeout", "timeout", shutdownTimeout)

		err = abort()
	}

	if err != nil {
		return fmt.Errorf("failed to shutdown server: %w", err)
	}

	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// closeSessions closes the MCP sessions still open. Closing a session waits
// for its calls in flight, so it runs once they are done or aborted, and
// gives up after abortTimeout if one ignores the cancel.
func closeSessions(srv *mcp.Server, log *slog.Logger) {
	done := make(chan struct{})

	go func() {
		defer close(done)

		for ss := range srv.Sessions() {
			if err := ss.Close(); err != nil {
				log.Debug("closing MCP session", "session_id", ss.ID(), "error", err)
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(abortTimeout):
		log.Warn("MCP sessions still open after the shutdown", "timeout", abortTimeout)
	}
}

// errShuttingDown answers requests that arrive after the calls were aborted.
var errShuttingDown = errors.New("server is shutting down")

// callTracker gives every MCP request a context that abort cancels, and
// counts the requests in flight. The SDK runs handlers on the session
// context, which nothing cancels, so this is how shutdown stops a tool call
// that hangs, for example on an unreachable Talos endpoint.
type callTracker struct {
	ctx    context.Context //nolint:containedctx // the shutdown signal shared by all calls
	cancel context.CancelFunc

	mu      sync.Mutex
	running int
	aborted bool
	// idle is closed once aborted and no request is running.
	idle chan struct{}
}

func newCallTracker() *callTracker {
	ctx, cancel := context.WithCancel(context.Background())

	return &callTracker{ctx: ctx, cancel: cancel, idle: make(chan struct{})}
}

func (c *callTracker) middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if !c.begin() {
			return nil, errShuttingDown
		}
		defer c.end()

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		stop := context.AfterFunc(c.ctx, cancel) //nolint:contextcheck // c.ctx is the shutdown signal, not a parent
		defer stop()

		return next(ctx, method, req)
	}
}

func (c *callTracker) begin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.aborted {
		return false
	}

	c.running++

	return true
}

func (c *callTracker) end() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.running--
	if c.aborted && c.running == 0 {
		close(c.idle)
	}
}

// abort cancels the requests in flight and rejects every later one.
func (c *callTracker) abort() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.aborted {
		return
	}

	c.aborted = true
	c.cancel()

	if c.running == 0 {
		close(c.idle)
	}
}

// wait reports whether the requests in flight returned within timeout of
// the abort.
func (c *callTracker) wait(timeout time.Duration) bool {
	select {
	case <-c.idle:
		return true
	case <-time.After(timeout):
		return false
	}
}
