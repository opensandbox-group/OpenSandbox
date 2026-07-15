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

package opensandbox

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
)

const (
	nistMinRSABits  = 2048
	nistMinECBits   = 224
	nistMinHashBits = 224
)

func minHashBitsForSignatureAlgorithm(algo x509.SignatureAlgorithm) (int, error) {
	switch algo {
	case x509.MD2WithRSA, x509.MD5WithRSA:
		return 128, nil
	case x509.SHA1WithRSA, x509.ECDSAWithSHA1:
		return 160, nil
	case x509.SHA256WithRSA, x509.ECDSAWithSHA256:
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

func enforceNISTPeerCertificateMinimums(cs tls.ConnectionState) error {
	if len(cs.VerifiedChains) == 0 {
		return fmt.Errorf("server did not present a verified certificate chain")
	}
	for i, chain := range cs.VerifiedChains {
		for j, cert := range chain {
			if err := ensureCertPublicKeyMeetsNISTMinimums(cert); err != nil {
				return fmt.Errorf("verified chain[%d] certificate[%d]: %w", i, j, err)
			}
			// The final certificate is the trust anchor. Its own signature hash
			// is not authenticated by the TLS chain, but its public key still is.
			if len(chain) > 1 && j == len(chain)-1 {
				continue
			}
			if err := ensureCertSignatureHashMeetsNISTMinimums(cert); err != nil {
				return fmt.Errorf("verified chain[%d] certificate[%d]: %w", i, j, err)
			}
		}
	}
	return nil
}
