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

package tools_test

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	"github.com/cosi-project/runtime/pkg/state/impl/namespaced"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/sergelogvinov/talos-mcp/internal/talos/talostest"
	"github.com/sergelogvinov/talos-mcp/internal/tools"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// nodeContexts has a reader cluster with one default node and discovery keys
// (so the discovery secret is a masked literal), and an operator cluster.
func nodeContexts() []talostest.Context {
	return []talostest.Context{
		{Name: "prod", Roles: []string{"os:reader"}, Endpoints: []string{"10.0.0.10"}, Nodes: []string{"10.0.0.21"}, Discovery: "prod-id"},
		{Name: "staging", Roles: []string{"os:operator"}},
	}
}

// newNodeTools builds tools whose Talos clients are fake.
func newNodeTools(t *testing.T, fake *talostest.FakeClient, contexts ...talostest.Context) *tools.TalosTools {
	t.Helper()

	tc, err := config.ParseTalosConfig([]byte(talostest.TalosConfig(t, now, contexts...)), "")
	require.NoError(t, err)

	pool, err := talos.NewPool(tc,
		talos.WithClock(func() time.Time { return now }),
		talos.WithClientFactory(func(context.Context, *clientconfig.Config, string) (talos.Client, error) { return fake, nil }),
	)
	require.NoError(t, err)

	t.Cleanup(func() { pool.Close() }) //nolint:errcheck

	return tools.NewTalosTools(pool, false, allExtensions())
}

// numbered returns n log lines "<prefix> <0-2 x> line <i>".
func numbered(prefix string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString(prefix)
		b.WriteString(" ")
		b.WriteString(strings.Repeat("x", i%3))
		b.WriteString(" line ")
		b.WriteString(strconv.Itoa(i))
		b.WriteString("\n")
	}

	return b.String()
}

func nodeLogsArgs(service string, tail int) tools.NodeLogsInput {
	return tools.NodeLogsInput{Service: service, Tail: tail}
}

func nodeDmesgArgs(tail int, grep string) tools.NodeDmesgInput {
	return tools.NodeDmesgInput{Tail: tail, Grep: grep}
}

func decode(t *testing.T, structured any, out any) {
	t.Helper()

	data, err := json.Marshal(structured)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, out))
}

// emptyState is a COSI state without Members.
func emptyState() state.State {
	return state.WrapCore(namespaced.NewState(inmem.Build))
}

// golden compares text with testdata/<name>.golden; -update rewrites it.
func golden(t *testing.T, name, text string) {
	t.Helper()

	path := filepath.Join("testdata", name+".golden")

	if *update {
		require.NoError(t, os.WriteFile(path, []byte(text+"\n"), 0o600))
	}

	want, err := os.ReadFile(path)
	require.NoError(t, err, "run go test -update to create the golden file")
	assert.Equal(t, string(want), text+"\n")
}

func callText(t *testing.T, tt *tools.TalosTools, name string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()

	res, err := newServer(t, tt).CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.Len(t, res.Content, 1)

	return res, res.Content[0].(*mcp.TextContent).Text
}

