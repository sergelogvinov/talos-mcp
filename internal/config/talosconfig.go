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

package config

import (
	"crypto/aes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/sergelogvinov/talos-mcp/internal/secrets"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	yaml "go.yaml.in/yaml/v3"
)

// DefaultDiscoveryEndpoint is the public Talos discovery service.
const DefaultDiscoveryEndpoint = "discovery.talos.dev:443"

// Errors for reading the talosconfig.
var (
	ErrInvalidTalosConfig = errors.New("invalid talosconfig")
	ErrNoContexts         = errors.New("talosconfig has no usable contexts")
	ErrUnknownContext     = errors.New("unknown context")
	ErrInvalidDiscovery   = errors.New("invalid discovery block")
	ErrNoUnlockSource     = errors.New("talosconfig has encrypted fields; set --talosconfig-identity, --talosconfig-passphrase-file, --talosconfig-askpass or TALOSCONFIG_PASSPHRASE")
)

// Secret field names, as in the talosconfig.
const (
	FieldKey           = "key"
	FieldClusterSecret = "cluster_secret"
)

// DiscoveryConfig is the optional per-context `discovery` block (design §2.2).
type DiscoveryConfig struct {
	// Endpoint is host:port after validation. It may be written as host,
	// host:port, or a URL as in the machine config
	// (https://discovery.talos.dev/); an http:// URL sets Insecure.
	Endpoint      string `yaml:"endpoint,omitempty"`
	ClusterID     string `yaml:"cluster_id"`
	ClusterSecret string `yaml:"cluster_secret"`
	// Insecure is set for an http:// endpoint: no TLS, for self-hosted
	// services only.
	Insecure bool `yaml:"-"`
}

// TalosConfig is a loaded and validated talosconfig.
type TalosConfig struct {
	// Config holds only the usable contexts, and its Context field is set to
	// Current, so it can be passed to client.WithConfig as is.
	Config *clientconfig.Config
	// Current is the default context, used when a tool omits `cluster`.
	Current string
	// Discovery holds the valid discovery blocks, keyed by context name.
	Discovery map[string]*DiscoveryConfig
	// Warnings lists skipped contexts and disabled discovery blocks, for the
	// caller to log.
	Warnings []string
	// Skipped lists the contexts dropped as unusable, sorted by when they
	// were dropped.
	Skipped []string
	// PlaintextKeys lists the usable contexts whose key is not encrypted.
	PlaintextKeys []string

	// filter is the --context value, empty when all contexts are used.
	filter string
}

// discoveryFile picks the `discovery` blocks out of a talosconfig. clientconfig
// ignores unknown keys, so the same bytes are decoded twice.
type discoveryFile struct {
	Contexts map[string]struct {
		Discovery *DiscoveryConfig `yaml:"discovery"`
	} `yaml:"contexts"`
}

// ParseOption configures ParseTalosConfig.
type ParseOption func(*parseOptions)

type parseOptions struct {
	unlock secrets.Options
}

// WithUnlock sets the sources that unlock encrypted fields (docs/secrets.md
// §4). Without it, a talosconfig with encrypted fields is an error.
func WithUnlock(opts secrets.Options) ParseOption {
	return func(o *parseOptions) {
		o.unlock = opts
	}
}

// LoadTalosConfig reads the talosconfig at path and validates it. The file is
// read directly instead of with clientconfig.Open, which creates a missing file.
func LoadTalosConfig(path, contextFilter string, opts ...ParseOption) (*TalosConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading talosconfig: %w", err)
	}

	return ParseTalosConfig(data, contextFilter, opts...)
}

