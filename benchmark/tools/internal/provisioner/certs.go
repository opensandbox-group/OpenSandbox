// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provisioner

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// RegistryCertBundle holds the benchmark-scoped self-signed CA and server TLS
// certificate/key pair for the ephemeral in-cluster registry:2 service.
type RegistryCertBundle struct {
	CACertPEM     []byte
	ServerCertPEM []byte
	ServerKeyPEM  []byte
}

// GenerateRegistryCertBundle generates a self-signed CA and a server TLS
// certificate valid for the in-cluster ephemeral registry DNS names.
func GenerateRegistryCertBundle(serviceName, namespace string, validFor time.Duration) (*RegistryCertBundle, error) {
	if validFor <= 0 {
		validFor = 7 * 24 * time.Hour
	}
	now := time.Now().UTC().Add(-5 * time.Minute)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA private key: %w", err)
	}
	caSerial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber: caSerial,
		Subject: pkix.Name{
			Organization: []string{"OpenSandbox Benchmark"},
			CommonName:   fmt.Sprintf("%s-ca", serviceName),
		},
		NotBefore:             now,
		NotAfter:              now.Add(validFor),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate server private key: %w", err)
	}
	serverSerial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	dnsNames := []string{
		serviceName,
		fmt.Sprintf("%s.%s", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, namespace),
	}
	serverTmpl := &x509.Certificate{
		SerialNumber: serverSerial,
		Subject: pkix.Name{
			Organization: []string{"OpenSandbox Benchmark"},
			CommonName:   fmt.Sprintf("%s.%s.svc", serviceName, namespace),
		},
		DNSNames:              dnsNames,
		NotBefore:             now,
		NotAfter:              now.Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, caTmpl, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create server certificate: %w", err)
	}
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return nil, fmt.Errorf("marshal server private key: %w", err)
	}

	return &RegistryCertBundle{
		CACertPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		ServerCertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		ServerKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER}),
	}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	return n, nil
}

// buildDockerConfigJSON builds a .dockerconfigjson payload for anonymous access
// to the benchmark ephemeral registry (per docs/guides/oci-templates.md).
func buildDockerConfigJSON(registryHost string) ([]byte, error) {
	payload := map[string]any{
		"auths": map[string]any{
			registryHost: map[string]any{},
		},
	}
	return json.Marshal(payload)
}

// buildAgentRegistryJSON builds the fast-sandbox-agent-registry registry.json payload.
func buildAgentRegistryJSON(registryEndpoint string) ([]byte, error) {
	payload := map[string]any{
		"endpoint": registryEndpoint,
	}
	return json.MarshalIndent(payload, "", "  ")
}
