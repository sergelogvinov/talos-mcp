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
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/sergelogvinov/talos-mcp/internal/talos/talostest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTalosError(t *testing.T) {
	expired := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	tc, err := config.ParseTalosConfig([]byte(talostest.TalosConfig(t, time.Now(),
		talostest.Context{Name: "prod", Roles: []string{"os:reader"}, Endpoints: []string{"10.0.0.1", "10.0.0.2"}},
		talostest.Context{Name: "old", Roles: []string{"os:reader"}, NotAfter: expired},
	)), "")
	require.NoError(t, err)

	pool, err := talos.NewPool(tc)
	require.NoError(t, err)

	tt := &TalosTools{pool: pool}

	for _, tc := range []struct {
		name     string
		cluster  string
		err      error
		expected string
	}{
		{name: "nil", cluster: "prod", err: nil},
		{
			name:     "unavailable",
			cluster:  "",
			err:      status.Error(codes.Unavailable, "connection refused"),
			expected: "cluster prod: endpoint 10.0.0.1, 10.0.0.2 unreachable: connection refused",
		},
		{
			name:     "deadline exceeded",
			cluster:  "prod",
			err:      context.DeadlineExceeded,
			expected: "cluster prod: endpoint 10.0.0.1, 10.0.0.2 unreachable: context deadline exceeded",
		},
		{
			name:     "permission denied",
			cluster:  "prod",
			err:      status.Error(codes.PermissionDenied, "not authorized"),
			expected: "cluster prod: credential role reader lacks permission for Logs",
		},
		{
			name:     "expired certificate",
			cluster:  "old",
			err:      status.Error(codes.Unauthenticated, "tls: expired certificate"),
			expected: "cluster old: client certificate expired at " + expired.Format(time.RFC3339),
		},
		{
			name:     "unauthenticated",
			cluster:  "prod",
			err:      status.Error(codes.Unauthenticated, "bad certificate"),
			expected: "cluster prod: authentication failed: bad certificate",
		},
		{
			name:     "other error is wrapped",
			cluster:  "prod",
			err:      errors.New("boom"),
			expected: "cluster prod: Logs: boom",
		},
		{
			name:     "unknown cluster",
			cluster:  "qa",
			err:      errors.New("boom"),
			expected: `unknown cluster "qa": valid clusters are old, prod`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tt.talosError(t.Context(), tc.err, tc.cluster, "Logs")
			if tc.expected == "" {
				assert.NoError(t, got)

				return
			}

			assert.EqualError(t, got, tc.expected)
		})
	}
}

func TestTalosErrorKeepsCause(t *testing.T) {
	tc, err := config.ParseTalosConfig([]byte(talostest.TalosConfig(t, time.Now(),
		talostest.Context{Name: "prod", Roles: []string{"os:reader"}},
	)), "")
	require.NoError(t, err)

	pool, err := talos.NewPool(tc)
	require.NoError(t, err)

	cause := errors.New("boom")
	got := (&TalosTools{pool: pool}).talosError(t.Context(), cause, "prod", "Version")
	assert.ErrorIs(t, got, cause)
}

// readerAPIs are the apid methods that allow os:reader in Talos v1.14.2
// (internal/app/machined/pkg/system/services/machined.go), for the methods
// the tools use. Update it when pkg/machinery is bumped (design §2.3).
var readerAPIs = map[string]bool{
	"/machine.MachineService/Dmesg":          true,
	"/machine.MachineService/EtcdMemberList": true,
	"/machine.MachineService/EtcdStatus":     true,
	"/machine.MachineService/Events":         true,
	"/machine.MachineService/LoadAvg":        true,
	"/machine.MachineService/Logs":           true,
	"/machine.MachineService/LogsContainers": true,
	"/machine.MachineService/Memory":         true,
	"/machine.MachineService/Mounts":         true,
	"/machine.MachineService/Processes":      true,
	"/machine.MachineService/ServiceList":    true,
	"/machine.MachineService/SystemStat":     true,
	"/machine.MachineService/Version":        true,
	"/cosi.resource.State/Get":               true,
	"/cosi.resource.State/List":              true,
	"/cosi.resource.State/Watch":             true,
}

// TestReaderToolsUseReaderAPIs fails when a tool open to os:reader calls an
// apid method that os:reader may not call (design §15.1 item 2).
func TestReaderToolsUseReaderAPIs(t *testing.T) {
	tc, err := config.ParseTalosConfig([]byte(talostest.TalosConfig(t, time.Now(),
		talostest.Context{Name: "prod", Roles: []string{"os:operator"}, Discovery: "prod-id"},
	)), "")
	require.NoError(t, err)

	pool, err := talos.NewPool(tc)
	require.NoError(t, err)

	tt := NewTalosTools(pool, true, map[string]bool{config.ExtensionCluster: true, config.ExtensionNode: true})
	tt.RegisterTools(mcp.NewServer(&mcp.Implementation{Name: "test", Version: "dev"}, nil))

	require.NotEmpty(t, tt.registered)

	require.Contains(t, toolNames(tt.registered), ToolNodeReboot, "the operator tool is checked too")

	for _, spec := range tt.registered {
		allowed := readerAPIs
		if spec.minRole == talos.RoleOperator {
			allowed = operatorAPIs
		}

		for _, api := range spec.apis {
			assert.True(t, allowed[api], "%s is open to os:%s but calls %s", spec.name, spec.minRole, api)
		}
	}
}

// operatorAPIs adds the operator-only methods the tools use to readerAPIs
// (Talos v1.14.2).
var operatorAPIs = func() map[string]bool {
	m := map[string]bool{"/machine.MachineService/Reboot": true}
	for api := range readerAPIs {
		m[api] = true
	}

	return m
}()

func toolNames(specs []toolSpec) []string {
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.name)
	}

	return names
}
