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

package crypto

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEnsureCertMeetsNISTMinimums_RSA1024Rejected(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)

	cert := &x509.Certificate{
		PublicKey:             &key.PublicKey,
		SignatureAlgorithm:    x509.SHA256WithRSA,
		SerialNumber:          big.NewInt(1),
		BasicConstraintsValid: true,
	}
	require.Error(t, ensureCertMeetsNISTMinimums(cert))
}

func TestEnsureCertMeetsNISTMinimums_EC224Accepted(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	require.NoError(t, err)

	cert := &x509.Certificate{
		PublicKey:             &key.PublicKey,
		SignatureAlgorithm:    x509.ECDSAWithSHA256,
		SerialNumber:          big.NewInt(2),
		BasicConstraintsValid: true,
	}
	require.NoError(t, ensureCertMeetsNISTMinimums(cert))
}

func TestEnsureCertMeetsNISTMinimums_SHA1Rejected(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	cert := &x509.Certificate{
		PublicKey:             &key.PublicKey,
		SignatureAlgorithm:    x509.SHA1WithRSA,
		SerialNumber:          big.NewInt(3),
		BasicConstraintsValid: true,
	}
	require.Error(t, ensureCertMeetsNISTMinimums(cert))
}

func TestEnsureCertMeetsNISTMinimums_UnknownSignatureAlgorithmRejected(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	cert := &x509.Certificate{
		PublicKey:             &key.PublicKey,
		SignatureAlgorithm:    x509.UnknownSignatureAlgorithm,
		SerialNumber:          big.NewInt(4),
		BasicConstraintsValid: true,
	}
	require.Error(t, ensureCertMeetsNISTMinimums(cert))
}

func TestValidateCertificateKeyPair_RejectsWeakRSA(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(10),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		SignatureAlgorithm:    x509.SHA256WithRSA,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM := pemEncode("CERTIFICATE", der)
	keyPEM := pemEncode("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
	certFile := writeTempFile(t, "weak-cert-*.pem", certPEM)
	keyFile := writeTempFile(t, "weak-key-*.pem", keyPEM)

	require.Error(t, ValidateCertificateKeyPair(certFile, keyFile))
}

func TestValidateCertificateKeyPair_AcceptsRSA2048(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(11),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		SignatureAlgorithm:    x509.SHA256WithRSA,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM := pemEncode("CERTIFICATE", der)
	keyPEM := pemEncode("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
	certFile := writeTempFile(t, "good-cert-*.pem", certPEM)
	keyFile := writeTempFile(t, "good-key-*.pem", keyPEM)

	require.NoError(t, ValidateCertificateKeyPair(certFile, keyFile))
}

func TestValidateTLSCertificate_RejectsWeakRSAFromTLSObject(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(12),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		SignatureAlgorithm:    x509.SHA256WithRSA,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	pair := &tls.Certificate{Certificate: [][]byte{der}}
	require.Error(t, ValidateTLSCertificate("weak-rotated-cert", pair))
}

func TestValidateTLSCertificate_AcceptsStrongCertFromTLSObject(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(13),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		SignatureAlgorithm:    x509.SHA256WithRSA,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	pair := &tls.Certificate{Certificate: [][]byte{der}}
	require.NoError(t, ValidateTLSCertificate("strong-rotated-cert", pair))
}

func TestValidateTLSCertificate_RejectsWeakIntermediate(t *testing.T) {
	// 3-cert chain: strong leaf (RSA 2048) + weak intermediate (RSA 1024) + strong root (RSA 2048).
	// The root is self-signed and should be skipped as a trust anchor,
	// but the weak intermediate must still be rejected.
	rootKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(99),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		SignatureAlgorithm:    x509.SHA256WithRSA,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	require.NoError(t, err)
	rootCert, err := x509.ParseCertificate(rootDER)
	require.NoError(t, err)

	weakKey, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)

	intermediateTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(100),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		SignatureAlgorithm:    x509.SHA256WithRSA,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	intermediateDER, err := x509.CreateCertificate(rand.Reader, intermediateTmpl, rootCert, &weakKey.PublicKey, rootKey)
	require.NoError(t, err)
	intermediateCert, err := x509.ParseCertificate(intermediateDER)
	require.NoError(t, err)

	strongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	leafTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(101),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		SignatureAlgorithm:    x509.SHA256WithRSA,
		BasicConstraintsValid: true,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, intermediateCert, &strongKey.PublicKey, weakKey)
	require.NoError(t, err)

	pair := &tls.Certificate{Certificate: [][]byte{leafDER, intermediateDER, rootDER}}
	err = ValidateTLSCertificate("chain-with-weak-intermediate", pair)
	require.Error(t, err)
	require.Contains(t, err.Error(), "[1]")
}

func TestValidateTLSCertificate_RejectsWeakRootKey(t *testing.T) {
	rootKey, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)

	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(200),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		SignatureAlgorithm:    x509.SHA256WithRSA,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	require.NoError(t, err)
	rootCert, err := x509.ParseCertificate(rootDER)
	require.NoError(t, err)

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	leafTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(201),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		SignatureAlgorithm:    x509.SHA256WithRSA,
		BasicConstraintsValid: true,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, rootCert, &leafKey.PublicKey, rootKey)
	require.NoError(t, err)

	pair := &tls.Certificate{Certificate: [][]byte{leafDER, rootDER}}
	err = ValidateTLSCertificate("chain-with-weak-root", pair)
	require.Error(t, err)
	require.Contains(t, err.Error(), "[1]")
}

func pemEncode(blockType string, der []byte) []byte {
	var buf bytes.Buffer
	_ = pem.Encode(&buf, &pem.Block{Type: blockType, Bytes: der})
	return buf.Bytes()
}

func writeTempFile(t *testing.T, pattern string, content []byte) string {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), pattern)
	require.NoError(t, err)
	_, err = f.Write(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}