func TestNodeLogs(t *testing.T) {
	for _, tc := range []struct {
		name              string
		args              map[string]any
		logs              string
		expectedLines     []string
		expectedTruncated bool
		expectedCall      talostest.LogsCall
	}{
		{
			name:              "tail of a service, more lines available",
			args:              map[string]any{"service": "kubelet", "tail": 3},
			logs:              numbered("kubelet", 5),
			expectedLines:     []string{"kubelet  line 3", "kubelet x line 4", "kubelet xx line 5"},
			expectedTruncated: true,
			expectedCall:      talostest.LogsCall{Node: "10.0.0.21", Namespace: "system", Driver: common.ContainerDriver_CONTAINERD, ID: "kubelet", TailLines: 4},
		},
		{
			name:          "all lines fit",
			args:          map[string]any{"service": "etcd", "tail": 10, "node": "10.0.0.22"},
			logs:          numbered("etcd", 2),
			expectedLines: []string{"etcd x line 1", "etcd xx line 2"},
			expectedCall:  talostest.LogsCall{Node: "10.0.0.22", Namespace: "system", Driver: common.ContainerDriver_CONTAINERD, ID: "etcd", TailLines: 11},
		},
		{
			name:              "grep reads a 10x window and keeps the last matches",
			args:              map[string]any{"service": "kubelet", "tail": 2, "grep": "XX"},
			logs:              numbered("kubelet", 12),
			expectedLines:     []string{"kubelet xx line 8", "kubelet xx line 11"},
			expectedTruncated: true,
			expectedCall:      talostest.LogsCall{Node: "10.0.0.21", Namespace: "system", Driver: common.ContainerDriver_CONTAINERD, ID: "kubelet", TailLines: 21},
		},
		{
			name:          "kubernetes container",
			args:          map[string]any{"service": "abc123", "kubernetes": true},
			logs:          "container started\n",
			expectedLines: []string{"container started"},
			expectedCall:  talostest.LogsCall{Node: "10.0.0.21", Namespace: "k8s.io", Driver: common.ContainerDriver_CRI, ID: "abc123", TailLines: 101},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &talostest.FakeClient{LogText: map[string]string{tc.args["service"].(string): tc.logs}, Chunk: 7}
			tt := newNodeTools(t, fake, nodeContexts()...)

			res, _ := callText(t, tt, tools.ToolNodeLogs, tc.args)
			require.False(t, res.IsError, "%v", res.Content)

			var result tools.NodeLogsResult
			decode(t, res.StructuredContent, &result)

			assert.Equal(t, tc.expectedLines, result.Lines)
			assert.Equal(t, len(tc.expectedLines), result.Count)
			assert.Equal(t, tc.expectedTruncated, result.Truncated)
			assert.Equal(t, []talostest.LogsCall{tc.expectedCall}, fake.Calls())
		})
	}
}

func TestNodeLogsSanitizesAndCaps(t *testing.T) {
	// Spaced words: a long run of one character is a "generic secret" to
	// the sanitizer and would be masked.
	long := strings.Repeat("ab ", 2000)
	huge := strings.Repeat("cd ", 25*1024)

	fake := &talostest.FakeClient{LogText: map[string]string{"machined": "user=admin password=hunter2\n" +
		"discovery secret " + talostest.Secret + " loaded\n" +
		long + "\n" + huge + "\n" + "after the huge line\n"}}
	tt := newNodeTools(t, fake, nodeContexts()...)

	result, err := tt.NodeLogs(t.Context(), nodeLogsArgs("machined", 0))
	require.NoError(t, err)
	require.Len(t, result.Lines, 5)

	assert.Equal(t, "user=admin password=***", result.Lines[0])
	assert.Equal(t, "discovery secret *** loaded", result.Lines[1], "the configured cluster_secret is masked")
	assert.Equal(t, long[:4096]+"…", result.Lines[2])
	assert.Equal(t, huge[:4096]+"…", result.Lines[3])
	assert.Equal(t, "after the huge line", result.Lines[4], "the rest of a huge line is skipped")
	assert.True(t, result.Truncated, "cut lines set truncated")
}

func TestNodeLogsGolden(t *testing.T) {
	fake := &talostest.FakeClient{LogText: map[string]string{"kubelet": "I1001 kubelet started\nE1001 token=abc123 rejected\nI1001 node ready\n"}}
	tt := newNodeTools(t, fake, nodeContexts()...)

	_, text := callText(t, tt, tools.ToolNodeLogs, map[string]any{"service": "kubelet", "tail": 2})
	golden(t, "node_logs", text)
}

func TestNodeLogsErrors(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fake        *talostest.FakeClient
		args        map[string]any
		expectedMsg string
	}{
		{
			name:        "unknown service lists the node's services",
			fake:        &talostest.FakeClient{LogsStreamErr: status.Error(codes.NotFound, "not found"), Services: []string{"kubelet", "apid", "etcd"}},
			args:        map[string]any{"service": "kubelt"},
			expectedMsg: `unknown service "kubelt" on prod/10.0.0.21; services: apid, etcd, kubelet (use kubernetes=true for a container id)`,
		},
		{
			name:        "known service keeps the Talos error",
			fake:        &talostest.FakeClient{LogsErr: status.Error(codes.Unavailable, "connection refused"), Services: []string{"kubelet"}},
			args:        map[string]any{"service": "kubelet"},
			expectedMsg: "cluster prod: endpoint 10.0.0.10 unreachable: connection refused",
		},
		{
			name:        "container errors don't look up services",
			fake:        &talostest.FakeClient{LogsErr: status.Error(codes.NotFound, "no such container")},
			args:        map[string]any{"service": "abc", "kubernetes": true},
			expectedMsg: "cluster prod: Logs: rpc error: code = NotFound desc = no such container",
		},
		{
			name:        "tail out of range",
			fake:        &talostest.FakeClient{},
			args:        map[string]any{"service": "kubelet", "tail": 1001},
			expectedMsg: "invalid tail 1001: must be between 1 and 1000",
		},
		{
			name:        "default node needs exactly one talosconfig node",
			fake:        &talostest.FakeClient{},
			args:        map[string]any{"service": "kubelet", "cluster": "staging"},
			expectedMsg: "no default node",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fake.LogText == nil {
				tc.fake.LogText = map[string]string{}
			}

			if tc.name == "default node needs exactly one talosconfig node" {
				tc.fake.St = emptyState()
			}

			res, text := callText(t, newNodeTools(t, tc.fake, nodeContexts()...), tools.ToolNodeLogs, tc.args)
			require.True(t, res.IsError)
			assert.Contains(t, text, tc.expectedMsg)
		})
	}
}

