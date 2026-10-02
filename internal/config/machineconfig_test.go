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

package config_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/role"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// machineConfig generates a machine config for cluster "prod".
func machineConfig(t *testing.T, typ machine.Type, opts ...generate.Option) []byte {
	t.Helper()

	in, err := generate.NewInput("prod", "https://10.0.0.1:6443", constants.DefaultKubernetesVersion, opts...)
	require.NoError(t, err)

	cfg, err := in.Config(typ)
	require.NoError(t, err)

	data, err := cfg.EncodeBytes()
	require.NoError(t, err)

	return data
}

func importOptions() config.ImportOptions {
	return config.ImportOptions{
		Roles: role.MakeSet(role.Reader),
		TTL:   24 * time.Hour,
		Now:   time.Now(),
	}
}

func TestImportMachineConfig(t *testing.T) {
	data := machineConfig(t, machine.TypeControlPlane)

	res, err := config.ImportMachineConfig(data, importOptions())
	require.NoError(t, err)

	assert.Equal(t, "prod", res.ClusterName)
	assert.Equal(t, []string{"10.0.0.1"}, res.Context.Endpoints)
	assert.Empty(t, res.DiscoveryNote)

	mc, err := configloader.NewFromBytes(data)
	require.NoError(t, err)

	require.NotNil(t, res.Context.Discovery)
	assert.Equal(t, mc.DiscoveryIdentityConfig().ClusterID(), res.Context.Discovery.ClusterID)
	assert.Equal(t, mc.DiscoveryIdentityConfig().ClusterSecret(), res.Context.Discovery.ClusterSecret)
	assert.Equal(t, base64.StdEncoding.EncodeToString(mc.Machine().Security().IssuingCA().Crt), res.Context.CA)

	// The CA key signs the certificate and is not written anywhere.
	caKey := base64.StdEncoding.EncodeToString(mc.Machine().Security().IssuingCA().Key)

	out, err := config.SetContext(nil, "prod", res.Context, false)
	require.NoError(t, err)
	assert.NotContains(t, string(out), caKey)

	tc, err := config.ParseTalosConfig(out, "")
	require.NoError(t, err)
	assert.Equal(t, "prod", tc.Current)
	assert.Empty(t, tc.Warnings)
	require.Contains(t, tc.Discovery, "prod")
	assert.Equal(t, config.DefaultDiscoveryEndpoint, tc.Discovery["prod"].Endpoint)
}

func TestImportMachineConfigOptions(t *testing.T) {
	o := importOptions()
	o.Endpoints = []string{"cp1.example.com", "cp2.example.com"}
	o.Nodes = []string{"10.0.0.2"}
	o.NoDiscovery = true

	res, err := config.ImportMachineConfig(machineConfig(t, machine.TypeControlPlane), o)
	require.NoError(t, err)

	assert.Equal(t, o.Endpoints, res.Context.Endpoints)
	assert.Equal(t, o.Nodes, res.Context.Nodes)
	assert.Nil(t, res.Context.Discovery)
	assert.Empty(t, res.DiscoveryNote)
}

func TestImportMachineConfigDiscoveryDisabled(t *testing.T) {
	res, err := config.ImportMachineConfig(machineConfig(t, machine.TypeControlPlane, generate.WithClusterDiscovery(false)), importOptions())
	require.NoError(t, err)

	assert.Nil(t, res.Context.Discovery)
	assert.Contains(t, res.DiscoveryNote, "disabled")
}

func TestImportMachineConfigErrors(t *testing.T) {
	_, err := config.ImportMachineConfig(machineConfig(t, machine.TypeWorker), importOptions())
	require.ErrorIs(t, err, config.ErrNoTalosCAKey)

	_, err = config.ImportMachineConfig([]byte("not: [a machine config"), importOptions())
	require.ErrorIs(t, err, config.ErrInvalidMachineConfig)

	o := importOptions()
	o.Roles = role.MakeSet()
	_, err = config.ImportMachineConfig(machineConfig(t, machine.TypeControlPlane), o)
	require.Error(t, err)
}

func TestSetContext(t *testing.T) {
	existing := []byte("# my clusters\ncontext: old\ncontexts:\n    old:\n        endpoints:\n            - old.example.com\n")
	entry := &config.ContextEntry{Endpoints: []string{"new.example.com"}, CA: "Y2E=", Crt: "Y3J0", Key: "a2V5"}

	out, err := config.SetContext(existing, "new", entry, false)
	require.NoError(t, err)

	s := string(out)
	assert.Contains(t, s, "# my clusters")
	assert.Contains(t, s, "context: old", "the current context is kept")
	assert.Contains(t, s, "old.example.com")
	assert.Contains(t, s, "new.example.com")

	_, err = config.SetContext(out, "new", entry, false)
	require.ErrorIs(t, err, config.ErrContextExists)

	entry.Endpoints = []string{"replaced.example.com"}
	out, err = config.SetContext(out, "new", entry, true)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "new.example.com")
	assert.Equal(t, 1, strings.Count(string(out), "replaced.example.com"))
}
