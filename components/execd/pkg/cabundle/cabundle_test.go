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

package cabundle

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func generateTestCertWithWindow(t *testing.T, cn string, serial int64, isCA bool, keyUsage x509.KeyUsage, notBefore, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              keyUsage,
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func generateTestCert(t *testing.T, cn string, serial int64, isCA bool, keyUsage x509.KeyUsage, notAfter time.Time) string {
	return generateTestCertWithWindow(t, cn, serial, isCA, keyUsage, time.Now().Add(-2*time.Hour), notAfter)
}

func generateTestCA(t *testing.T, cn string, serial int64) string {
	return generateTestCert(t, cn, serial, true, x509.KeyUsageCertSign|x509.KeyUsageCRLSign, time.Now().Add(24*time.Hour))
}

func TestDefaultConfig(t *testing.T) {
	t.Setenv("OPENSANDBOX_MITM_CA", "/custom/mitm-ca.pem")
	t.Setenv("OPENSANDBOX_MERGED_CA_PATH", "/custom/merged.pem")
	t.Setenv("NODE_EXTRA_CA_CERTS", "/custom/mitm-ca.pem")

	cfg := DefaultConfig()
	require.Equal(t, DefaultStateDir, cfg.StateDir)
	require.Equal(t, DefaultTrustAnchorDir, cfg.TrustAnchorDir)
	require.Equal(t, "/custom/merged.pem", cfg.PreinstalledBundlePath)
	require.NotEmpty(t, cfg.SystemBundlePaths)
	require.Equal(t, "/custom/mitm-ca.pem", cfg.PreinstalledCertPaths[0])
	require.Equal(t, []string{"/custom/mitm-ca.pem"}, cfg.InheritExtraCAPaths)
}

func TestParsePEMBundle(t *testing.T) {
	ca1 := generateTestCA(t, "test-ca-1", 1)
	ca2 := generateTestCA(t, "test-ca-2", 2)

	t.Run("empty clears bundle", func(t *testing.T) {
		certs, err := ParsePEMBundle("   \n\t ")
		require.NoError(t, err)
		require.Empty(t, certs)
	})

	t.Run("multiple certs deduplicated", func(t *testing.T) {
		certs, err := ParsePEMBundle(ca1 + "\n" + ca2 + "\n" + ca1)
		require.NoError(t, err)
		require.Len(t, certs, 2)
		require.NotEqual(t, certs[0].Fingerprint, certs[1].Fingerprint)
	})

	t.Run("rejects garbage and non-cert PEM", func(t *testing.T) {
		_, err := ParsePEMBundle("not-a-pem")
		require.Error(t, err)

		_, err = ParsePEMBundle(ca1 + "\ntrailing-garbage")
		require.Error(t, err)

		nonCert := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("abc")}))
		_, err = ParsePEMBundle(nonCert)
		require.Error(t, err)

		badDER := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-der")}))
		_, err = ParsePEMBundle(badDER)
		require.Error(t, err)
	})

	t.Run("rejects non-CA leaf, missing keyCertSign, and expired CA (R8)", func(t *testing.T) {
		leaf := generateTestCert(t, "leaf-cert", 10, false, x509.KeyUsageDigitalSignature, time.Now().Add(24*time.Hour))
		_, err := ParsePEMBundle(leaf)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not a CA")

		noCertSign := generateTestCert(t, "ca-no-certsign", 11, true, x509.KeyUsageDigitalSignature, time.Now().Add(24*time.Hour))
		_, err = ParsePEMBundle(noCertSign)
		require.Error(t, err)
		require.Contains(t, err.Error(), "keyCertSign")

		expired := generateTestCert(t, "expired-ca", 12, true, x509.KeyUsageCertSign, time.Now().Add(-time.Minute))
		_, err = ParsePEMBundle(expired)
		require.Error(t, err)
		require.Contains(t, err.Error(), "expired")

		future := generateTestCertWithWindow(t, "future-ca", 13, true, x509.KeyUsageCertSign, time.Now().Add(time.Hour), time.Now().Add(24*time.Hour))
		_, err = ParsePEMBundle(future)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not valid until")
	})

	t.Run("rejects oversized bundle and exceeding cert count cap", func(t *testing.T) {
		_, err := ParsePEMBundle(strings.Repeat("a", MaxBundleBytes+1))
		require.Error(t, err)

		var b strings.Builder
		for i := 0; i <= MaxBundleCerts; i++ {
			b.WriteString(generateTestCA(t, "ca-cap", int64(100+i)))
		}
		_, err = ParsePEMBundle(b.String())
		require.Error(t, err)
		require.Contains(t, err.Error(), "must not exceed")
	})
}

