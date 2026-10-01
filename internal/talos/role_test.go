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

package talos_test

import (
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sergelogvinov/talos-mcp/internal/config"
	"github.com/sergelogvinov/talos-mcp/internal/talos"
	"github.com/sergelogvinov/talos-mcp/internal/talos/talostest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestParseCredential(t *testing.T) {
	valid := now.Add(365 * 24 * time.Hour)

	for _, tt := range []struct {
		name          string
		orgs          []string
		expectedRole  talos.Role
		expectedAdmin bool
	}{
		{name: "reader", orgs: []string{"os:reader"}, expectedRole: talos.RoleReader},
		{name: "operator", orgs: []string{"os:operator"}, expectedRole: talos.RoleOperator},
		{name: "admin maps to operator", orgs: []string{"os:admin"}, expectedRole: talos.RoleOperator, expectedAdmin: true},
		{name: "highest of several roles wins", orgs: []string{"os:reader", "os:operator"}, expectedRole: talos.RoleOperator},
		{name: "unknown roles are ignored", orgs: []string{"os:etcd:backup", "custom", "os:reader"}, expectedRole: talos.RoleReader},
		{name: "no known role", orgs: []string{"os:etcd:backup"}, expectedRole: talos.RoleNone},
		{name: "no roles at all", orgs: nil, expectedRole: talos.RoleNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cred, err := talos.ParseCredential(talostest.Crt(t, valid, tt.orgs...))
			require.NoError(t, err)

			assert.Equal(t, tt.expectedRole, cred.Role)
			assert.Equal(t, tt.expectedAdmin, cred.Admin)
			assert.Equal(t, valid, cred.NotAfter)
		})
	}
}

func TestParseCredentialInvalid(t *testing.T) {
	for _, tt := range []struct {
		name string
		crt  string
	}{
		{name: "not base64", crt: "not base64!"},
		{name: "not PEM", crt: base64.StdEncoding.EncodeToString([]byte("plain text"))},
		{name: "wrong PEM type", crt: base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1}}))},
		{name: "bad DER", crt: base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}}))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := talos.ParseCredential(tt.crt)
			require.ErrorIs(t, err, talos.ErrInvalidCertificate)
		})
	}
}

func TestRoleAllows(t *testing.T) {
	assert.True(t, talos.RoleReader.Allows(talos.RoleReader))
	assert.False(t, talos.RoleReader.Allows(talos.RoleOperator))
	assert.True(t, talos.RoleOperator.Allows(talos.RoleReader))
	assert.True(t, talos.RoleOperator.Allows(talos.RoleOperator))
	assert.False(t, talos.RoleNone.Allows(talos.RoleNone))
	assert.Equal(t, "reader", talos.RoleReader.String())
	assert.Equal(t, "operator", talos.RoleOperator.String())
}

func TestCredentialExpiry(t *testing.T) {
	for _, tt := range []struct {
		name         string
		notAfter     time.Time
		expectedExp  bool
		expectedSoon bool
	}{
		{name: "valid", notAfter: now.Add(30 * 24 * time.Hour)},
		{name: "expires within 7 days", notAfter: now.Add(6 * 24 * time.Hour), expectedSoon: true},
		{name: "expired", notAfter: now.Add(-time.Hour), expectedExp: true, expectedSoon: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cred := &talos.Credential{NotAfter: tt.notAfter}
			assert.Equal(t, tt.expectedExp, cred.Expired(now))
			assert.Equal(t, tt.expectedSoon, cred.ExpiresSoon(now))
		})
	}
}

type fixtureContext struct {
	name string
	crt  string
}

// newTalosConfig builds a validated talosconfig with the given contexts. The
// first context is the current one.
func newTalosConfig(t *testing.T, filter string, contexts ...fixtureContext) *config.TalosConfig {
	t.Helper()

	var b strings.Builder

	fmt.Fprintf(&b, "context: %s\ncontexts:\n", contexts[0].name)

	for _, c := range contexts {
		fmt.Fprintf(&b, "  %s:\n    endpoints: [10.0.0.1]\n    ca: Y2E=\n    crt: %s\n    key: a2V5\n", c.name, c.crt)
	}

	tc, err := config.ParseTalosConfig([]byte(b.String()), filter)
	require.NoError(t, err)

	return tc
}

func TestCheckCredentials(t *testing.T) {
	year := now.Add(365 * 24 * time.Hour)

	tc := newTalosConfig(t, "",
		fixtureContext{name: "backup", crt: talostest.Crt(t, year, "os:etcd:backup")},
		fixtureContext{name: "admin", crt: talostest.Crt(t, year, "os:admin")},
		fixtureContext{name: "expired", crt: talostest.Crt(t, now.Add(-time.Hour), "os:reader")},
		fixtureContext{name: "garbage", crt: "Y3J0"},
		fixtureContext{name: "reader", crt: talostest.Crt(t, year, "os:reader")},
		fixtureContext{name: "soon", crt: talostest.Crt(t, now.Add(48*time.Hour), "os:operator")},
	)

	creds, err := talos.CheckCredentials(tc, now)
	require.NoError(t, err)

	assert.Equal(t, []string{"admin", "expired", "reader", "soon"}, tc.Contexts())
	assert.Len(t, creds, 4)
	assert.Equal(t, talos.RoleOperator, creds["admin"].Role)
	assert.Equal(t, talos.RoleReader, creds["reader"].Role)
	assert.Equal(t, talos.RoleOperator, creds["soon"].Role)

	// The current context "backup" was skipped, so the first remaining one is used.
	assert.Equal(t, "admin", tc.Current)
	assert.Equal(t, "admin", tc.Config.Context)

	assert.Equal(t, []string{
		`context "admin" uses an os:admin credential; it gets the operator tool set, use a dedicated os:operator or os:reader credential`,
		`context "backup" skipped: client certificate has no os:reader, os:operator or os:admin role (roles: os:etcd:backup)`,
		`current context "backup" is not usable, using "admin"`,
		`context "expired": client certificate expired at 2026-10-01T11:00:00Z`,
		`context "garbage" skipped: invalid client certificate: crt is not a PEM certificate`,
		`context "soon": client certificate expires at 2026-10-03T12:00:00Z`,
	}, tc.Warnings)
}

func TestCheckCredentialsFilteredContextWithoutRole(t *testing.T) {
	year := now.Add(365 * 24 * time.Hour)

	tc := newTalosConfig(t, "backup",
		fixtureContext{name: "backup", crt: talostest.Crt(t, year, "os:etcd:backup")},
		fixtureContext{name: "reader", crt: talostest.Crt(t, year, "os:reader")},
	)

	_, err := talos.CheckCredentials(tc, now)
	require.EqualError(t, err, `context "backup": client certificate has no os:reader, os:operator or os:admin role (roles: os:etcd:backup)`)
}

func TestCheckCredentialsNoUsableContext(t *testing.T) {
	tc := newTalosConfig(t, "",
		fixtureContext{name: "backup", crt: talostest.Crt(t, now.Add(time.Hour), "os:etcd:backup")},
	)

	_, err := talos.CheckCredentials(tc, now)
	require.ErrorIs(t, err, config.ErrNoContexts)
}
