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
	"crypto/dsa"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
)

const (
	nistMinRSABits     = 2048
	nistMinDLKeyBits   = 224
	nistMinDLGroupBits = 2048
	nistMinECBits      = 224
	nistMinHashBits    = 224
)

func minHashBitsForSignatureAlgorithm(algo x509.SignatureAlgorithm) (int, error) {
	switch algo {
	case x509.MD2WithRSA, x509.MD5WithRSA:
		return 128, nil
	case x509.SHA1WithRSA, x509.DSAWithSHA1, x509.ECDSAWithSHA1:
		return 160, nil
	case x509.DSAWithSHA256, x509.SHA256WithRSA, x509.ECDSAWithSHA256:
		return 256, nil
	case x509.SHA384WithRSA, x509.ECDSAWithSHA384:
		return 384, nil
	case x509.SHA512WithRSA, x509.ECDSAWithSHA512:
		return 512, nil
	case x509.SHA256WithRSAPSS:
		return 256, nil
	case x509.SHA384WithRSAPSS:
		return 384, nil
	case x509.SHA512WithRSAPSS:
		return 512, nil
	case x509.PureEd25519:
		return 256, nil
	default:
		return 0, fmt.Errorf("unknown certificate signature algorithm: %s", algo.String())
	}
}

func ensureCertSignatureHashMeetsNISTMinimums(cert *x509.Certificate) error {
	hashBits, err := minHashBitsForSignatureAlgorithm(cert.SignatureAlgorithm)
	if err != nil {
		return err
	}
	if hashBits < nistMinHashBits {
		return fmt.Errorf(
			"certificate hash strength %d bits is below NIST minimum %d bits (signature algorithm: %s)",
			hashBits,
			nistMinHashBits,
			cert.SignatureAlgorithm.String(),
		)
	}
	return nil
}

func ensureCertPublicKeyMeetsNISTMinimums(cert *x509.Certificate) error {
	if cert == nil {
		return fmt.Errorf("certificate is nil")
	}

	switch pub := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		if pub.N == nil {
			return fmt.Errorf("certificate RSA public key modulus is nil")
		}
		bits := pub.N.BitLen()
		if bits < nistMinRSABits {
			return fmt.Errorf(
				"certificate RSA key length %d bits is below NIST minimum %d bits",
				bits,
				nistMinRSABits,
			)
		}
	case *ecdsa.PublicKey:
		if pub.Curve == nil {
			return fmt.Errorf("certificate EC public key curve is nil")
		}
		bits := pub.Curve.Params().BitSize
		if bits < nistMinECBits {
			return fmt.Errorf(
				"certificate EC key length %d bits is below NIST minimum %d bits",
				bits,
				nistMinECBits,
			)
		}
	case *dsa.PublicKey:
		if pub.Parameters.P == nil || pub.Parameters.Q == nil {
			return fmt.Errorf("certificate DSA public key parameters are incomplete")
		}
		subgroupBits := pub.Parameters.Q.BitLen()
		groupBits := pub.Parameters.P.BitLen()
		if subgroupBits < nistMinDLKeyBits {
			return fmt.Errorf(
				"certificate DSA subgroup (Q) length %d bits is below NIST minimum %d bits",
				subgroupBits,
				nistMinDLKeyBits,
			)
		}
		if groupBits < nistMinDLGroupBits {
			return fmt.Errorf(
				"certificate DSA group (P) length %d bits is below NIST minimum %d bits",
				groupBits,
				nistMinDLGroupBits,
			)
		}
	case ed25519.PublicKey:
		bits := len(pub) * 8
		if bits < nistMinECBits {
			return fmt.Errorf(
				"certificate Ed25519 key length %d bits is below NIST minimum %d bits",
				bits,
				nistMinECBits,
			)
		}
	default:
		return fmt.Errorf("unsupported certificate public key type %T", cert.PublicKey)
	}

	return nil
}

func ensureCertMeetsNISTMinimums(cert *x509.Certificate) error {
	if cert == nil {
		return fmt.Errorf("certificate is nil")
	}
	if err := ensureCertPublicKeyMeetsNISTMinimums(cert); err != nil {
		return err
	}
	return ensureCertSignatureHashMeetsNISTMinimums(cert)
}

func isSelfSignedCA(cert *x509.Certificate) bool {
	return cert != nil && cert.IsCA &&
		bytes.Equal(cert.RawSubject, cert.RawIssuer) &&
		cert.CheckSignatureFrom(cert) == nil
}

// ValidateCertificateKeyPair loads and validates a TLS certificate/key pair.
func ValidateCertificateKeyPair(certFile, keyFile string) error {
	certPair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("load TLS key pair (%s, %s): %w", certFile, keyFile, err)
	}
	return ValidateTLSCertificate(certFile, &certPair)
}

// ValidateTLSCertificate checks the certificate chain against NIST minimum key/hash requirements.
func ValidateTLSCertificate(certName string, certPair *tls.Certificate) error {
	if certPair == nil {
		return fmt.Errorf("TLS certificate is nil for %s", certName)
	}
	if len(certPair.Certificate) == 0 {
		return fmt.Errorf("TLS certificate chain is empty for %s", certName)
	}
	lastIdx := len(certPair.Certificate) - 1
	for i, rawCert := range certPair.Certificate {
		var cert *x509.Certificate
		if i == 0 && certPair.Leaf != nil {
			cert = certPair.Leaf
		} else {
			var err error
			cert, err = x509.ParseCertificate(rawCert)
			if err != nil {
				return fmt.Errorf("parse TLS certificate %s[%d]: %w", certName, i, err)
			}
		}
		if err := ensureCertPublicKeyMeetsNISTMinimums(cert); err != nil {
			return fmt.Errorf("certificate %s[%d]: %w", certName, i, err)
		}
		// If a root CA is included, its self-signature is not part of the served
		// authentication path. Its public key length is still checked above.
		if i == lastIdx && lastIdx > 0 && isSelfSignedCA(cert) {
			continue
		}
		if err := ensureCertSignatureHashMeetsNISTMinimums(cert); err != nil {
			return fmt.Errorf("certificate %s[%d]: %w", certName, i, err)
		}
	}
	return nil
}