func TestManagerReconcileInstallRotateAndClear(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	trustDir := filepath.Join(root, "trust")
	sysBundle := filepath.Join(root, "ca-certificates.crt")
	require.NoError(t, os.MkdirAll(trustDir, 0o755))

	baseCA := generateTestCA(t, "base-root", 10)
	unmanagedCA := generateTestCA(t, "unmanaged-root", 11)
	ca1 := generateTestCA(t, "mitm-ca-v1", 21)
	ca2 := generateTestCA(t, "mitm-ca-v2", 22)

	require.NoError(t, os.WriteFile(sysBundle, []byte(baseCA), 0o644))
	unmanagedPath := filepath.Join(trustDir, "custom-user.crt")
	require.NoError(t, os.WriteFile(unmanagedPath, []byte(unmanagedCA), 0o644))

	mgr := NewManager(Config{
		StateDir:          stateDir,
		SystemBundlePaths: []string{sysBundle},
		TrustAnchorDir:    trustDir,
	})

	// 1. Install v1.
	certs1, err := ParsePEMBundle(ca1)
	require.NoError(t, err)
	res1, err := mgr.Reconcile(certs1)
	require.NoError(t, err)
	require.Empty(t, res1.Warnings)
	require.Equal(t, filepath.Join(stateDir, bundleFileName), res1.BundlePath)
	require.Equal(t, filepath.Join(stateDir, extraFileName), res1.ExtraPath)
	require.Equal(t, []string{certs1[0].Fingerprint}, res1.Fingerprints)

	combined, err := os.ReadFile(res1.BundlePath)
	require.NoError(t, err)
	require.Contains(t, string(combined), baseCA)
	require.Contains(t, string(combined), ca1)

	sysAfter1, err := os.ReadFile(sysBundle)
	require.NoError(t, err)
	require.Equal(t, combined, sysAfter1)

	v1File := filepath.Join(trustDir, managedCertPrefix+certs1[0].Fingerprint+managedCertSuffix)
	require.FileExists(t, v1File)
	require.FileExists(t, unmanagedPath)

	// 2. Rotate to v2: v1 must be removed, base + unmanaged preserved, v2 installed.
	certs2, err := ParsePEMBundle(ca2)
	require.NoError(t, err)
	res2, err := mgr.Reconcile(certs2)
	require.NoError(t, err)
	require.Empty(t, res2.Warnings)
	require.Equal(t, []string{certs2[0].Fingerprint}, res2.Fingerprints)

	combined2, err := os.ReadFile(res2.BundlePath)
	require.NoError(t, err)
	require.Contains(t, string(combined2), baseCA)
	require.Contains(t, string(combined2), ca2)
	require.NotContains(t, string(combined2), ca1)

	require.NoFileExists(t, v1File)
	v2File := filepath.Join(trustDir, managedCertPrefix+certs2[0].Fingerprint+managedCertSuffix)
	require.FileExists(t, v2File)
	require.FileExists(t, unmanagedPath)

	// 3. Clear (empty slice): restores pristine base system bundle and removes managed files.
	res3, err := mgr.Reconcile(nil)
	require.NoError(t, err)
	require.Empty(t, res3.BundlePath)
	require.Empty(t, res3.ExtraPath)
	require.Empty(t, res3.Fingerprints)

	require.NoFileExists(t, v2File)
	require.NoFileExists(t, filepath.Join(stateDir, bundleFileName))
	require.NoFileExists(t, filepath.Join(stateDir, extraFileName))
	require.FileExists(t, unmanagedPath)

	sysAfterClear, err := os.ReadFile(sysBundle)
	require.NoError(t, err)
	require.Equal(t, baseCA, string(sysAfterClear))
}

