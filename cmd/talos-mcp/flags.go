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
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/logger"
	"github.com/sergelogvinov/talos-mcp/internal/secrets"
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
	flagListen           = "listen"
	flagNoHostCheck      = "disable-localhost-protection"
	flagIdentity         = "talosconfig-identity"
	flagPassphraseFile   = "talosconfig-passphrase-file"
	flagAskpass          = "talosconfig-askpass"
	flagRequireAll       = "require-all-contexts"

	envTalosConfig      = "TALOSCONFIG"
	envContext          = "TALOS_CONTEXT"
	envExtensions       = "EXTENSIONS"
	envAllowDestructive = "ALLOW_DESTRUCTIVE"
	envLogLevel         = "LOG_LEVEL"
	envLogFormat        = "LOG_FORMAT"
	envPort             = "PORT"
	envListen           = "LISTEN"
	envNoHostCheck      = "DISABLE_LOCALHOST_PROTECTION"
	envIdentity         = "TALOSCONFIG_IDENTITY"
	envPassphraseFile   = "TALOSCONFIG_PASSPHRASE_FILE"
	envAskpass          = "TALOSCONFIG_ASKPASS"
	envPassphrase       = "TALOSCONFIG_PASSPHRASE"
	envRequireAll       = "REQUIRE_ALL_CONTEXTS"
)

const (
	defaultExtensions       = "all"
	defaultAllowDestructive = false
	defaultLogLevel         = "info"
	defaultLogFormat        = "text"
	defaultPort             = 8080
	defaultListen           = "127.0.0.1"
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
	Listen           string
	NoHostCheck      bool
	Output           string

	// Unlock sources for encrypted talosconfig fields (docs/secrets.md §4).
	Identity           []string
	PassphraseFile     string
	Askpass            string
	RequireAllContexts bool

	// passphrase is TALOSCONFIG_PASSPHRASE, removed from the environment
	// when it is read.
	passphrase []byte
	// prompt allows the terminal prompt. Only the tools and config
	// subcommands set it: in mcp mode stdin is the transport.
	prompt bool

	// portErr is set when PORT is not a number. Only the server uses the
	// port, so it reports the error unless --port is given.
	portErr error
}

// DefaultFlags returns the default flags for the command,
// populated from environment variables where applicable.
func DefaultFlags() *Flags {
	port, portErr := envInt(envPort, defaultPort)

	return &Flags{
		TalosConfig:        withDefaultEnv(envTalosConfig, ""),
		Context:            withDefaultEnv(envContext, ""),
		Extensions:         withDefaultEnv(envExtensions, defaultExtensions),
		AllowDestructive:   withDefaultEnvBool(envAllowDestructive, defaultAllowDestructive),
		LogLevel:           withDefaultEnv(envLogLevel, defaultLogLevel),
		LogFormat:          withDefaultEnv(envLogFormat, defaultLogFormat),
		Port:               port,
		Listen:             withDefaultEnv(envListen, defaultListen),
		NoHostCheck:        withDefaultEnvBool(envNoHostCheck, false),
		Output:             defaultOutputFormat,
		Identity:           splitList(withDefaultEnv(envIdentity, "")),
		PassphraseFile:     withDefaultEnv(envPassphraseFile, ""),
		Askpass:            withDefaultEnv(envAskpass, ""),
		RequireAllContexts: withDefaultEnvBool(envRequireAll, false),
		passphrase:         secrets.TakeEnv(envPassphrase),
		portErr:            portErr,
	}
}