// ParseTalosConfig parses and validates talosconfig bytes (design §2.1, §2.2).
// Encrypted fields are decrypted in memory (docs/secrets.md §5), and the
// unlock material is dropped before it returns. Unusable contexts, including
// ones that do not decrypt, are dropped with a warning. contextFilter, when
// set, restricts the result to that one context.
func ParseTalosConfig(data []byte, contextFilter string, opts ...ParseOption) (*TalosConfig, error) {
	var o parseOptions
	for _, opt := range opts {
		opt(&o)
	}

	cfg, df, err := parseFile(data)
	if err != nil {
		return nil, err
	}

	if len(cfg.Contexts) == 0 {
		return nil, ErrNoContexts
	}

	if contextFilter != "" {
		if _, ok := cfg.Contexts[contextFilter]; !ok {
			return nil, fmt.Errorf("%w %q: talosconfig has %s", ErrUnknownContext, contextFilter, strings.Join(sortedKeys(cfg.Contexts), ", "))
		}
	}

	tc := &TalosConfig{
		Discovery: map[string]*DiscoveryConfig{},
		filter:    contextFilter,
	}

	names := contextNames(cfg, contextFilter)

	u := secrets.NewUnlocker(o.unlock)
	defer u.Close()

	if err := checkUnlock(cfg, df, names, u); err != nil {
		return nil, err
	}

	usable := map[string]*clientconfig.Context{}

	for _, name := range names {
		plainKey := cfg.Contexts[name] != nil && !secrets.IsEncrypted(cfg.Contexts[name].Key)

		err := validateContext(cfg.Contexts[name])
		if err == nil {
			err = decryptContext(cfg.Contexts[name], df.Contexts[name].Discovery, u)
		}

		if err != nil {
			if name == contextFilter {
				return nil, fmt.Errorf("context %q: %w", name, err)
			}

			tc.warnf("context %q skipped: %v", name, err)
			tc.Skipped = append(tc.Skipped, name)

			continue
		}

		usable[name] = cfg.Contexts[name]

		if plainKey {
			tc.PlaintextKeys = append(tc.PlaintextKeys, name)
		}

		if d := df.Contexts[name].Discovery; d != nil {
			if err := d.validate(); err != nil {
				tc.warnf("context %q: discovery disabled: %v", name, err)

				continue
			}

			tc.Discovery[name] = d
		}
	}

	if len(usable) == 0 {
		return nil, ErrNoContexts
	}

	tc.Current = tc.pickCurrent(cfg.Context, contextFilter, usable)

	cfg.Contexts = usable
	cfg.Context = tc.Current
	tc.Config = cfg

	return tc, nil
}

// Remove drops a context that a later check found unusable, such as one
// without a known role (design §2.3), and records reason as a warning. If the
// context was the current one, the first remaining context becomes current.
// Removing the --context context, or the last context, is an error.
func (t *TalosConfig) Remove(name, reason string) error {
	if _, ok := t.Config.Contexts[name]; !ok {
		return nil
	}

	if name == t.filter {
		return fmt.Errorf("context %q: %s", name, reason)
	}

	delete(t.Config.Contexts, name)
	delete(t.Discovery, name)
	t.PlaintextKeys = slices.DeleteFunc(t.PlaintextKeys, func(n string) bool { return n == name })
	t.warnf("context %q skipped: %s", name, reason)
	t.Skipped = append(t.Skipped, name)

	if len(t.Config.Contexts) == 0 {
		return ErrNoContexts
	}

	if name == t.Current {
		t.Current = t.pickCurrent(name, "", t.Config.Contexts)
		t.Config.Context = t.Current
	}

	return nil
}

// Warnf records a startup warning for the caller to log.
func (t *TalosConfig) Warnf(format string, args ...any) {
	t.warnf(format, args...)
}

// Contexts returns the usable context names, sorted.
func (t *TalosConfig) Contexts() []string {
	return sortedKeys(t.Config.Contexts)
}