func TestManagerReconcileNoOpWhenCertUnchanged(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	trustDir := filepath.Join(root, "trust")
	sysBundle := filepath.Join(root, "ca-certificates.crt")
	require.NoError(t, os.MkdirAll(trustDir, 0o755))
	require.NoError(t, os.WriteFile(sysBundle, []byte(generateTestCA(t, "base-root", 1)), 0o644))

	mgr := NewManager(Config{
		StateDir:          stateDir,
		SystemBundlePaths: []string{sysBundle},
		TrustAnchorDir:    trustDir,
	})

	ca1 := generateTestCA(t, "mitm-ca-v1", 2)
	certs1, err := ParsePEMBundle(ca1)
	require.NoError(t, err)

	res1, err := mgr.Reconcile(certs1)
	require.NoError(t, err)

	// Record mod times and set them in the past so any rewrite is detectable.
	past := time.Now().Add(-time.Minute).Truncate(time.Second)
	for _, p := range []string{res1.BundlePath, res1.ExtraPath, sysBundle, filepath.Join(stateDir, stateFileName)} {
		require.NoError(t, os.Chtimes(p, past, past))
	}

	// Reconciling the same cert on the same manager (in-memory fast path) must not write any files.
	res2, err := mgr.Reconcile(certs1)
	require.NoError(t, err)
	require.Equal(t, res1, res2)
	for _, p := range []string{res1.BundlePath, res1.ExtraPath, sysBundle, filepath.Join(stateDir, stateFileName)} {
		info, statErr := os.Stat(p)
		require.NoError(t, statErr)
		require.Equal(t, past, info.ModTime().Truncate(time.Second), "file %s should not be rewritten on no-op", p)
	}

	// A fresh manager instance (simulating execd restart) also hits the on-disk state fast path.
	mgr2 := NewManager(Config{
		StateDir:          stateDir,
		SystemBundlePaths: []string{sysBundle},
		TrustAnchorDir:    trustDir,
	})
	res3, err := mgr2.Reconcile(certs1)
	require.NoError(t, err)
	require.Equal(t, res1, res3)
	for _, p := range []string{res1.BundlePath, res1.ExtraPath, sysBundle, filepath.Join(stateDir, stateFileName)} {
		info, statErr := os.Stat(p)
		require.NoError(t, statErr)
		require.Equal(t, past, info.ModTime().Truncate(time.Second), "file %s should not be rewritten after restart", p)
	}
}

