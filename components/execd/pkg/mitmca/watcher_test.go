// Copyright 2026 Alibaba Group Holding Ltd.
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

package mitmca

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSetProcessTrustEnvironment(t *testing.T) {
	t.Setenv("NODE_EXTRA_CA_CERTS", "")
	t.Setenv("REQUESTS_CA_BUNDLE", "")
	t.Setenv("SSL_CERT_FILE", "")

	require.NoError(t, setProcessTrustEnvironment(os.Getenv, os.Setenv, nil))
	require.Equal(t, defaultCAPath, os.Getenv("NODE_EXTRA_CA_CERTS"))
	require.Equal(t, defaultMergedCAPath, os.Getenv("REQUESTS_CA_BUNDLE"))
	require.Equal(t, defaultMergedCAPath, os.Getenv("SSL_CERT_FILE"))
}

func TestSetProcessTrustEnvironmentPreservesOverrides(t *testing.T) {
	values := map[string]string{
		"NODE_EXTRA_CA_CERTS": "/custom/node.pem",
		"REQUESTS_CA_BUNDLE":  "/custom/requests.pem",
		"SSL_CERT_FILE":       "/custom/ssl.pem",
	}

	err := setProcessTrustEnvironment(func(name string) string {
		return values[name]
	}, func(name, value string) error {
		values[name] = value
		return nil
	}, nil)

	require.NoError(t, err)
	require.Equal(t, "/custom/node.pem", values["NODE_EXTRA_CA_CERTS"])
	require.Equal(t, "/custom/requests.pem", values["REQUESTS_CA_BUNDLE"])
	require.Equal(t, "/custom/ssl.pem", values["SSL_CERT_FILE"])
}

func TestSetProcessTrustEnvironmentReplacesManagedFallbacks(t *testing.T) {
	values := map[string]string{
		"NODE_EXTRA_CA_CERTS": defaultCAPath,
		"REQUESTS_CA_BUNDLE":  defaultCAPath,
		"SSL_CERT_FILE":       defaultCAPath,
	}

	err := setProcessTrustEnvironment(func(name string) string {
		return values[name]
	}, func(name, value string) error {
		values[name] = value
		return nil
	}, nil)

	require.NoError(t, err)
	require.Equal(t, defaultCAPath, values["NODE_EXTRA_CA_CERTS"])
	require.Equal(t, defaultMergedCAPath, values["REQUESTS_CA_BUNDLE"])
	require.Equal(t, defaultMergedCAPath, values["SSL_CERT_FILE"])
}

func TestSetProcessTrustEnvironmentReportsFailure(t *testing.T) {
	err := setProcessTrustEnvironment(func(string) string { return "" }, func(name, _ string) error {
		if name == "REQUESTS_CA_BUNDLE" {
			return errors.New("setenv failed")
		}
		return nil
	}, nil)

	require.ErrorContains(t, err, "REQUESTS_CA_BUNDLE")
	require.ErrorContains(t, err, "setenv failed")
}

func TestSetProcessTrustEnvironmentReportsManagedUpdates(t *testing.T) {
	values := map[string]string{
		"NODE_EXTRA_CA_CERTS": "",
		"REQUESTS_CA_BUNDLE":  defaultCAPath,
		"SSL_CERT_FILE":       "/custom/ssl.pem",
	}
	type update struct {
		value           string
		managedFallback string
	}
	updates := make(map[string]update)

	err := setProcessTrustEnvironment(func(name string) string {
		return values[name]
	}, func(name, value string) error {
		values[name] = value
		return nil
	}, func(name, value, managedFallback string) {
		updates[name] = update{value: value, managedFallback: managedFallback}
	})

	require.NoError(t, err)
	require.Equal(t, map[string]update{
		"NODE_EXTRA_CA_CERTS": {value: defaultCAPath},
		"REQUESTS_CA_BUNDLE":  {value: defaultMergedCAPath, managedFallback: defaultCAPath},
	}, updates)
	require.NotContains(t, updates, "SSL_CERT_FILE")
}

func TestWatchRefreshesInitialCertificateAndValidRotations(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "mitmproxy-ca-cert.pem")
	first := testCertificate(t, 1)
	second := testCertificate(t, 2)
	require.NoError(t, os.WriteFile(caPath, first, 0o644))

	var refreshes atomic.Int32
	cancel, done := runTestWatcher(t, watchConfig{
		caPath:       caPath,
		pollInterval: 5 * time.Millisecond,
		refresh: func(context.Context) error {
			refreshes.Add(1)
			return nil
		},
	})
	defer stopTestWatcher(t, cancel, done)

	require.Eventually(t, func() bool { return refreshes.Load() == 1 }, time.Second, 5*time.Millisecond)
	require.NoError(t, os.WriteFile(caPath, second, 0o644))
	require.Eventually(t, func() bool { return refreshes.Load() == 2 }, time.Second, 5*time.Millisecond)

	require.NoError(t, os.WriteFile(caPath, second, 0o644))
	require.Never(t, func() bool { return refreshes.Load() != 2 }, 30*time.Millisecond, 5*time.Millisecond)
}

