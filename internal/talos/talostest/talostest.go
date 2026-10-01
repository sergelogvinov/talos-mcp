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

// Package talostest builds talosconfig fixtures for tests: client
// certificates with Talos roles, generated in memory, and talosconfig YAML.
package talostest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

// Secret is a valid discovery cluster_secret (base64 of 32 bytes).
const Secret = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

// CrtKey returns a self-signed base64 PEM client certificate with the given
// Subject O values (Talos roles) and expiry, and its base64 PEM private key,
// as found in talosconfig `crt` and `key` fields.
func CrtKey(t testing.TB, notAfter time.Time, orgs ...string) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: orgs},
		NotBefore:    notAfter.Add(-365 * 24 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

// Crt returns only the certificate of CrtKey.
func Crt(t testing.TB, notAfter time.Time, orgs ...string) string {
	t.Helper()

	crt, _ := CrtKey(t, notAfter, orgs...)

	return crt
}

// Context describes one talosconfig context of a fixture.
type Context struct {
	Name string
	// Crt is the `crt` value. Leave it empty and set Roles to generate one.
	Crt string
	// Roles are the Talos roles of a generated certificate.
	Roles []string
	// NotAfter is the expiry of a generated certificate; zero means a year
	// after now.
	NotAfter time.Time
	// Endpoints defaults to "<name>.example.com".
	Endpoints []string
	Nodes     []string
	// Discovery adds a valid discovery block with this cluster_id.
	Discovery string
}

// TalosConfig renders a talosconfig YAML with the given contexts. The first
// context is the current one.
func TalosConfig(t testing.TB, now time.Time, contexts ...Context) string {
	t.Helper()

	var b strings.Builder

	fmt.Fprintf(&b, "context: %s\ncontexts:\n", contexts[0].Name)

	for _, c := range contexts {
		crt := c.Crt
		if crt == "" {
			notAfter := c.NotAfter
			if notAfter.IsZero() {
				notAfter = now.Add(365 * 24 * time.Hour)
			}

			crt = Crt(t, notAfter, c.Roles...)
		}

		endpoints := c.Endpoints
		if len(endpoints) == 0 {
			endpoints = []string{c.Name + ".example.com"}
		}

		fmt.Fprintf(&b, "  %s:\n    endpoints: [%s]\n", c.Name, quoteList(endpoints))

		if len(c.Nodes) > 0 {
			fmt.Fprintf(&b, "    nodes: [%s]\n", quoteList(c.Nodes))
		}

		fmt.Fprintf(&b, "    ca: Y2E=\n    crt: %s\n    key: a2V5\n", crt)

		if c.Discovery != "" {
			fmt.Fprintf(&b, "    discovery:\n      cluster_id: %s\n      cluster_secret: %s\n", c.Discovery, Secret)
		}
	}

	return b.String()
}

// quoteList renders a YAML flow list of quoted strings, so addresses such
// as "[2001:db8::1]:50000" stay strings.
func quoteList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, item := range items {
		quoted = append(quoted, fmt.Sprintf("%q", item))
	}

	return strings.Join(quoted, ", ")
}
