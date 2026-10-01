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

// Package main implements the talos-mcp command-line tool.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/logger"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/sergelogvinov/talos-mcp/internal/tools"
	"github.com/spf13/cobra"
)

var (
	version     = "dev"
	commit      = "none"
	bin         = "talos-mcp"
	description = "Talos MCP server"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	rootCmd := newRootCmd()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		errorString := err.Error()
		fmt.Fprintf(os.Stderr, "Error: %s\n\n", errorString)

		if strings.Contains(errorString, "arg(s)") {
			fmt.Fprintln(os.Stderr, rootCmd.UsageString())
		}

		cancel()
		os.Exit(1) //nolint:gocritic
	}
}

func newRootCmd() *cobra.Command {
	flags := DefaultFlags()

	rootCmd := &cobra.Command{
		Use:           bin,
		Short:         "Talos MCP Server - Talos Linux tooling",
		Long:          "Talos MCP Server provides Talos Linux tooling via MCP protocol",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Global (persistent) flags, inherited by every subcommand.
	flags.AddPersistentFlags(rootCmd.PersistentFlags())

	rootCmd.AddCommand(
		newMCPCmd(flags),
		newServerCmd(flags),
		newConfigCmd(flags),
		newToolsCmd(flags),
		newVersionCmd(),
	)

	return rootCmd
}

func newLogger(cfg *config.Config) (*slog.Logger, error) {
	return logger.New(logger.Options{
		Level:  logger.Level(cfg.LogLevel),
		Format: logger.Format(cfg.LogFormat),
	})
}

// newMCPServer loads the talosconfig, builds the client pool and registers
// the tools. The caller closes the pool.
func newMCPServer(cfg *config.Config, log *slog.Logger) (*mcp.Server, *talos.Pool, error) {
	extensions, err := config.ParseExtensions(cfg.Extensions)
	if err != nil {
		return nil, nil, err
	}

	pool, err := talos.LoadPool(cfg)
	if err != nil {
		return nil, nil, err
	}

	for _, w := range pool.Warnings() {
		log.Warn(w)
	}

	log.Info("server config",
		"talosconfig", cfg.TalosConfig,
		"clusters", strings.Join(pool.List(), ","),
		"current", pool.Current(),
		"allowDestructive", cfg.AllowDestructive,
		"extensions", cfg.Extensions,
	)

	srv := mcp.NewServer(&mcp.Implementation{
		Name:        bin,
		Title:       description,
		Description: description,
		Version:     version,
	}, &mcp.ServerOptions{
		Logger: log,
	})
	srv.AddReceivingMiddleware(loggingMiddleware)

	tools.NewTalosTools(pool, cfg.AllowDestructive, extensions).RegisterTools(srv)

	return srv, pool, nil
}

func loggingMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if ctr, ok := req.(*mcp.CallToolRequest); ok {
			params := []any{
				"method", "tools/call",
				"name", ctr.Params.Name,
			}

			if len(ctr.Params.Arguments) > 0 {
				var raw map[string]any
				if err := json.Unmarshal(ctr.Params.Arguments, &raw); err != nil {
					raw = nil
				}

				for k, v := range raw {
					params = append(params, k, fmt.Sprintf("%v", v))
				}
			}

			logger.FromContext(ctx).Info("calling", params...)
		}

		return next(ctx, method, req)
	}
}
