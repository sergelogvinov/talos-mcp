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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/logger"
	"github.com/spf13/cobra"
)

const (
	// shutdownTimeout bounds the graceful shutdown of the HTTP server.
	shutdownTimeout = 5 * time.Second
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
	cfg, err := f.Config()
	if err != nil {
		return err
	}

	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("invalid port %d: must be between 1 and 65535", cfg.Port)
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

	addr := fmt.Sprintf(":%d", cfg.Port)

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}

	log.Info("starting MCP server", "address", addr)

	return serveHTTP(ctx, ln, newHTTPHandler(srv, log), log)
}

// newHTTPHandler serves MCP on /mcp and a health check on /healthz. Every
// request carries the logger in its context for the logging middleware.
func newHTTPHandler(srv *mcp.Server, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Logger:         log,
		SessionTimeout: sessionTimeout,
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
// Standing SSE streams (GET requests) end as soon as shutdown starts, since
// they never finish on their own. Other requests in flight, such as tool
// calls, get shutdownTimeout to finish before their connections are closed.
func serveHTTP(ctx context.Context, ln net.Listener, handler http.Handler, log *slog.Logger) error {
	// Request contexts do not inherit the signal context, so a signal does not
	// abort a tool call before the grace period. They end when the server is
	// closed.
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

	errCh := make(chan error, 1)

	go func() {
		errCh <- httpServer.Serve(ln)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down MCP server")

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	err := httpServer.Shutdown(shutdownCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		log.Warn("closing connections still open after the shutdown timeout", "timeout", shutdownTimeout)

		cancelBase()

		err = httpServer.Close()
	}

	if err != nil {
		return fmt.Errorf("failed to shutdown server: %w", err)
	}

	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}
