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

// Package talos holds the per-context Talos clients, credential roles and
// node resolution of the talos-mcp server.
package talos

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/siderolabs/talos/pkg/machinery/role"
)

// Role is the effective role of a context's credential.
// Roles are ordered: a higher role allows everything a lower one does.
type Role int

// Effective roles.
const (
	RoleNone Role = iota
	RoleReader
	RoleOperator
)

const roleNone = "none"

// CertExpiryWarning is how far ahead an expiring client certificate is reported.
const CertExpiryWarning = 7 * 24 * time.Hour

// Errors for reading the client certificate.
var (
	ErrInvalidCertificate = errors.New("invalid client certificate")
	ErrNoKnownRole        = errors.New("client certificate has no os:reader, os:operator or os:admin role")
)

// String returns the role name used in tool descriptions and errors.
func (r Role) String() string {
	switch r {
	case RoleReader:
		return "reader"
	case RoleOperator:
		return "operator"
	case RoleNone:
		return roleNone
	default:
		return roleNone
	}
}

// Allows reports whether the role meets the minimum role required.
func (r Role) Allows(required Role) bool {
	return r != RoleNone && r >= required
}

// Credential is what the server learns from a context's client certificate,
// without any network call.
type Credential struct {
	// Role is the effective role. os:admin maps to RoleOperator.
	Role Role
	// Admin reports that the certificate carries os:admin.
	Admin bool
	// Roles lists the certificate's Subject O values, sorted.
	Roles []string
	// NotAfter is the certificate expiry time.
	NotAfter time.Time
}

// Expired reports whether the certificate has expired at now.
func (c *Credential) Expired(now time.Time) bool {
	return !now.Before(c.NotAfter)
}

// ExpiresSoon reports whether the certificate is expired or expires within
// CertExpiryWarning of now.
func (c *Credential) ExpiresSoon(now time.Time) bool {
	return c.NotAfter.Sub(now) < CertExpiryWarning
}

// ParseCredential reads the role and expiry from a talosconfig `crt` value
// (base64-encoded PEM certificate).
func ParseCredential(crt string) (*Credential, error) {
	data, err := base64.StdEncoding.DecodeString(crt)
	if err != nil {
		return nil, fmt.Errorf("%w: crt is not valid base64", ErrInvalidCertificate)
	}

	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%w: crt is not a PEM certificate", ErrInvalidCertificate)
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCertificate, err)
	}

	roles := slices.Clone(cert.Subject.Organization)
	slices.Sort(roles)

	effective, admin := roleFromOrganizations(roles)

	return &Credential{
		Role:     effective,
		Admin:    admin,
		Roles:    roles,
		NotAfter: cert.NotAfter,
	}, nil
}

// roleFromOrganizations maps Talos roles to the effective role. The highest
// known role wins, unknown roles are ignored, and os:admin counts as operator.
func roleFromOrganizations(orgs []string) (Role, bool) {
	switch {
	case slices.Contains(orgs, string(role.Admin)):
		return RoleOperator, true
	case slices.Contains(orgs, string(role.Operator)):
		return RoleOperator, false
	case slices.Contains(orgs, string(role.Reader)):
		return RoleReader, false
	default:
		return RoleNone, false
	}
}

// CheckCredentials parses the client certificate of every context in tc. A
// context whose certificate can't be parsed or has no known role is removed
// from tc with a warning. os:admin credentials and certificates that are
// expired or expire soon are kept, with a warning.
func CheckCredentials(tc *config.TalosConfig, now time.Time) (map[string]*Credential, error) {
	creds := make(map[string]*Credential, len(tc.Config.Contexts))

	for _, name := range tc.Contexts() {
		cred, err := ParseCredential(tc.Config.Contexts[name].Crt)
		if err == nil && cred.Role == RoleNone {
			err = fmt.Errorf("%w (roles: %s)", ErrNoKnownRole, rolesList(cred.Roles))
		}

		if err != nil {
			if rerr := tc.Remove(name, err.Error()); rerr != nil {
				return nil, rerr
			}

			continue
		}

		if cred.Admin {
			tc.Warnf("context %q uses an os:admin credential; it gets the operator tool set, use a dedicated os:operator or os:reader credential", name)
		}

		switch {
		case cred.Expired(now):
			tc.Warnf("context %q: client certificate expired at %s", name, cred.NotAfter.UTC().Format(time.RFC3339))
		case cred.ExpiresSoon(now):
			tc.Warnf("context %q: client certificate expires at %s", name, cred.NotAfter.UTC().Format(time.RFC3339))
		}

		creds[name] = cred
	}

	return creds, nil
}

func rolesList(roles []string) string {
	if len(roles) == 0 {
		return roleNone
	}

	return strings.Join(roles, ", ")
}
