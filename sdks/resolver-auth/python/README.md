# opensandbox-resolver-auth

Resolver-side verification SDK for the [OpenSandbox](https://open-sandbox.ai) credential-vault
resolver caller authentication ([OSEP-0025](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0025-credential-vault-provider-spi.md)).

The OpenSandbox egress attaches one `OpenSandbox-Egress-Credential` header — an Ed25519 JWT —
to every resolve call it makes over TCP. This library verifies that credential offline against
the JWKS published by the lifecycle server, so a resolver can prove "this call came from the
egress" and bind it to the attested sandbox subject without any shared secret.

## Install

```bash
pip install opensandbox-resolver-auth
```

Requires Python 3.10+.

## Quick start

```python
from opensandbox_resolver_auth import CredentialVaultCallerAuth, VerifyError

caller_auth = CredentialVaultCallerAuth(
    jwks_base="https://lifecycle-server.example.com/credential-vault",
    expected_issuer="opensandbox",
    expected_audience="https://resolver.internal:8443",  # your own origin
)

def handle_resolve(document, headers):
    try:
        subject = caller_auth.verify_caller(
            headers["OpenSandbox-Egress-Credential"],
            document_subject=document["subject"],
        )
    except VerifyError as exc:
        return 401, {"error": exc.code}
    # `subject` is the egress-attested sandboxId — the sole authoritative
    # identity for per-sandbox authorization.
    ...
```

One instance per resolver process (it holds the bounded JWKS caches); one
`verify_caller` call per resolve request. Every `VerifyError` maps to HTTP
401; unknown kids are fetched from `{jwks_base}/jwks/{kid}` with bounded
positive/negative caching. See the package docstrings for the exact claim
rules and the [credential vault guide](https://open-sandbox.ai) for the
full contract.
