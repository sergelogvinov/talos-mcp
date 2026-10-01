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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

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

// noNodeAnswered is the error of a multi-node tool when every node failed.
// Credential errors and down endpoints (endpointErr: the member list could
// not be read through them either) are mapped by talosError. Otherwise the
// endpoints answer and only the nodes failed, so the per-node warnings are
// returned instead of an "endpoint unreachable" that blames the endpoint.
func (t *TalosTools) noNodeAnswered(ctx context.Context, cluster, api string, first, endpointErr error, nodeWarnings []string) error {
	switch code := status.Code(first); {
	case code == codes.PermissionDenied || code == codes.Unauthenticated:
		return t.talosError(ctx, first, cluster, api)
	case endpointErr != nil:
		return t.talosError(ctx, endpointErr, cluster, api)
	case len(nodeWarnings) == 0:
		return t.talosError(ctx, first, cluster, api)
	default:
		return fmt.Errorf("cluster %s: no node answered: %s", cluster, strings.Join(nodeWarnings, "; "))
	}
}

// grpcMessage returns the status message of a gRPC error, or the error text.
func grpcMessage(err error) string {
	if s, ok := status.FromError(err); ok {
		return s.Message()
	}

	return err.Error()
}

// Output caps for log and dmesg lines (design §10).
const (
	defaultTail    = 100
	maxTail        = 1000
	grepWindowMult = 10
	maxGrepWindow  = 10000
	maxLineBytes   = 4096
	// maxReadLine bounds how much of one line is read and sanitized before
	// it is cut to maxLineBytes, so a secret is masked before the cut.
	maxReadLine = 64 * 1024
	cutMarker   = "…"
)

// tailOrDefault validates the tail argument: 0 means defaultTail.
func tailOrDefault(tail int) (int, error) {
	switch {
	case tail == 0:
		return defaultTail, nil
	case tail < 0 || tail > maxTail:
		return 0, fmt.Errorf("invalid tail %d: must be between 1 and %d", tail, maxTail)
	default:
		return tail, nil
	}
}

// grepWindow is how many lines to read so that tail matches of grep can be
// found: tail without grep, 10×tail with it, at most maxGrepWindow.
func grepWindow(tail int, grep string) int {
	if grep == "" {
		return tail
	}

	return min(tail*grepWindowMult, maxGrepWindow)
}

// lineWindow keeps the last n sanitized lines that match grep, capped.
type lineWindow struct {
	n       int
	grep    string // lower-cased
	clean   func(string) string
	ring    []string
	next    int
	matches int
	cut     bool
}

func newLineWindow(n int, grep string, clean func(string) string) *lineWindow {
	return &lineWindow{n: n, grep: strings.ToLower(grep), clean: clean, ring: make([]string, 0, n)}
}

// add sanitizes, filters and stores one line. cut reports that the reader
// already dropped the end of the line.
func (w *lineWindow) add(line string, cut bool) {
	// Sanitize before grep: matching the raw line would let repeated grep
	// calls probe a masked secret one character at a time.
	line = w.clean(strings.TrimRight(line, "\r\n"))

	if w.grep != "" && !strings.Contains(strings.ToLower(line), w.grep) {
		return
	}

	w.matches++

	if capped, ok := capLine(line); ok || cut {
		line = capped
		if cut && !ok {
			line += cutMarker
		}

		w.cut = true
	}

	if len(w.ring) < w.n {
		w.ring = append(w.ring, line)

		return
	}

	w.ring[w.next] = line
	w.next = (w.next + 1) % w.n
}

// lines returns the kept lines, oldest first, and whether lines were dropped
// or cut.
func (w *lineWindow) lines() ([]string, bool) {
	out := make([]string, 0, len(w.ring))
	out = append(out, w.ring[w.next:]...)
	out = append(out, w.ring[:w.next]...)

	return out, w.cut || w.matches > w.n
}

// capLine cuts a line to maxLineBytes on a rune boundary and adds the marker.
func capLine(line string) (string, bool) {
	if len(line) <= maxLineBytes {
		return line, false
	}

	cut := maxLineBytes
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}

	return line[:cut] + cutMarker, true
}

// readLines calls fn for each line of r. A line longer than maxReadLine is
// passed truncated, with cut set, and the rest of it is discarded.
func readLines(r io.Reader, fn func(line string, cut bool)) error {
	br := bufio.NewReaderSize(r, maxReadLine)

	for {
		chunk, err := br.ReadSlice('\n')

		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			fn(string(chunk), true)

			// Discard the rest of the long line.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}

			switch {
			case errors.Is(err, io.EOF):
				return nil
			case err != nil:
				return err
			}
		case err == nil:
			fn(string(chunk), false)
		case errors.Is(err, io.EOF):
			if len(chunk) > 0 {
				fn(string(chunk), false)
			}

			return nil
		default:
			if len(chunk) > 0 {
				fn(string(chunk), false)
			}

			return err
		}
	}
}

// linesResult renders log-like output as a one-line header followed by the
// lines (design §10), next to the structured result.
func linesResult[T any](header string, lines []string, result *T) (*mcp.CallToolResult, T, error) {
	var b bytes.Buffer

	b.WriteString(header)
	b.WriteByte('\n')

	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: strings.TrimRight(b.String(), "\n")}},
	}, *result, nil
}

// linesHeader is the one-line header of log-like text output, e.g.
// "kubelet logs on prod/worker-1 (10.0.0.21): 50 lines (truncated)",
// followed by any warnings.
func linesHeader(what, cluster, node string, count int, truncated bool, warnings []string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s on %s/%s: %d lines", what, cluster, node, count)

	if truncated {
		b.WriteString(" (truncated)")
	}

	for _, w := range warnings {
		b.WriteString("\nwarning: " + w)
	}

	return b.String()
}
