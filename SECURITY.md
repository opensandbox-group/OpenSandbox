# Security Policy

## Reporting Security Issues

The OpenSandbox team takes security seriously. If you discover a security vulnerability, please report it responsibly.

### How to Report

- **GitHub Security Advisories**: Open a private security advisory on GitHub
- **Email**: Contact the maintainers directly with "[SECURITY]" in the subject

### What to Include

- Clear description of the vulnerability
- Steps to reproduce
- Potential impact and scope
- Suggested remediation (if available)

## Response Process

1. Acknowledgment within 48 hours
2. Investigation and validation
3. Fix development and testing
4. Coordinated disclosure

## Supported Versions

Only the latest release and main branch are actively supported with security updates.

## Release Signatures

OpenSandbox signs public release outputs with GitHub/Sigstore attestations,
cosign keyless container signatures, and Maven Central package signatures where
applicable. See [Release Verification](docs/release-verification.md) for the
trusted signer identities and verification commands.

## Security Best Practices

When deploying OpenSandbox:
- Keep dependencies up to date
- Use network policies to restrict sandbox egress
- Monitor audit logs regularly
- Follow principle of least privilege

## Cryptographic Key Length Policy

OpenSandbox TLS defaults are aligned with OpenSSF `crypto_keylength` guidance
(NIST minimum strength through year 2030, as stated in 2012):

- symmetric key: at least 112 bits
- factoring modulus (RSA): at least 2048 bits
- discrete logarithm key: at least 224 bits
- discrete logarithm group: at least 2048 bits
- elliptic curve key: at least 224 bits
- hash algorithm strength: at least 224 bits

Project-owned enforcement points include:

- Go SDK default transport certificate validation
- Kubernetes controller validation for configured webhook/metrics TLS certificates

For controlled interoperability scenarios, legacy weaker key lengths can be explicitly enabled:

- Go SDK: set `TransportConfig.AllowWeakServerCertKeyLengths=true`
- Kubernetes controller: set `--allow-weak-tls-keylengths=true`