func TestNodeDmesg(t *testing.T) {
	fake := &talostest.FakeClient{Kmsg: []string{
		"kern:    info: [2026-10-01T10:00:00.123456789Z]: Linux version 6.12.0-talos",
		"kern:    warn: [2026-10-01T10:00:01.5Z]: eth0: link down",
		"user:  notice: [2026-10-01T10:00:02Z]: machined: token=abcdef.0123456789abcdef",
		"kern:     err: [2026-10-01T10:00:03Z]: eth0: link up",
		"not in kmsg format",
	}}
	tt := newNodeTools(t, fake, nodeContexts()...)

	result, err := tt.NodeDmesg(t.Context(), nodeDmesgArgs(3, ""))
	require.NoError(t, err)
	assert.Equal(t, []string{
		"2026-10-01T10:00:02Z user.notice machined: token=***",
		"2026-10-01T10:00:03Z kern.err eth0: link up",
		"not in kmsg format",
	}, result.Lines)
	assert.True(t, result.Truncated)
	assert.Equal(t, []string{"10.0.0.21"}, fake.DmesgNodes)

	result, err = tt.NodeDmesg(t.Context(), nodeDmesgArgs(0, "ETH0"))
	require.NoError(t, err)
	assert.Equal(t, []string{
		"2026-10-01T10:00:01Z kern.warn eth0: link down",
		"2026-10-01T10:00:03Z kern.err eth0: link up",
	}, result.Lines)
	assert.False(t, result.Truncated)

	_, text := callText(t, tt, tools.ToolNodeDmesg, map[string]any{"tail": 2})
	golden(t, "node_dmesg", text)
}

func TestNodeDmesgError(t *testing.T) {
	fake := &talostest.FakeClient{DmesgErr: status.Error(codes.PermissionDenied, "not authorized")}

	res, text := callText(t, newNodeTools(t, fake, nodeContexts()...), tools.ToolNodeDmesg, map[string]any{})
	require.True(t, res.IsError)
	assert.Equal(t, "cluster prod: credential role reader lacks permission for Dmesg", text)
}

// TestNodeGrepSeesOnlySanitizedLines checks that grep can't be used to probe
// a masked secret: a prefix of the secret matches nothing, and the mask does.
func TestNodeGrepSeesOnlySanitizedLines(t *testing.T) {
	fake := &talostest.FakeClient{
		LogText: map[string]string{"machined": "auth password=hunter2 ok\n"},
		Kmsg:    []string{"user: notice: [2026-10-01T10:00:00Z]: auth password=hunter2 ok"},
	}
	tt := newNodeTools(t, fake, nodeContexts()...)

	for _, grep := range []string{"password=h", "hunter2"} {
		logs, err := tt.NodeLogs(t.Context(), tools.NodeLogsInput{Service: "machined", Grep: grep})
		require.NoError(t, err)
		assert.Empty(t, logs.Lines, "logs grep %q must not match the masked value", grep)

		dmesg, err := tt.NodeDmesg(t.Context(), tools.NodeDmesgInput{Grep: grep})
		require.NoError(t, err)
		assert.Empty(t, dmesg.Lines, "dmesg grep %q must not match the masked value", grep)
	}

	logs, err := tt.NodeLogs(t.Context(), tools.NodeLogsInput{Service: "machined", Grep: "password=***"})
	require.NoError(t, err)
	assert.Equal(t, []string{"auth password=*** ok"}, logs.Lines)
}