// AddPersistentFlags adds the global flags shared by every subcommand.
func (f *Flags) AddPersistentFlags(flags *pflag.FlagSet) {
	flags.StringVarP(&f.TalosConfig, flagTalosConfig, "", f.TalosConfig, "path to the talosconfig file, env "+envTalosConfig+" (default: ~/.talos/config)")
	flags.StringVarP(&f.Context, flagContext, "", f.Context, "restrict the server to a single talosconfig context, env "+envContext+" (default: all contexts)")
	flags.StringVarP(&f.Extensions, flagExtensions, "", f.Extensions, "comma-separated list of tool groups to enable: cluster, node, or 'all', env "+envExtensions)
	flags.BoolVarP(&f.AllowDestructive, flagAllowDestructive, "", f.AllowDestructive, "allow destructive operations, env "+envAllowDestructive+" (default: false)")
	flags.StringVarP(&f.LogLevel, flagLogLevel, "", f.LogLevel, "log level: debug, info, warn, error, env "+envLogLevel)
	flags.StringVarP(&f.LogFormat, flagLogFormat, "", f.LogFormat, "log output format: text, json, env "+envLogFormat)
	flags.StringSliceVarP(&f.Identity, flagIdentity, "", f.Identity, "age identity file or OpenSSH private key that decrypts talosconfig fields (repeatable), env "+envIdentity+" (comma-separated)")
	flags.StringVarP(&f.PassphraseFile, flagPassphraseFile, "", f.PassphraseFile,
		"file whose first line is the passphrase of encrypted talosconfig fields, env "+envPassphraseFile+"; or set the passphrase itself in "+envPassphrase)
	flags.StringVarP(&f.Askpass, flagAskpass, "", f.Askpass, "program that prints the passphrase of encrypted talosconfig fields on stdout, env "+envAskpass)
}

// AddServerFlags adds the flags for the "server" subcommand.
func (f *Flags) AddServerFlags(flags *pflag.FlagSet) {
	flags.IntVarP(&f.Port, flagPort, "", f.Port, "http listen port, env "+envPort)
	flags.StringVarP(&f.Listen, flagListen, "", f.Listen, "http listen address; use 0.0.0.0 to accept connections from other hosts, env "+envListen)
	flags.BoolVarP(&f.NoHostCheck, flagNoHostCheck, "", f.NoHostCheck,
		"accept requests on a loopback address with a non-localhost Host header, as sent by a sidecar proxy (disables DNS rebinding protection), env "+envNoHostCheck)
	flags.BoolVarP(&f.RequireAllContexts, flagRequireAll, "", f.RequireAllContexts, "fail to start when any talosconfig context is skipped, env "+envRequireAll+" (default: false)")
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
		TalosConfig:        path,
		Context:            f.Context,
		Unlock:             f.unlockOptions(),
		RequireAllContexts: f.RequireAllContexts,
		Port:               f.Port,
		Listen:             f.Listen,
		NoHostCheck:        f.NoHostCheck,
		Extensions:         f.Extensions,
		AllowDestructive:   f.AllowDestructive,
		LogLevel:           f.LogLevel,
		LogFormat:          f.LogFormat,
	}, nil
}

// haveTerminal reports whether a terminal prompt is possible. Tests replace it,
// so they do not depend on how go test was started.
var haveTerminal = secrets.HaveTerminal

// unlockOptions returns the unlock sources. The terminal prompt is offered
// only where f.prompt allows it, when no other passphrase source is set and
// there is a terminal.
func (f *Flags) unlockOptions() secrets.Options {
	opts := secrets.Options{
		IdentityFiles:  f.Identity,
		PassphraseFile: f.PassphraseFile,
		Askpass:        f.Askpass,
		Passphrase:     f.passphrase,
	}

	if f.prompt && opts.PassphraseFile == "" && opts.Askpass == "" && opts.Passphrase == nil && haveTerminal() {
		opts.Prompt = secrets.TerminalPrompt
	}

	return opts
}

// splitList splits a comma-separated value, dropping empty items.
func splitList(value string) []string {
	var items []string

	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}

	return items
}

// withDefaultEnv returns the environment value, or def when it is unset or empty.
func withDefaultEnv(key string, def string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return def
}

// envInt returns the environment value as a number, or def when it is unset
// or empty. A value that is not a number is an error, so a typo does not
// silently fall back to def.
func envInt(key string, def int) (int, error) {
	val, ok := os.LookupEnv(key)
	if !ok || val == "" {
		return def, nil
	}

	n, err := strconv.Atoi(val)
	if err != nil {
		return def, fmt.Errorf("invalid %s %q: must be a number", key, val)
	}

	return n, nil
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