func TestWatchRefreshesWhenCertificateAppearsAfterStartup(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "mitmproxy-ca-cert.pem")
	var refreshes atomic.Int32
	cancel, done := runTestWatcher(t, watchConfig{
		caPath:       caPath,
		pollInterval: 5 * time.Millisecond,
		refresh: func(context.Context) error {
			refreshes.Add(1)
			return nil
		},
	})
	defer stopTestWatcher(t, cancel, done)

	require.NoError(t, os.WriteFile(caPath, testCertificate(t, 1), 0o644))
	require.Eventually(t, func() bool { return refreshes.Load() == 1 }, time.Second, 5*time.Millisecond)
}

func TestWatchIgnoresEmptyAndInvalidCertificates(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "mitmproxy-ca-cert.pem")
	var refreshes atomic.Int32
	cancel, done := runTestWatcher(t, watchConfig{
		caPath:       caPath,
		pollInterval: 5 * time.Millisecond,
		refresh: func(context.Context) error {
			refreshes.Add(1)
			return nil
		},
	})
	defer stopTestWatcher(t, cancel, done)

	require.NoError(t, os.WriteFile(caPath, nil, 0o644))
	require.NoError(t, os.WriteFile(caPath, []byte("not a certificate"), 0o644))
	require.Never(t, func() bool { return refreshes.Load() != 0 }, 30*time.Millisecond, 5*time.Millisecond)

	require.NoError(t, os.WriteFile(caPath, testCertificate(t, 1), 0o644))
	require.Eventually(t, func() bool { return refreshes.Load() == 1 }, time.Second, 5*time.Millisecond)
}

func TestWatchRetriesFailedRefreshWithoutAdvancingFingerprint(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "mitmproxy-ca-cert.pem")

	var attempts atomic.Int32
	cancel, done := runTestWatcher(t, watchConfig{
		caPath:       caPath,
		pollInterval: 5 * time.Millisecond,
		refresh: func(context.Context) error {
			if attempts.Add(1) == 1 {
				return errors.New("temporary refresh failure")
			}
			return nil
		},
	})
	defer stopTestWatcher(t, cancel, done)

	require.NoError(t, os.WriteFile(caPath, testCertificate(t, 2), 0o644))
	require.Eventually(t, func() bool { return attempts.Load() == 2 }, time.Second, 5*time.Millisecond)
	require.Never(t, func() bool { return attempts.Load() != 2 }, 30*time.Millisecond, 5*time.Millisecond)
}

func TestWatchRetriesWhenCertificateChangesDuringRefresh(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "mitmproxy-ca-cert.pem")
	second := testCertificate(t, 2)
	third := testCertificate(t, 3)

	var refreshes atomic.Int32
	cancel, done := runTestWatcher(t, watchConfig{
		caPath:       caPath,
		pollInterval: 5 * time.Millisecond,
		refresh: func(context.Context) error {
			if refreshes.Add(1) == 1 {
				return os.WriteFile(caPath, third, 0o644)
			}
			return nil
		},
	})
	defer stopTestWatcher(t, cancel, done)

	require.NoError(t, os.WriteFile(caPath, second, 0o644))
	require.Eventually(t, func() bool { return refreshes.Load() == 2 }, time.Second, 5*time.Millisecond)
	require.Never(t, func() bool { return refreshes.Load() != 2 }, 30*time.Millisecond, 5*time.Millisecond)
}

func TestReadCertificateFingerprintRejectsEndEntityCertificate(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "mitmproxy-ca-cert.pem")
	require.NoError(t, os.WriteFile(caPath, testEndEntityCertificate(t, 1), 0o644))

	_, available, err := readCertificateFingerprint(caPath)

	require.False(t, available)
	require.ErrorContains(t, err, "certificate is not a CA")
}

func runTestWatcher(t *testing.T, cfg watchConfig) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	ready := make(chan struct{})
	cfg.onReady = func() { close(ready) }
	go func() {
		defer close(done)
		watch(ctx, cfg)
	}()
	select {
	case <-ready:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("watcher did not complete its initial CA snapshot")
	}
	return cancel, done
}

func stopTestWatcher(t *testing.T, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop after context cancellation")
	}
}

func testCertificate(t *testing.T, serial int64) []byte {
	return testCertificateWithCAFlag(t, serial, true)
}

func testEndEntityCertificate(t *testing.T, serial int64) []byte {
	return testCertificateWithCAFlag(t, serial, false)
}

func testCertificateWithCAFlag(t *testing.T, serial int64, isCA bool) []byte {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(4102444800, 0),
		IsCA:                  isCA,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
	}
	if isCA {
		template.KeyUsage = x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
