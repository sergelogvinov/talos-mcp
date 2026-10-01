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

package talos

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sergelogvinov/talos-mcp/internal/config"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

// Errors returned by the pool.
var (
	ErrUnknownCluster = errors.New("unknown cluster")
	ErrRoleDenied     = errors.New("credential role denied")
	ErrPoolClosed     = errors.New("pool is closed")
)

// Pool holds the per-context Talos clients and what the server knows about
// each context offline: its role, credential and discovery block (design §6).
type Pool struct {
	cfg       *clientconfig.Config
	contexts  []string // sorted, after --context filter & validation
	current   string   // default context
	creds     map[string]*Credential
	discovery map[string]*config.DiscoveryConfig
	warnings  []string

	newClient ClientFactory

	mu      sync.Mutex
	closed  bool
	clients map[string]Client // lazily created, one per context
}

// Option configures a Pool.
type Option func(*options)

type options struct {
	newClient ClientFactory
	now       func() time.Time
}

// WithClientFactory replaces the Talos client constructor, for tests.
func WithClientFactory(f ClientFactory) Option {
	return func(o *options) {
		o.newClient = f
	}
}

// WithClock replaces the clock used for certificate expiry checks, for tests.
func WithClock(now func() time.Time) Option {
	return func(o *options) {
		o.now = now
	}
}

// LoadPool loads the talosconfig named in cfg and builds the pool from it.
func LoadPool(cfg *config.Config, opts ...Option) (*Pool, error) {
	tc, err := config.LoadTalosConfig(cfg.TalosConfig, cfg.Context)
	if err != nil {
		return nil, err
	}

	return NewPool(tc, opts...)
}

// NewPool checks the credential of every context in tc (design §2.3) and
// builds the pool. Contexts without a known role are dropped. No network
// call is made; clients are created on first use.
func NewPool(tc *config.TalosConfig, opts ...Option) (*Pool, error) {
	o := options{
		newClient: NewTalosClient,
		now:       time.Now,
	}

	for _, opt := range opts {
		opt(&o)
	}

	creds, err := CheckCredentials(tc, o.now())
	if err != nil {
		return nil, err
	}

	return &Pool{
		cfg:       tc.Config,
		contexts:  tc.Contexts(),
		current:   tc.Current,
		creds:     creds,
		discovery: tc.Discovery,
		warnings:  slices.Clone(tc.Warnings),
		newClient: o.newClient,
		clients:   map[string]Client{},
	}, nil
}

// Warnings returns the startup warnings: skipped contexts, disabled discovery
// blocks, admin credentials and expiring certificates. The caller logs them.
func (p *Pool) Warnings() []string {
	return slices.Clone(p.warnings)
}

// List returns the cluster names, sorted.
func (p *Pool) List() []string {
	return slices.Clone(p.contexts)
}

// Current returns the default cluster.
func (p *Pool) Current() string {
	return p.current
}

// Resolve maps a tool's `cluster` argument to a cluster name: "" is the
// current cluster, and an unknown name is an error listing the valid names.
func (p *Pool) Resolve(cluster string) (string, error) {
	if cluster == "" {
		return p.current, nil
	}

	if _, ok := p.creds[cluster]; !ok {
		return "", fmt.Errorf("%w %q: valid clusters are %s", ErrUnknownCluster, cluster, strings.Join(p.contexts, ", "))
	}

	return cluster, nil
}

// ContextInfo returns the talosconfig endpoints and default nodes of a cluster.
func (p *Pool) ContextInfo(cluster string) (endpoints, nodes []string, err error) {
	name, err := p.Resolve(cluster)
	if err != nil {
		return nil, nil, err
	}

	c := p.cfg.Contexts[name]

	return slices.Clone(c.Endpoints), slices.Clone(c.Nodes), nil
}

// Role returns the effective role of a cluster's credential, RoleNone for an
// unknown cluster.
func (p *Pool) Role(cluster string) Role {
	name, err := p.Resolve(cluster)
	if err != nil {
		return RoleNone
	}

	return p.creds[name].Role
}

// Credential returns what the server read from a cluster's client
// certificate, or nil for an unknown cluster.
func (p *Pool) Credential(cluster string) *Credential {
	name, err := p.Resolve(cluster)
	if err != nil {
		return nil
	}

	cred := *p.creds[name]
	cred.Roles = slices.Clone(cred.Roles)

	return &cred
}

// Discovery returns a cluster's discovery block, or nil when it has none.
func (p *Pool) Discovery(cluster string) *config.DiscoveryConfig {
	name, err := p.Resolve(cluster)
	if err != nil {
		return nil
	}

	d, ok := p.discovery[name]
	if !ok {
		return nil
	}

	cp := *d

	return &cp
}

// ClustersWithRole returns the clusters whose role meets required, sorted.
// It drives tool registration (design §9).
func (p *Pool) ClustersWithRole(required Role) []string {
	var names []string

	for _, name := range p.contexts {
		if p.creds[name].Role.Allows(required) {
			names = append(names, name)
		}
	}

	return names
}

// ClustersWithDiscovery returns the clusters with a valid discovery block,
// sorted. It drives talos_clusters_members registration (design §8.4).
func (p *Pool) ClustersWithDiscovery() []string {
	var names []string

	for _, name := range p.contexts {
		if _, ok := p.discovery[name]; ok {
			names = append(names, name)
		}
	}

	return names
}

// Require is the per-call role check (design §9). It fails for an unknown
// cluster, or when the cluster's credential is below required.
func (p *Pool) Require(cluster string, required Role) error {
	name, err := p.Resolve(cluster)
	if err != nil {
		return err
	}

	cred := p.creds[name]
	if cred.Role.Allows(required) {
		return nil
	}

	return fmt.Errorf("%w: cluster %s uses an %s credential; os:%s is required", ErrRoleDenied, name, cred.talosRole(), required)
}

// Client returns the Talos client of a cluster, creating it on first use.
// A failed creation is not cached, so the next call retries.
func (p *Pool) Client(ctx context.Context, cluster string) (Client, error) {
	name, err := p.Resolve(cluster)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, ErrPoolClosed
	}

	if c, ok := p.clients[name]; ok {
		return c, nil
	}

	c, err := p.newClient(ctx, p.cfg, name)
	if err != nil {
		return nil, fmt.Errorf("cluster %s: creating Talos client: %w", name, err)
	}

	p.clients[name] = c

	return c, nil
}

// Close closes every client. The pool can't hand out clients afterwards.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true

	var errs []error

	for _, name := range slices.Sorted(maps.Keys(p.clients)) {
		if err := p.clients[name].Close(); err != nil {
			errs = append(errs, fmt.Errorf("cluster %s: %w", name, err))
		}
	}

	clear(p.clients)

	return errors.Join(errs...)
}

// talosRole is the strongest Talos role name of the credential, as used in
// error messages.
func (c *Credential) talosRole() string {
	if c.Admin {
		return "os:admin"
	}

	return "os:" + c.Role.String()
}