func TestManagerReconcilePreinstalledFastPathVerifiesBundleContents(t *testing.T) {
	baseCA := generateTestCA(t, "base-root", 1)
	mitmCA := generateTestCA(t, "bootstrap-mitm-ca", 2)
	certs, err := ParsePEMBundle(mitmCA)
	require.NoError(t, err)

	t.Run("skips writes when preinstalled bundle contains requested CA", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		trustDir := filepath.Join(root, "trust")
		sysBundle := filepath.Join(root, "ca-certificates.crt")
		bootstrapCA := filepath.Join(root, "mitmproxy-ca-cert.pem")
		bootstrapMerged := filepath.Join(root, "merged-ca-certificates.pem")
		require.NoError(t, os.MkdirAll(trustDir, 0o755))

		require.NoError(t, os.WriteFile(sysBundle, []byte(baseCA+mitmCA), 0o644))
		require.NoError(t, os.WriteFile(bootstrapCA, []byte(mitmCA), 0o644))
		require.NoError(t, os.WriteFile(bootstrapMerged, []byte(baseCA+mitmCA), 0o644))

		mgr := NewManager(Config{
			StateDir:               stateDir,
			SystemBundlePaths:      []string{sysBundle},
			TrustAnchorDir:         trustDir,
			PreinstalledCertPaths:  []string{bootstrapCA},
			PreinstalledBundlePath: bootstrapMerged,
		})

		res, err := mgr.Reconcile(certs)
		require.NoError(t, err)
		require.Equal(t, bootstrapMerged, res.BundlePath)
		require.Equal(t, bootstrapCA, res.ExtraPath)
		require.Equal(t, []string{certs[0].Fingerprint}, res.Fingerprints)
		require.NoDirExists(t, stateDir)
	})

	t.Run("falls back to slow path when preinstalled bundle lacks requested CA (R1/R1b)", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		trustDir := filepath.Join(root, "trust")
		sysBundle := filepath.Join(root, "ca-certificates.crt")
		bootstrapCA := filepath.Join(root, "mitmproxy-ca-cert.pem")
		staleMerged := filepath.Join(root, "merged-ca-certificates.pem")
		require.NoError(t, os.MkdirAll(trustDir, 0o755))

		// System and merged bundles contain ONLY baseCA (missing mitmCA).
		require.NoError(t, os.WriteFile(sysBundle, []byte(baseCA), 0o644))
		require.NoError(t, os.WriteFile(bootstrapCA, []byte(mitmCA), 0o644))
		require.NoError(t, os.WriteFile(staleMerged, []byte(baseCA), 0o644))

		mgr := NewManager(Config{
			StateDir:               stateDir,
			SystemBundlePaths:      []string{sysBundle},
			TrustAnchorDir:         trustDir,
			PreinstalledCertPaths:  []string{bootstrapCA},
			PreinstalledBundlePath: staleMerged,
		})

		res, err := mgr.Reconcile(certs)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(stateDir, bundleFileName), res.BundlePath)

		published, readErr := os.ReadFile(res.BundlePath)
		require.NoError(t, readErr)
		require.Contains(t, string(published), baseCA)
		require.Contains(t, string(published), mitmCA)
	})
}

func TestManagerRetainsOwnershipWhenAnchorRemovalFails(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	trustDir := filepath.Join(root, "trust")
	sysBundle := filepath.Join(root, "ca-certificates.crt")
	require.NoError(t, os.MkdirAll(trustDir, 0o755))
	require.NoError(t, os.WriteFile(sysBundle, []byte(generateTestCA(t, "base", 1)), 0o644))

	mgr := NewManager(Config{
		StateDir:          stateDir,
		SystemBundlePaths: []string{sysBundle},
		TrustAnchorDir:    trustDir,
	})

	certs1, err := ParsePEMBundle(generateTestCA(t, "ca-1", 2))
	require.NoError(t, err)
	certs2, err := ParsePEMBundle(generateTestCA(t, "ca-2", 3))
	require.NoError(t, err)

	_, err = mgr.Reconcile(certs1)
	require.NoError(t, err)
	v1File := filepath.Join(trustDir, managedCertPrefix+certs1[0].Fingerprint+managedCertSuffix)
	require.FileExists(t, v1File)

	// Replace v1File with a non-empty directory so os.Remove fails even as uid 0 (R3).
	require.NoError(t, os.Remove(v1File))
	require.NoError(t, os.MkdirAll(filepath.Join(v1File, "blocker"), 0o755))

	res2, err := mgr.Reconcile(certs2)
	require.NoError(t, err)
	require.NotEmpty(t, res2.Warnings)
	require.DirExists(t, v1File)

	// Restore v1File as a regular file and clear: retained ownership must clean it up.
	require.NoError(t, os.RemoveAll(v1File))
	require.NoError(t, os.WriteFile(v1File, certs1[0].PEM, 0o644))
	res3, err := mgr.Reconcile(nil)
	require.NoError(t, err)
	require.Empty(t, res3.Warnings)
	require.NoFileExists(t, v1File)
}

