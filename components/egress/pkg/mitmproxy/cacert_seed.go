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

// Pinned-CA seeding (OSEP-0022 "MITM Trust Anchor", fast-sandbox profile):
// the pool-pinned CA keypair arrives on a read-only seed mount
// (constants.EnvMitmCaSeedDir, `tls.crt` + `tls.key` entries) and is copied
// idempotently into the writable confdir under the names mitmdump actually
// loads — confCAKeyName, a single file holding the certificate AND the
// private key (mitmproxy's CertStore.from_pem scans this one file for both
// PEM blocks; a key-only file makes mitmdump exit at startup), plus
// mitmCACertName (the public certificate copy). mitmdump loads a confdir CA
// when present and only generates when missing; in this profile it must
// never generate.

const (
	// SeedCACertName / SeedCAKeyName are the pinned-CA Secret entries on the
	// read-only seed mount (Kubernetes tls-Secret conventions).
	SeedCACertName = "tls.crt"
	SeedCAKeyName  = "tls.key"
	// confCAKeyName is the CA bundle mitmdump loads from the confdir: ONE
	// file containing both the certificate and the private key, in the shape
	// mitmproxy's own generate-on-start produces. Order matters for our own
	// tooling: the certificate goes first, so the first PEM block is a
	// CERTIFICATE.
	confCAKeyName = "mitmproxy-ca.pem"
)

// SeedPinnedCA copies the pinned CA keypair from the read-only seed mount into
// the writable confDir and returns the pinned certificate's SHA-256
// fingerprint (lowercase hex over the DER). A missing or unloadable pinned CA
// is an error: the caller fails the MITM addon closed — it never falls back to
// generation. uid/gid receive ownership of the confdir and the copied files so
// the unprivileged mitmdump user can read them.
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

// seedPinnedCA is the testable core of SeedPinnedCA.
func seedPinnedCA(certPEM, keyPEM []byte, confDir string, uid, gid int) (string, error) {
	// Validate the pair BEFORE touching the confdir: an unloadable pinned CA
	// must fail the addon closed, never leave a half-seeded confdir for
	// mitmdump to silently replace with a generated CA.
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return "", fmt.Errorf("pinned CA keypair (%s + %s): %w", SeedCACertName, SeedCAKeyName, err)
	}
	cert, err := parseCertificatePEM(certPEM)
	if err != nil {
		return "", fmt.Errorf("pinned CA certificate: %w", err)
	}
	// A certificate that cannot actually sign (non-CA, missing keyCertSign)
	// or is outside its validity window would seed cleanly and pass every
	// consistency check while every intercepted TLS handshake fails client
	// verification — exactly the silent failure the fail-closed design
	// exists to prevent. Refuse it at the seed.
	if err := validateCACertificate(cert); err != nil {
		return "", err
	}
	fp := certFingerprint(cert)
	bundle := combineCAPEM(certPEM, keyPEM)

	// Idempotent: a confdir already holding the exact pinned pair is left
	// byte-for-byte untouched, so restarts keep the files (and their inodes)
	// stable — but ownership is still re-asserted: an image upgrade can change
	// the mitmdump uid while the confdir persists, and a 0600 private-key
	// bundle the mitmdump user cannot read would surface downstream as a
	// misleading consistency failure instead of a permission error.
	if samePinnedPair(confDir, certPEM, bundle) {
		if err := chownConfCA(confDir, uid, gid); err != nil {
			return "", err
		}
		return fp, nil
	}

	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir confdir %s: %w", confDir, err)
	}
	// mitmdump runs as an unprivileged user; files written here by root must
	// be readable by it (the bundle carries the private key, mode 0600, so
	// ownership is not optional). writeConfCAFile chowns each file it lands.
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

// samePinnedPair reports whether the confdir already materializes the pinned
// pair (bundle and certificate files byte-identical to the seed).
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

// VerifyConfdirPinnedCA is the startup consistency check: BOTH confdir CA
// files must present a certificate matching the pinned fingerprint — the
// bundle mitmdump actually loads (confCAKeyName) and the public certificate
// copy (mitmCACertName). Checking only the copy would miss a swapped bundle
// (the copy is never read by mitmdump), silently running the addon on a
// non-pinned CA.
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

// parseCertificatePEM decodes and parses the first certificate in a PEM bundle.
func parseCertificatePEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no CERTIFICATE PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// validateCACertificate rejects certificates that cannot serve as an
// interception trust anchor: a non-CA or keyCertSign-less certificate, or one
// outside its validity window, would seed cleanly and pass every consistency
// check while all intercepted TLS handshakes fail client verification.
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

// certFingerprint returns the lowercase hex SHA-256 fingerprint over the DER.
func certFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// combineCAPEM builds the single-file confdir CA bundle mitmproxy loads:
// certificate first (so the first PEM block is a CERTIFICATE), then the
// private key — the same shape mitmproxy's own generate-on-start writes, and
// the only shape its CertStore.from_pem accepts (it scans the one file for
// both the CERTIFICATE and the private-key PEM blocks).
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

// chownConfCA re-asserts ownership of the confdir and both CA files (used on
// the idempotent fast path, where the files already exist but a changed
// mitmdump uid across image upgrades may have left them root-owned).
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

// writeConfCAFile atomically materializes one confdir file (temp + rename so
// a reader never observes a partial file).
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
