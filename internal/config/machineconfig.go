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
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	coreconfig "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/role"
)

// Errors for importing a machine config.
var (
	ErrInvalidMachineConfig = errors.New("invalid machine config")
	ErrNoTalosCAKey         = errors.New("machine config has no Talos API CA key: use a control plane machine config")
)

// ContextEntry is one talosconfig context, as written by SetContext. Key and
// Discovery.ClusterSecret hold base64 plaintext or an encrypted value.
type ContextEntry struct {
	Endpoints []string         `yaml:"endpoints"`
	Nodes     []string         `yaml:"nodes,omitempty"`
	CA        string           `yaml:"ca"`
	Crt       string           `yaml:"crt"`
	Key       string           `yaml:"key"`
	Discovery *DiscoveryConfig `yaml:"discovery,omitempty"`
}

// ImportOptions configures ImportMachineConfig.
type ImportOptions struct {
	// Roles are the Talos roles of the new client certificate.
	Roles role.Set
	// TTL is the lifetime of the new client certificate.
	TTL time.Duration
	// Endpoints overrides the endpoints taken from the cluster endpoint.
	Endpoints []string
	// Nodes is the optional default node list.
	Nodes []string
	// NoDiscovery leaves the discovery block out.
	NoDiscovery bool
	// Now is the start of the certificate validity.
	Now time.Time
}

// ImportResult is a context built from a machine config.
type ImportResult struct {
	// ClusterName is the cluster name from the machine config.
	ClusterName string
	Context     *ContextEntry
	// DiscoveryNote explains why the discovery block was left out, empty
	// when it is there or NoDiscovery was set.
	DiscoveryNote string
}

// ImportMachineConfig builds a talosconfig context from a control plane
// machine config: the Talos API CA, a new client certificate signed by the
// CA key, the endpoints, and the discovery block. The CA key is used only to
// sign the certificate and is not kept.
func ImportMachineConfig(data []byte, o ImportOptions) (*ImportResult, error) {
	if o.Roles.Empty() {
		return nil, errors.New("at least one role is required")
	}

	if o.TTL <= 0 {
		return nil, errors.New("certificate TTL must be positive")
	}

	cfg, err := configloader.NewFromBytes(data)
	if err != nil {
		return nil, errors.Join(ErrInvalidMachineConfig, err)
	}

	if cfg.Machine() == nil || cfg.Machine().Security() == nil {
		return nil, fmt.Errorf("%w: no v1alpha1 machine section", ErrInvalidMachineConfig)
	}

	ca := cfg.Machine().Security().IssuingCA()
	if ca == nil || len(ca.Crt) == 0 {
		return nil, fmt.Errorf("%w: no Talos API CA (machine.ca)", ErrInvalidMachineConfig)
	}

	if len(ca.Key) == 0 {
		return nil, ErrNoTalosCAKey
	}

	res := &ImportResult{}

	endpoints := o.Endpoints

	if k8s := cfg.K8sClusterConfig(); k8s != nil {
		res.ClusterName = k8s.ClusterName()

		if len(endpoints) == 0 && k8s.ClusterEndpoint() != nil && k8s.ClusterEndpoint().Hostname() != "" {
			endpoints = []string{k8s.ClusterEndpoint().Hostname()}
		}
	}

	if len(endpoints) == 0 {
		return nil, fmt.Errorf("%w: no cluster endpoint (cluster.controlPlane.endpoint); set the endpoints", ErrInvalidMachineConfig)
	}

	client, err := secrets.NewAdminCertificateAndKey(o.Now, ca, o.Roles, o.TTL)
	if err != nil {
		return nil, fmt.Errorf("generating client certificate: %w", err)
	}

	res.Context = &ContextEntry{
		Endpoints: endpoints,
		Nodes:     o.Nodes,
		CA:        base64.StdEncoding.EncodeToString(ca.Crt),
		Crt:       base64.StdEncoding.EncodeToString(client.Crt),
		Key:       base64.StdEncoding.EncodeToString(client.Key),
	}

	clear(client.Key)

	if !o.NoDiscovery {
		res.Context.Discovery, res.DiscoveryNote = importDiscovery(cfg)
	}

	return res, nil
}

// importDiscovery returns the discovery block of a machine config, or the
// reason there is none. The endpoint is kept as written in the machine
// config, so an http:// endpoint stays insecure.
func importDiscovery(cfg coreconfig.Config) (*DiscoveryConfig, string) {
	identity := cfg.DiscoveryIdentityConfig()
	if identity == nil || identity.ClusterID() == "" || identity.ClusterSecret() == "" {
		return nil, "the machine config has no cluster id and secret"
	}

	services := cfg.DiscoveryServiceConfigs()
	if len(services) == 0 || services[0].Endpoint() == nil {
		return nil, "the discovery service is disabled in the machine config"
	}

	d := &DiscoveryConfig{
		Endpoint:      services[0].Endpoint().String(),
		ClusterID:     identity.ClusterID(),
		ClusterSecret: identity.ClusterSecret(),
	}

	check := *d
	if err := check.validate(); err != nil {
		return nil, err.Error()
	}

	return d, ""
}
