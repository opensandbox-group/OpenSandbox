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

package mitmproxy

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
)

const (
	// SeedCACertName / SeedCAKeyName are the pinned-CA Secret entries on the
	// read-only seed mount (Kubernetes tls-Secret conventions).
	SeedCACertName = "tls.crt"
	SeedCAKeyName  = "tls.key"
	// mitmproxy loads both certificate and key from this bundle; fingerprint tooling requires the certificate first.
	confCAKeyName = "mitmproxy-ca.pem"
)

// SeedPinnedCA returns the certificate's lowercase hex SHA-256 DER fingerprint; invalid seeds must not fall back to generated CAs.
func SeedPinnedCA(seedDir, confDir string, uid, gid int) (string, error) {
	seedDir = strings.TrimSpace(seedDir)
	if seedDir == "" {
		return "", fmt.Errorf("pinned CA seed mount is empty (set %s to the read-only secret mount)", constants.EnvMitmCaSeedDir)
	}
	certPEM, err := os.ReadFile(filepath.Join(seedDir, SeedCACertName))
	if err != nil {
		return "", fmt.Errorf("read pinned CA certificate %s: %w", filepath.Join(seedDir, SeedCACertName), err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(seedDir, SeedCAKeyName))
	if err != nil {
		return "", fmt.Errorf("read pinned CA key %s: %w", filepath.Join(seedDir, SeedCAKeyName), err)
	}
	return seedPinnedCA(certPEM, keyPEM, confDir, uid, gid)
}

func seedPinnedCA(certPEM, keyPEM []byte, confDir string, uid, gid int) (string, error) {
	// Validate before writing so mitmdump cannot replace a half-seeded CA with a generated one.
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return "", fmt.Errorf("pinned CA keypair (%s + %s): %w", SeedCACertName, SeedCAKeyName, err)
	}
	cert, err := parseCertificatePEM(certPEM)
	if err != nil {
		return "", fmt.Errorf("pinned CA certificate: %w", err)
	}
	if err := validateCACertificate(cert); err != nil {
		return "", err
	}
	fp := certFingerprint(cert)
	bundle := combineCAPEM(certPEM, keyPEM)

	// Preserve matching files but refresh ownership: image upgrades may change the mitmdump UID.
	if samePinnedPair(confDir, certPEM, bundle) {
		if err := chownConfCA(confDir, uid, gid); err != nil {
			return "", err
		}
		return fp, nil
	}

	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir confdir %s: %w", confDir, err)
	}
	// The unprivileged mitmdump user must be able to traverse confdir and read its 0600 key bundle.
	if err := os.Chown(confDir, uid, gid); err != nil {
		return "", fmt.Errorf("chown confdir %s: %w", confDir, err)
	}
	if err := writeConfCAFile(confDir, confCAKeyName, bundle, 0o600, uid, gid); err != nil {
		return "", fmt.Errorf("seed %s: %w", confCAKeyName, err)
	}
	if err := writeConfCAFile(confDir, mitmCACertName, certPEM, 0o644, uid, gid); err != nil {
		return "", fmt.Errorf("seed %s: %w", mitmCACertName, err)
	}
	log.Infof("[mitmproxy] pinned CA seeded into confdir %s (fingerprint %s)", confDir, fp)
	return fp, nil
}

func samePinnedPair(confDir string, certPEM, bundle []byte) bool {
	existingBundle, err := os.ReadFile(filepath.Join(confDir, confCAKeyName))
	if err != nil || !bytes.Equal(existingBundle, bundle) {
		return false
	}
	existingCert, err := os.ReadFile(filepath.Join(confDir, mitmCACertName))
	if err != nil {
		return false
	}
	return bytes.Equal(existingCert, certPEM)
}

// VerifyConfdirPinnedCA checks both files: checking only the public copy would miss a swapped bundle actually loaded by mitmdump.
func VerifyConfdirPinnedCA(confDir, wantFingerprint string) error {
	for _, name := range []string{confCAKeyName, mitmCACertName} {
		pemBytes, err := os.ReadFile(filepath.Join(confDir, name))
		if err != nil {
			return fmt.Errorf("read confdir %s: %w", name, err)
		}
		got, err := pemCertFingerprint(pemBytes)
		if err != nil {
			return fmt.Errorf("confdir %s: %w", name, err)
		}
		if got != wantFingerprint {
			return fmt.Errorf("confdir %s certificate %s does not match pinned CA %s", name, got, wantFingerprint)
		}
	}
	return nil
}

// pemCertFingerprint returns the lowercase hex SHA-256 fingerprint of the
// first certificate in a PEM bundle.
func pemCertFingerprint(certPEM []byte) (string, error) {
	cert, err := parseCertificatePEM(certPEM)
	if err != nil {
		return "", err
	}
	return certFingerprint(cert), nil
}

func parseCertificatePEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no CERTIFICATE PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// Keypair and fingerprint consistency alone do not establish a valid interception trust anchor.
func validateCACertificate(cert *x509.Certificate) error {
	name := cert.Subject.CommonName
	if !cert.IsCA {
		return fmt.Errorf("pinned CA certificate %q is not a CA (basicConstraints IsCA=false)", name)
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return fmt.Errorf("pinned CA certificate %q lacks the keyCertSign key usage", name)
	}
	now := time.Now()
	if now.Before(cert.NotBefore) {
		return fmt.Errorf("pinned CA certificate %q is not valid until %s", name, cert.NotBefore.UTC().Format(time.RFC3339))
	}
	if now.After(cert.NotAfter) {
		return fmt.Errorf("pinned CA certificate %q expired at %s", name, cert.NotAfter.UTC().Format(time.RFC3339))
	}
	return nil
}

func certFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

func combineCAPEM(certPEM, keyPEM []byte) []byte {
	cert := bytes.TrimRight(certPEM, "\n")
	key := bytes.TrimRight(keyPEM, "\n")
	out := make([]byte, 0, len(cert)+len(key)+2)
	out = append(out, cert...)
	out = append(out, '\n', '\n')
	out = append(out, key...)
	out = append(out, '\n')
	return out
}

func chownConfCA(confDir string, uid, gid int) error {
	if err := os.Chown(confDir, uid, gid); err != nil {
		return fmt.Errorf("chown confdir %s: %w", confDir, err)
	}
	for _, name := range []string{confCAKeyName, mitmCACertName} {
		if err := os.Chown(filepath.Join(confDir, name), uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", name, err)
		}
	}
	return nil
}

func writeConfCAFile(dir, name string, data []byte, mode os.FileMode, uid, gid int) error {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chown(tmpPath, uid, gid); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(dir, name))
}