func TestManagerSurfacesWarningOnUnwritableSystemBundle(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	trustDir := filepath.Join(root, "trust")
	sysDir := filepath.Join(root, "sys")
	sysBundle := filepath.Join(sysDir, "ca-certificates.crt")
	require.NoError(t, os.MkdirAll(trustDir, 0o755))
	require.NoError(t, os.MkdirAll(sysDir, 0o755))
	baseCA := generateTestCA(t, "base", 1)
	require.NoError(t, os.WriteFile(sysBundle, []byte(baseCA), 0o644))

	mgr := NewManager(Config{
		StateDir:          stateDir,
		SystemBundlePaths: []string{sysBundle},
		TrustAnchorDir:    trustDir,
	})

	// Seed base-bundle.pem and state.json with a first reconcile, then replace
	// sysBundle with a non-empty directory so atomic rename fails even as uid 0 (R7).
	certs1, err := ParsePEMBundle(generateTestCA(t, "ca-1", 2))
	require.NoError(t, err)
	_, err = mgr.Reconcile(certs1)
	require.NoError(t, err)

	require.NoError(t, os.Remove(sysBundle))
	require.NoError(t, os.MkdirAll(filepath.Join(sysBundle, "blocker"), 0o755))

	certs2, err := ParsePEMBundle(generateTestCA(t, "ca-2", 3))
	require.NoError(t, err)
	res, err := mgr.Reconcile(certs2)
	require.NoError(t, err)
	require.NotEmpty(t, res.Warnings)

	// Once sysBundle is a regular file again, a subsequent Reconcile with the
	// same certs (even after restart + LoadActive) must retry syncing it.
	require.NoError(t, os.RemoveAll(sysBundle))
	require.NoError(t, os.WriteFile(sysBundle, []byte(baseCA), 0o644))

	restarted := NewManager(Config{
		StateDir:          stateDir,
		SystemBundlePaths: []string{sysBundle},
		TrustAnchorDir:    trustDir,
	})
	loaded, err := restarted.LoadActive()
	require.NoError(t, err)
	require.NotEmpty(t, loaded.Warnings)

	resRetry, err := restarted.Reconcile(certs2)
	require.NoError(t, err)
	require.Empty(t, resRetry.Warnings)
	sysContent, readErr := os.ReadFile(sysBundle)
	require.NoError(t, readErr)
	require.Contains(t, string(sysContent), string(certs2[0].PEM))
}

func TestManagerRejectsTamperedSystemBundlePathInState(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	trustDir := filepath.Join(root, "trust")
	sysBundle := filepath.Join(root, "ca-certificates.crt")
	victim := filepath.Join(root, "victim.txt")
	require.NoError(t, os.MkdirAll(stateDir, 0o755))
	require.NoError(t, os.MkdirAll(trustDir, 0o755))
	require.NoError(t, os.WriteFile(sysBundle, []byte(generateTestCA(t, "base", 1)), 0o644))
	require.NoError(t, os.WriteFile(victim, []byte("keep-me\n"), 0o644))

	tampered, err := json.Marshal(persistedState{
		Version:          stateSchemaVersion,
		SystemBundlePath: victim,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stateDir, stateFileName), tampered, 0o644))

	mgr := NewManager(Config{
		StateDir:          stateDir,
		SystemBundlePaths: []string{sysBundle},
		TrustAnchorDir:    trustDir,
	})

	certs, err := ParsePEMBundle(generateTestCA(t, "ca-1", 2))
	require.NoError(t, err)
	_, err = mgr.Reconcile(certs)
	require.NoError(t, err)

	victimData, readErr := os.ReadFile(victim)
	require.NoError(t, readErr)
	require.Equal(t, "keep-me\n", string(victimData), "tampered systemBundlePath must never overwrite arbitrary files (R9)")
}

