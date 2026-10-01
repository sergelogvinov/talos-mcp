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

package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/logger"
	"github.com/sergelogvinov/talos-mcp/pkg/formatter"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Per-call timeouts (design §6).
const (
	defaultTimeout   = 30 * time.Second
	aggregateTimeout = 60 * time.Second
)

// textResult returns the text rendering of a structured result, next to the
// structured value itself (design §7).
func textResult[T any](result *T) (*mcp.CallToolResult, T, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: formatter.ToText(result)},
		},
	}, *result, nil
}

// withTimeout bounds one tool call. Use aggregateTimeout for tools that fan
// out over several nodes.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

// talosError maps a Talos API error to a short, actionable message (design
// §11). api names the call, such as "Logs", for the permission message.
func (t *TalosTools) talosError(ctx context.Context, cause error, cluster, api string) error {
	if cause == nil {
		return nil
	}

	err := cause

	name, rerr := t.pool.Resolve(cluster)
	if rerr != nil {
		return rerr
	}

	code := status.Code(err)

	switch {
	case code == codes.Unavailable || code == codes.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded):
		endpoints, _, err := t.pool.ContextInfo(name)
		if err != nil {
			return err
		}

		return fmt.Errorf("cluster %s: endpoint %s unreachable: %s", name, strings.Join(endpoints, ", "), grpcMessage(cause))
	case code == codes.PermissionDenied:
		role := t.pool.Role(name)

		logger.FromContext(ctx).Warn("Talos denied a call the role table allows; the role table may not match this Talos version",
			"cluster", name, "api", api, "role", role.String())

		return fmt.Errorf("cluster %s: credential role %s lacks permission for %s", name, role, api)
	case code == codes.Unauthenticated:
		if cred := t.pool.Credential(name); cred != nil && cred.Expired(time.Now()) {
			return fmt.Errorf("cluster %s: client certificate expired at %s", name, cred.NotAfter.UTC().Format(time.RFC3339))
		}

		return fmt.Errorf("cluster %s: authentication failed: %s", name, grpcMessage(err))
	default:
		return fmt.Errorf("cluster %s: %s: %w", name, api, err)
	}
}

// grpcMessage returns the status message of a gRPC error, or the error text.
func grpcMessage(err error) string {
	if s, ok := status.FromError(err); ok {
		return s.Message()
	}

	return err.Error()
}