// pickCurrent returns the --context filter, else the talosconfig's current
// context when it is usable, else the first usable context with a warning.
func (t *TalosConfig) pickCurrent(current, contextFilter string, usable map[string]*clientconfig.Context) string {
	if contextFilter != "" {
		return contextFilter
	}

	if _, ok := usable[current]; ok {
		return current
	}

	fallback := sortedKeys(usable)[0]

	if current == "" {
		t.warnf("talosconfig has no current context, using %q", fallback)
	} else {
		t.warnf("current context %q is not usable, using %q", current, fallback)
	}

	return fallback
}

func (t *TalosConfig) warnf(format string, args ...any) {
	t.Warnings = append(t.Warnings, fmt.Sprintf(format, args...))
}

// NewUnlocker returns an Unlocker for the encrypted fields of the contexts in
// data: all of them, or only contextFilter. Like ParseTalosConfig, it fails
// when there are encrypted fields but no unlock source is set, or an
// identity file cannot be read. The caller closes it.
func NewUnlocker(data []byte, contextFilter string, opts secrets.Options) (*secrets.Unlocker, error) {
	cfg, df, err := parseFile(data)
	if err != nil {
		return nil, err
	}

	u := secrets.NewUnlocker(opts)

	if err := checkUnlock(cfg, df, contextNames(cfg, contextFilter), u); err != nil {
		u.Close()

		return nil, err
	}

	return u, nil
}

// ParseClientConfig decodes a talosconfig with clientconfig. A context
// written with no fields (`old:`) is decoded as an empty context instead of
// nil, which clientconfig.FromBytes dereferences and panics on.
func ParseClientConfig(data []byte) (*clientconfig.Config, error) {
	data, err := fillNullContexts(data)
	if err != nil {
		return nil, errors.Join(ErrInvalidTalosConfig, err)
	}

	cfg, err := clientconfig.FromBytes(data)
	if err != nil {
		return nil, errors.Join(ErrInvalidTalosConfig, err)
	}

	return cfg, nil
}

// fillNullContexts replaces null context values with empty mappings. data
// is returned unchanged when there are none.
func fillNullContexts(data []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}

	if len(doc.Content) == 0 {
		return data, nil
	}

	contexts := mappingValue(doc.Content[0], "contexts")
	if contexts == nil || contexts.Kind != yaml.MappingNode {
		return data, nil
	}

	changed := false

	for i := 1; i < len(contexts.Content); i += 2 {
		if c := contexts.Content[i]; c.Kind == yaml.ScalarNode && c.Tag == "!!null" {
			contexts.Content[i] = &yaml.Node{Kind: yaml.MappingNode, Tag: yamlMapTag}
			changed = true
		}
	}

	if !changed {
		return data, nil
	}

	return yaml.Marshal(&doc)
}

// parseFile decodes a talosconfig and its discovery blocks.
func parseFile(data []byte) (*clientconfig.Config, *discoveryFile, error) {
	cfg, err := ParseClientConfig(data)
	if err != nil {
		return nil, nil, err
	}

	var df discoveryFile
	if err := yaml.Unmarshal(data, &df); err != nil {
		return nil, nil, errors.Join(ErrInvalidTalosConfig, err)
	}

	return cfg, &df, nil
}

// contextNames returns the sorted context names, or only contextFilter.
func contextNames(cfg *clientconfig.Config, contextFilter string) []string {
	var names []string

	for _, name := range sortedKeys(cfg.Contexts) {
		if contextFilter == "" || name == contextFilter {
			names = append(names, name)
		}
	}

	return names
}

// checkUnlock fails when the contexts in names have encrypted fields but no
// unlock source is set, or an identity file cannot be read. Both would make
// every encrypted context fail, so they are reported once.
func checkUnlock(cfg *clientconfig.Config, df *discoveryFile, names []string, u *secrets.Unlocker) error {
	encrypted := false

	for _, name := range names {
		if c := cfg.Contexts[name]; c != nil && secrets.IsEncrypted(c.Key) {
			encrypted = true
		}

		if d := df.Contexts[name].Discovery; d != nil && secrets.IsEncrypted(d.ClusterSecret) {
			encrypted = true
		}
	}

	if !encrypted {
		return nil
	}

	if !u.Configured() {
		return ErrNoUnlockSource
	}

	return u.Load()
}