func TestManagerLoadActiveRestoresAfterRestart(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	trustDir := filepath.Join(root, "trust")
	sysBundle := filepath.Join(root, "ca-certificates.crt")
	require.NoError(t, os.MkdirAll(trustDir, 0o755))
	require.NoError(t, os.WriteFile(sysBundle, []byte(generateTestCA(t, "base", 1)), 0o644))

	cfg := Config{
		StateDir:          stateDir,
		SystemBundlePaths: []string{sysBundle},
		TrustAnchorDir:    trustDir,
	}
	certs, err := ParsePEMBundle(generateTestCA(t, "ca-1", 2))
	require.NoError(t, err)
	res1, err := NewManager(cfg).Reconcile(certs)
	require.NoError(t, err)

	// Fresh Manager simulating execd restart loads persisted state (R5).
	loaded, err := NewManager(cfg).LoadActive()
	require.NoError(t, err)
	require.Empty(t, loaded.Warnings)
	require.Equal(t, res1.BundlePath, loaded.BundlePath)
	require.Equal(t, res1.ExtraPath, loaded.ExtraPath)
	require.Equal(t, res1.Fingerprints, loaded.Fingerprints)
}

func TestManagerFoldsInheritedExtraCACerts(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	trustDir := filepath.Join(root, "trust")
	sysBundle := filepath.Join(root, "ca-certificates.crt")
	mitmCAFile := filepath.Join(root, "mitmproxy-ca-cert.pem")
	require.NoError(t, os.MkdirAll(trustDir, 0o755))

	baseCA := generateTestCA(t, "base-root", 1)
	mitmCA := generateTestCA(t, "bootstrap-mitm-ca", 2)
	corpCA := generateTestCA(t, "corp-proxy-ca", 3)
	require.NoError(t, os.WriteFile(sysBundle, []byte(baseCA), 0o644))
	require.NoError(t, os.WriteFile(mitmCAFile, []byte(mitmCA), 0o644))

	mgr := NewManager(Config{
		StateDir:            stateDir,
		SystemBundlePaths:   []string{sysBundle},
		TrustAnchorDir:      trustDir,
		InheritExtraCAPaths: []string{mitmCAFile},
	})

	certs, err := ParsePEMBundle(corpCA)
	require.NoError(t, err)
	res, err := mgr.Reconcile(certs)
	require.NoError(t, err)
	require.Empty(t, res.Warnings)

	extraData, err := os.ReadFile(res.ExtraPath)
	require.NoError(t, err)
	require.Contains(t, string(extraData), mitmCA)
	require.Contains(t, string(extraData), corpCA)

	bundleData, err := os.ReadFile(res.BundlePath)
	require.NoError(t, err)
	require.Contains(t, string(bundleData), baseCA)
	require.Contains(t, string(bundleData), mitmCA)
	require.Contains(t, string(bundleData), corpCA)
}

