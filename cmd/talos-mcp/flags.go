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
	"os"
	"strconv"

	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/logger"
	"github.com/spf13/pflag"
)

const (
	flagTalosConfig      = "talosconfig"
	flagContext          = "context"
	flagExtensions       = "extensions"
	flagAllowDestructive = "allow-destructive"
	flagLogLevel         = "log-level"
	flagLogFormat        = "log-format"
	flagPort             = "port"

	envTalosConfig      = "TALOSCONFIG"
	envContext          = "TALOS_CONTEXT"
	envExtensions       = "EXTENSIONS"
	envAllowDestructive = "ALLOW_DESTRUCTIVE"
	envLogLevel         = "LOG_LEVEL"
	envLogFormat        = "LOG_FORMAT"
	envPort             = "PORT"
)

const (
	defaultExtensions       = "all"
	defaultAllowDestructive = false
	defaultLogLevel         = "info"
	defaultLogFormat        = "text"
	defaultPort             = 8080
	defaultOutputFormat     = "text"
)

// Flags holds the command-line flags of every subcommand.
type Flags struct {
	TalosConfig      string
	Context          string
	Extensions       string
	AllowDestructive bool
	LogLevel         string
	LogFormat        string
	Port             int
	Output           string
}

// DefaultFlags returns the default flags for the command,
// populated from environment variables where applicable.
func DefaultFlags() *Flags {
	return &Flags{
		TalosConfig:      withDefaultEnv(envTalosConfig, ""),
		Context:          withDefaultEnv(envContext, ""),
		Extensions:       withDefaultEnv(envExtensions, defaultExtensions),
		AllowDestructive: withDefaultEnvBool(envAllowDestructive, defaultAllowDestructive),
		LogLevel:         withDefaultEnv(envLogLevel, defaultLogLevel),
		LogFormat:        withDefaultEnv(envLogFormat, defaultLogFormat),
		Port:             withDefaultEnvInt(envPort, defaultPort),
		Output:           defaultOutputFormat,
	}
}

// AddPersistentFlags adds the global flags shared by every subcommand.
func (f *Flags) AddPersistentFlags(flags *pflag.FlagSet) {
	flags.StringVarP(&f.TalosConfig, flagTalosConfig, "", f.TalosConfig, "path to the talosconfig file (default: ~/.talos/config)")
	flags.StringVarP(&f.Context, flagContext, "", f.Context, "restrict the server to a single talosconfig context (default: all contexts)")
	flags.StringVarP(&f.Extensions, flagExtensions, "", f.Extensions, "comma-separated list of tool groups to enable: cluster, node, or 'all'")
	flags.BoolVarP(&f.AllowDestructive, flagAllowDestructive, "", f.AllowDestructive, "allow destructive operations (default: false)")
	flags.StringVarP(&f.LogLevel, flagLogLevel, "", f.LogLevel, "log level: debug, info, warn, error")
	flags.StringVarP(&f.LogFormat, flagLogFormat, "", f.LogFormat, "log output format: text, json")
}

// AddServerFlags adds the flags for the "server" subcommand.
func (f *Flags) AddServerFlags(flags *pflag.FlagSet) {
	flags.IntVarP(&f.Port, flagPort, "", f.Port, "http listen port")
}

// AddToolFlags adds the flags for the "tools" subcommand.
func (f *Flags) AddToolFlags(flags *pflag.FlagSet) {
	flags.StringVarP(&f.Output, "output", "o", defaultOutputFormat, "output format: text, json, yaml")
}

// Config returns the internal config populated from the parsed flags.
// It resolves the talosconfig path and validates the enumerated values.
func (f *Flags) Config() (*config.Config, error) {
	path, err := config.ResolveTalosConfigPath(f.TalosConfig)
	if err != nil {
		return nil, err
	}

	if _, err := config.ParseExtensions(f.Extensions); err != nil {
		return nil, err
	}

	if err := logger.Level(f.LogLevel).Validate(); err != nil {
		return nil, err
	}

	if err := logger.Format(f.LogFormat).Validate(); err != nil {
		return nil, err
	}

	return &config.Config{
		TalosConfig:      path,
		Context:          f.Context,
		Port:             f.Port,
		Extensions:       f.Extensions,
		AllowDestructive: f.AllowDestructive,
		LogLevel:         f.LogLevel,
		LogFormat:        f.LogFormat,
	}, nil
}

// withDefaultEnv returns the environment value, or def when it is unset or empty.
func withDefaultEnv(key string, def string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return def
}

func withDefaultEnvInt(key string, def int) int {
	if val, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(val); err == nil {
			return n
		}
	}
	return def
}

func withDefaultEnvBool(key string, def bool) bool {
	if val, ok := os.LookupEnv(key); ok {
		switch val {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		}
	}
	return def
}