// decryptContext replaces the encrypted key and cluster_secret of a context
// with their plaintext, base64-encoded like a plain talosconfig value.
func decryptContext(c *clientconfig.Context, d *DiscoveryConfig, u *secrets.Unlocker) error {
	if err := decryptField(&c.Key, FieldKey, u); err != nil {
		return err
	}

	if d != nil {
		return decryptField(&d.ClusterSecret, FieldClusterSecret, u)
	}

	return nil
}

func decryptField(value *string, field string, u *secrets.Unlocker) error {
	if !secrets.IsEncrypted(*value) {
		return nil
	}

	plain, err := secrets.Decrypt(*value, u)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}

	*value = base64.StdEncoding.EncodeToString(plain)
	clear(plain)

	return nil
}

// validateContext checks the fields the server needs to reach apid.
func validateContext(c *clientconfig.Context) error {
	if c == nil {
		return errors.New("empty context")
	}

	if c.Auth.SideroV1 != nil {
		return errors.New("omni (auth.siderov1) contexts are not supported")
	}

	if len(c.Endpoints) == 0 {
		return errors.New("no endpoints")
	}

	var missing []string

	for field, value := range map[string]string{"ca": c.CA, "crt": c.Crt, "key": c.Key} {
		if value == "" {
			missing = append(missing, field)
		}
	}

	if len(missing) > 0 {
		slices.Sort(missing)

		return fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}

	return nil
}

// validate checks a discovery block and fills in the default endpoint.
func (d *DiscoveryConfig) validate() error {
	if d.ClusterID == "" {
		return fmt.Errorf("%w: cluster_id is required", ErrInvalidDiscovery)
	}

	if d.ClusterSecret == "" {
		return fmt.Errorf("%w: cluster_secret is required", ErrInvalidDiscovery)
	}

	key, err := base64.StdEncoding.DecodeString(d.ClusterSecret)
	if err != nil {
		return fmt.Errorf("%w: cluster_secret is not valid base64", ErrInvalidDiscovery)
	}

	if _, err := aes.NewCipher(key); err != nil {
		return fmt.Errorf("%w: cluster_secret is not a valid AES key (%d bytes)", ErrInvalidDiscovery, len(key))
	}

	endpoint, insecure, err := normalizeDiscoveryEndpoint(d.Endpoint)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidDiscovery, err)
	}

	d.Endpoint, d.Insecure = endpoint, insecure

	return nil
}

// normalizeDiscoveryEndpoint turns the endpoint forms accepted in the
// discovery block into host:port. The default port is 443 for TLS and 80
// for http://.
func normalizeDiscoveryEndpoint(endpoint string) (string, bool, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return DefaultDiscoveryEndpoint, false, nil
	}

	insecure := false
	port := "443"

	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil {
			return "", false, fmt.Errorf("endpoint %q: %v", endpoint, err)
		}

		switch u.Scheme {
		case "https", "grpcs":
		case "http", "grpc":
			insecure, port = true, "80"
		default:
			return "", false, fmt.Errorf("endpoint %q: unsupported scheme %q, use https:// or host:port", endpoint, u.Scheme)
		}

		if u.Path != "" && u.Path != "/" {
			return "", false, fmt.Errorf("endpoint %q: a path is not supported", endpoint)
		}

		endpoint = u.Host
	}

	host, p, err := net.SplitHostPort(endpoint)
	if err != nil {
		// No port: a bare host or a bracketed IPv6 address.
		host, p = strings.Trim(endpoint, "[]"), port
	}

	if host == "" {
		return "", false, fmt.Errorf("endpoint %q: missing host", endpoint)
	}

	return net.JoinHostPort(host, p), insecure, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}