func TestManagerLoadActiveDegradesOnCorruptOrUnsupportedState(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"corrupt json", "not json"},
		{"unsupported version", `{"version":99}`},
		{"missing version", `{"fingerprints":["abc"]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			require.NoError(t, os.MkdirAll(stateDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(stateDir, stateFileName), []byte(tc.content), 0o644))

			mgr := NewManager(Config{
				StateDir:       stateDir,
				TrustAnchorDir: t.TempDir(),
			})
			res, err := mgr.LoadActive()
			require.NoError(t, err)
			require.Empty(t, res.Fingerprints)
			require.NotEmpty(t, res.Warnings)
		})
	}
}

func TestManagerRetriesFailedAnchorWriteAfterRestart(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	trustDir := filepath.Join(root, "trust")
	sysBundle := filepath.Join(root, "ca-certificates.crt")
	require.NoError(t, os.MkdirAll(trustDir, 0o755))
	require.NoError(t, os.WriteFile(sysBundle, []byte(generateTestCA(t, "base", 1)), 0o644))

	certs, err := ParsePEMBundle(generateTestCA(t, "ca-1", 2))
	require.NoError(t, err)
	anchorTarget := filepath.Join(trustDir, managedCertPrefix+certs[0].Fingerprint+managedCertSuffix)
	// Block atomic rename to anchorTarget with a non-empty directory (fails even as uid 0).
	require.NoError(t, os.MkdirAll(filepath.Join(anchorTarget, "blocker"), 0o755))

	cfg := Config{
		StateDir:          stateDir,
		SystemBundlePaths: []string{sysBundle},
		TrustAnchorDir:    trustDir,
	}
	res1, err := NewManager(cfg).Reconcile(certs)
	require.NoError(t, err)
	require.NotEmpty(t, res1.Warnings)

	// Fresh manager with same certs must NOT take disk fast path while AnchorsSynced=false.
	res2, err := NewManager(cfg).Reconcile(certs)
	require.NoError(t, err)
	require.NotEmpty(t, res2.Warnings)

	// Unblock anchorTarget and retry: now it converges cleanly.
	require.NoError(t, os.RemoveAll(anchorTarget))
	res3, err := NewManager(cfg).Reconcile(certs)
	require.NoError(t, err)
	require.Empty(t, res3.Warnings)
	require.FileExists(t, anchorTarget)
}

func TestManagerClearPreservesBundleFilesWhenRequested(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	trustDir := filepath.Join(root, "trust")
	sysBundle := filepath.Join(root, "ca-certificates.crt")
	require.NoError(t, os.MkdirAll(trustDir, 0o755))

	baseCA := generateTestCA(t, "base-root", 1)
	ca1 := generateTestCA(t, "custom-ca", 2)
	require.NoError(t, os.WriteFile(sysBundle, []byte(baseCA), 0o644))

	mgr := NewManager(Config{
		StateDir:          stateDir,
		SystemBundlePaths: []string{sysBundle},
		TrustAnchorDir:    trustDir,
	})

	certs1, err := ParsePEMBundle(ca1)
	require.NoError(t, err)
	res1, err := mgr.Reconcile(certs1)
	require.NoError(t, err)

	// Clear with PreserveBundleFilesOnClear keeps bundle.pem (base-only) and extra.pem on disk.
	resClear, err := mgr.ReconcileWithOptions(nil, ReconcileOptions{PreserveBundleFilesOnClear: true})
	require.NoError(t, err)
	require.Empty(t, resClear.BundlePath)
	require.Empty(t, resClear.ExtraPath)
	require.FileExists(t, res1.BundlePath)
	require.FileExists(t, res1.ExtraPath)

	bundleAfterClear, err := os.ReadFile(res1.BundlePath)
	require.NoError(t, err)
	require.Equal(t, baseCA, string(bundleAfterClear))
	require.NotContains(t, string(bundleAfterClear), ca1)

	extraAfterClear, err := os.ReadFile(res1.ExtraPath)
	require.NoError(t, err)
	require.Empty(t, extraAfterClear)

	// Subsequent full reset (PreserveBundleFilesOnClear=false) removes the files.
	_, err = mgr.Reconcile(nil)
	require.NoError(t, err)
	require.NoFileExists(t, res1.BundlePath)
	require.NoFileExists(t, res1.ExtraPath)
}

func TestManagerWarnsWhenTrustAnchorDirMissing(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	sysBundle := filepath.Join(root, "ca-certificates.crt")
	require.NoError(t, os.WriteFile(sysBundle, []byte(generateTestCA(t, "base", 1)), 0o644))

	mgr := NewManager(Config{
		StateDir:          stateDir,
		SystemBundlePaths: []string{sysBundle},
		TrustAnchorDir:    filepath.Join(root, "does-not-exist"),
	})

	certs, err := ParsePEMBundle(generateTestCA(t, "ca-1", 2))
	require.NoError(t, err)
	res, err := mgr.Reconcile(certs)
	require.NoError(t, err)
	require.NotEmpty(t, res.Warnings)
	require.Contains(t, res.Warnings[0], "does not exist")
}
