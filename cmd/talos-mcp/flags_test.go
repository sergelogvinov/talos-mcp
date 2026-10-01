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
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFlagsEnvFallback(t *testing.T) {
	t.Setenv(envTalosConfig, "/etc/talos/config")
	t.Setenv(envContext, "prod")
	t.Setenv(envExtensions, "cluster")
	t.Setenv(envAllowDestructive, "true")
	t.Setenv(envLogLevel, "debug")
	t.Setenv(envLogFormat, "json")
	t.Setenv(envPort, "9090")

	cfg, err := DefaultFlags().Config()
	require.NoError(t, err)

	assert.Equal(t, "/etc/talos/config", cfg.TalosConfig)
	assert.Equal(t, "prod", cfg.Context)
	assert.Equal(t, "cluster", cfg.Extensions)
	assert.True(t, cfg.AllowDestructive)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, "json", cfg.LogFormat)
	assert.Equal(t, 9090, cfg.Port)
}

func TestFlagsOverrideEnv(t *testing.T) {
	t.Setenv(envTalosConfig, "/etc/talos/config")
	t.Setenv(envContext, "prod")

	f := DefaultFlags()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	f.AddPersistentFlags(fs)
	require.NoError(t, fs.Parse([]string{"--talosconfig", "/tmp/mcp-config", "--context", "staging"}))

	cfg, err := f.Config()
	require.NoError(t, err)

	assert.Equal(t, "/tmp/mcp-config", cfg.TalosConfig)
	assert.Equal(t, "staging", cfg.Context)
}

func TestFlagsDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, env := range []string{envTalosConfig, envContext, envExtensions, envAllowDestructive, envLogLevel, envLogFormat, envPort} {
		t.Setenv(env, "")
	}

	cfg, err := DefaultFlags().Config()
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(home, ".talos", "config"), cfg.TalosConfig)
	assert.Empty(t, cfg.Context)
	assert.Equal(t, defaultExtensions, cfg.Extensions)
	assert.False(t, cfg.AllowDestructive)
	assert.Equal(t, defaultLogLevel, cfg.LogLevel)
	assert.Equal(t, defaultLogFormat, cfg.LogFormat)
	assert.Equal(t, defaultPort, cfg.Port)
}

func TestFlagsInvalidValues(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*Flags)
	}{
		{name: "extensions", mutate: func(f *Flags) { f.Extensions = "etcd" }},
		{name: "log level", mutate: func(f *Flags) { f.LogLevel = "trace" }},
		{name: "log format", mutate: func(f *Flags) { f.LogFormat = "xml" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &Flags{Extensions: defaultExtensions, LogLevel: defaultLogLevel, LogFormat: defaultLogFormat}
			tt.mutate(f)

			_, err := f.Config()
			require.Error(t, err)
		})
	}
}
