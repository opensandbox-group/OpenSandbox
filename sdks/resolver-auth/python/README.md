# opensandbox-resolver-auth

OpenSandbox Resolver Caller Authentication SDK (Python) — resolver-side offline verification of egress caller credentials ([OSEP-0025](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0025-credential-vault-provider-spi.md)).

Every `https://` resolve dial carries an Ed25519 JWT (`OpenSandbox-Egress-Credential`) minted by the egress. This SDK verifies it against a server-published JWKS endpoint so a resolver closes the R4 trust boundary in a few lines: no secrets are distributed to resolvers, and key rotation is revocation.

## Installation

```bash
pip install opensandbox-resolver-auth
```

## Quick start

```python
from opensandbox_resolver_auth import (
    CredentialVerifier,
    JwksClient,
    VerificationError,
)

verifier = CredentialVerifier(
    key_provider=JwksClient(jwks_base="https://lifecycle-server.example"),
    expected_issuer="opensandbox",
    expected_aud="credential-resolver@resolver.internal",  # this resolver's own origin
)

# In the request handler for POST /resolve:
try:
    verified = verifier.verify(
        request.headers.get("OpenSandbox-Egress-Credential"),
        subject=document["subject"],
    )
except VerificationError as error:
    respond(401, {"error": error.reason})
    # reason: credential_missing | credential_invalid | subject_mismatch
```

Configuration is exactly three values: `jwks_base`, `expected_issuer`, and `expected_aud`. Verification is offline (JWKS keys are cached and re-validated at most hourly), fail-closed, and thread-safe. TLS certificate verification on the JWKS fetch is always on and cannot be disabled; an optional `cafile` may pin a platform CA.

## Verification rules

- `alg` pinned to `EdDSA`; header whitelist `alg`/`kid`/`typ` — `jku`, `x5u`, and embedded `jwk` are rejected on sight.
- `kid` must match the platform identifier grammar (sidecar sandboxId or fast-sandbox `<pool>:<version>`); off-grammar kids are rejected locally with no JWKS fetch.
- `iss` must equal the configured issuer; `aud` must equal this resolver's own origin; `exp - iat ≤ 3600` with 30s leeway; signed `sub` must equal the resolve document's `subject`.

Deliberate omissions per OSEP-0025: no `nbf` check, no `scope`/`role`/tenant claims, no `jti` deduplication. Unknown payload claims are ignored.

## Development

```bash
uv sync
uv run ruff check
uv run pyright
uv run pytest
```

## License

Apache-2.0 — see [LICENSE](LICENSE).
