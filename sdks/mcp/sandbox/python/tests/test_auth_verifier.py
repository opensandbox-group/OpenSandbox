# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Security and cryptographic verification tests for JWTTokenVerifier and JWKSClient."""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import time
import warnings
from typing import Any

import httpx
import jwt
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ec, ed25519, rsa
from jwt.algorithms import ECAlgorithm, OKPAlgorithm, RSAAlgorithm

from opensandbox_mcp.auth import (
    MAX_BEARER_TOKEN_BYTES,
    MAX_JWKS_RESPONSE_BYTES,
    JWTTokenVerifier,
    OAuthConfig,
)

ISSUER = "https://idp.example.com"
RESOURCE = "https://mcp.example.com/mcp"
JWKS_URL = "https://idp.example.com/.well-known/jwks.json"


def _b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).decode("ascii").rstrip("=")


def _gen_rsa_jwk(
    kid: str, key_size: int = 2048
) -> tuple[rsa.RSAPrivateKey, dict[str, Any]]:
    private_key = rsa.generate_private_key(public_exponent=65537, key_size=key_size)
    jwk_dict = json.loads(RSAAlgorithm.to_jwk(private_key.public_key()))
    jwk_dict["kid"] = kid
    jwk_dict["use"] = "sig"
    jwk_dict["alg"] = "RS256"
    return private_key, jwk_dict


def _gen_ec_jwk(kid: str) -> tuple[ec.EllipticCurvePrivateKey, dict[str, Any]]:
    private_key = ec.generate_private_key(ec.SECP256R1())
    jwk_dict = json.loads(ECAlgorithm.to_jwk(private_key.public_key()))
    jwk_dict["kid"] = kid
    jwk_dict["use"] = "sig"
    jwk_dict["alg"] = "ES256"
    return private_key, jwk_dict


def _gen_ed25519_jwk(kid: str) -> tuple[ed25519.Ed25519PrivateKey, dict[str, Any]]:
    private_key = ed25519.Ed25519PrivateKey.generate()
    jwk_dict = json.loads(OKPAlgorithm.to_jwk(private_key.public_key()))
    jwk_dict["kid"] = kid
    jwk_dict["use"] = "sig"
    jwk_dict["alg"] = "EdDSA"
    return private_key, jwk_dict


def _sign(
    private_key: Any,
    *,
    alg: str = "RS256",
    headers: dict[str, Any] | None = None,
    **claim_overrides: Any,
) -> str:
    now = int(time.time())
    claims: dict[str, Any] = {
        "iss": ISSUER,
        "aud": RESOURCE,
        "sub": "user-123",
        "client_id": "chatgpt-client",
        "scope": "opensandbox:mcp",
        "iat": now - 10,
        "exp": now + 300,
    }
    claims.update(claim_overrides)
    return jwt.encode(claims, private_key, algorithm=alg, headers=headers)


def _config(**overrides: Any) -> OAuthConfig:
    params: dict[str, Any] = {
        "issuer_url": ISSUER,
        "resource_url": RESOURCE,
        "jwks_url": JWKS_URL,
        "required_scopes": ("opensandbox:mcp",),
        "clock_skew_seconds": 30.0,
        "jwks_cache_ttl_seconds": 300.0,
        "jwks_refresh_cooldown_seconds": 30.0,
    }
    params.update(overrides)
    return OAuthConfig(**params)


def _mock_client(handler) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


async def test_valid_rsa_ec_and_eddsa_tokens() -> None:
    rsa_key, rsa_jwk = _gen_rsa_jwk("rsa-1")
    ec_key, ec_jwk = _gen_ec_jwk("ec-1")
    ed_key, ed_jwk = _gen_ed25519_jwk("ed-1")

    client = _mock_client(
        lambda _req: httpx.Response(200, json={"keys": [rsa_jwk, ec_jwk, ed_jwk]})
    )
    verifier = JWTTokenVerifier(_config(), http_client=client)

    tok_rsa = _sign(rsa_key, alg="RS256", headers={"kid": "rsa-1", "typ": "at+jwt"})
    tok_ec = jwt.encode(
        {
            "iss": ISSUER,
            "aud": ["https://other.example.com", f"{RESOURCE}/"],
            "sub": "user-ec",
            "scp": ["opensandbox:mcp", "extra:scope"],
            "exp": int(time.time()) + 300,
        },
        ec_key,
        algorithm="ES256",
        headers={"kid": "ec-1", "typ": "JWT"},
    )
    tok_ed = jwt.encode(
        {
            "iss": ISSUER,
            "aud": RESOURCE,
            "sub": "user-ed",
            "azp": "azp-client",
            "scp": "opensandbox:mcp extra:read",
            "exp": int(time.time()) + 300,
        },
        ed_key,
        algorithm="EdDSA",
        headers={"kid": "ed-1"},
    )

    res_rsa = await verifier.verify_token(tok_rsa)
    assert res_rsa is not None
    assert res_rsa.client_id == "chatgpt-client"
    assert res_rsa.subject == "user-123"
    assert res_rsa.scopes == ["opensandbox:mcp"]
    assert res_rsa.resource == RESOURCE
    assert res_rsa.claims == {"iss": ISSUER}

    res_ec = await verifier.verify_token(tok_ec)
    assert res_ec is not None
    assert res_ec.client_id == "user-ec"
    assert res_ec.scopes == ["opensandbox:mcp", "extra:scope"]
    assert res_ec.resource == RESOURCE

    res_ed = await verifier.verify_token(tok_ed)
    assert res_ed is not None
    assert res_ed.client_id == "azp-client"
    assert res_ed.scopes == ["opensandbox:mcp", "extra:read"]


async def test_jwt_integrity_and_claim_rejections() -> None:
    good_key, good_jwk = _gen_rsa_jwk("kid-good")
    rogue_key, _ = _gen_rsa_jwk("kid-rogue")
    weak_key, weak_jwk = _gen_rsa_jwk("kid-weak", key_size=1024)
    enc_jwk = {**good_jwk, "kid": "kid-enc", "use": "enc"}
    no_verify_jwk = {**good_jwk, "kid": "kid-noverify", "key_ops": ["encrypt"]}

    client = _mock_client(
        lambda _req: httpx.Response(
            200,
            json={
                "keys": [
                    "not-a-dict",
                    {"kty": "oct", "k": "secret", "kid": "sym"},
                    {"kty": "RSA", "kid": "broken"},
                    enc_jwk,
                    no_verify_jwk,
                    weak_jwk,
                    good_jwk,
                ]
            },
        )
    )
    verifier = JWTTokenVerifier(_config(clock_skew_seconds=10.0), http_client=client)

    now = int(time.time())
    valid_token = _sign(good_key, headers={"kid": "kid-good"})
    h, p, s = valid_token.split(".")

    # 1. Tampered payload
    tampered_payload = _b64url(
        json.dumps(
            {
                "iss": ISSUER,
                "aud": RESOURCE,
                "sub": "admin",
                "scope": "opensandbox:mcp",
                "exp": now + 300,
            }
        ).encode()
    )
    tampered_token = f"{h}.{tampered_payload}.{s}"

    # 2. alg=none attack
    none_header = _b64url(json.dumps({"alg": "none", "typ": "JWT"}).encode())
    none_token = f"{none_header}.{p}."

    # 3. Raw HMAC-SHA256 algorithm confusion signed with RSA public key PEM
    pub_pem = good_key.public_key().public_bytes(
        encoding=serialization.Encoding.PEM,
        format=serialization.PublicFormat.SubjectPublicKeyInfo,
    )
    hs_header = _b64url(json.dumps({"alg": "HS256", "kid": "kid-good"}).encode())
    hs_input = f"{hs_header}.{p}".encode("ascii")
    hs_sig = _b64url(hmac.new(pub_pem, hs_input, hashlib.sha256).digest())
    hs256_token = f"{hs_header}.{p}.{hs_sig}"

    with warnings.catch_warnings():
        warnings.simplefilter("ignore")
        weak_token = _sign(weak_key, headers={"kid": "kid-weak"})

    bad_tokens = [
        "",
        "   ",
        "not-a-jwt",
        "a.b",
        "a.b.c.d",
        "!!!.@@@.###",
        "a." + ("x" * (MAX_BEARER_TOKEN_BYTES + 10)) + ".c",
        tampered_token,
        none_token,
        hs256_token,
        # Untrusted signing key claiming good kid
        _sign(rogue_key, headers={"kid": "kid-good"}),
        # Unknown / filtered kids (enc, non-verify, weak RSA-1024)
        _sign(good_key, headers={"kid": "kid-unknown"}),
        _sign(good_key, headers={"kid": "kid-enc"}),
        _sign(good_key, headers={"kid": "kid-noverify"}),
        weak_token,
        # Empty kid header
        _sign(good_key, headers={"kid": ""}),
        # Wrong issuer
        _sign(
            good_key,
            headers={"kid": "kid-good"},
            iss="https://evil-idp.example.com",
        ),
        # Wrong audience (token substitution) and malformed audience types
        _sign(
            good_key,
            headers={"kid": "kid-good"},
            aud="https://other-api.example.com/mcp",
        ),
        _sign(good_key, headers={"kid": "kid-good"}, aud=12345),
        # Expired beyond clock skew & future nbf beyond clock skew
        _sign(good_key, headers={"kid": "kid-good"}, exp=now - 30),
        _sign(good_key, headers={"kid": "kid-good"}, nbf=now + 60),
        # Forbidden header key-injection / SSRF vectors
        _sign(
            good_key,
            headers={"kid": "kid-good", "jku": "https://attacker.example/jwks.json"},
        ),
        _sign(
            good_key,
            headers={"kid": "kid-good", "x5u": "https://attacker.example/cert.pem"},
        ),
        _sign(good_key, headers={"kid": "kid-good", "jwk": good_jwk}),
        # ID token confusion (typ and OIDC ID-token claims)
        _sign(good_key, headers={"kid": "kid-good", "typ": "id+jwt"}),
        _sign(good_key, headers={"kid": "kid-good", "typ": "oauth-id-jag+jwt"}),
        _sign(good_key, headers={"kid": "kid-good"}, nonce="oidc-nonce-123"),
        _sign(good_key, headers={"kid": "kid-good"}, at_hash="abc123"),
        _sign(good_key, headers={"kid": "kid-good"}, c_hash="def456"),
        # Malformed claims (invalid scope/scp/iat/sub/client_id)
        _sign(good_key, headers={"kid": "kid-good"}, scope=123),
        _sign(good_key, headers={"kid": "kid-good"}, scp=456),
        _sign(good_key, headers={"kid": "kid-good"}, iat=True),
        _sign(good_key, headers={"kid": "kid-good"}, sub="   "),
        _sign(good_key, headers={"kid": "kid-good"}, client_id=""),
        # Missing required claims (no exp, no aud, no sub/client_id)
        jwt.encode(
            {"iss": ISSUER, "aud": RESOURCE, "sub": "u"},
            good_key,
            algorithm="RS256",
            headers={"kid": "kid-good"},
        ),
        jwt.encode(
            {"iss": ISSUER, "exp": now + 300, "sub": "u"},
            good_key,
            algorithm="RS256",
            headers={"kid": "kid-good"},
        ),
        jwt.encode(
            {"iss": ISSUER, "aud": RESOURCE, "exp": now + 300},
            good_key,
            algorithm="RS256",
            headers={"kid": "kid-good"},
        ),
    ]

    for bad in bad_tokens:
        assert await verifier.verify_token(bad) is None, (
            f"Unexpectedly accepted token: {bad[:40]!r}"
        )


async def test_clock_skew_boundary() -> None:
    key, jwk_dict = _gen_rsa_jwk("kid-skew")
    client = _mock_client(lambda _req: httpx.Response(200, json={"keys": [jwk_dict]}))
    verifier = JWTTokenVerifier(_config(clock_skew_seconds=20.0), http_client=client)

    now = int(time.time())
    within_exp_skew = _sign(key, headers={"kid": "kid-skew"}, exp=now - 5)
    beyond_exp_skew = _sign(key, headers={"kid": "kid-skew"}, exp=now - 25)
    within_nbf_skew = _sign(key, headers={"kid": "kid-skew"}, nbf=now + 5)
    beyond_nbf_skew = _sign(key, headers={"kid": "kid-skew"}, nbf=now + 25)

    res = await verifier.verify_token(within_exp_skew)
    assert res is not None
    assert res.expires_at is not None and res.expires_at >= now
    assert await verifier.verify_token(beyond_exp_skew) is None
    assert await verifier.verify_token(within_nbf_skew) is not None
    assert await verifier.verify_token(beyond_nbf_skew) is None


async def test_key_rotation_cooldown_cache_expiry_and_jwks_outage() -> None:
    key1, jwk1 = _gen_rsa_jwk("kid-1")
    key2, jwk2 = _gen_rsa_jwk("kid-2")

    current_time = [1000.0]
    jwks_state: dict[str, Any] = {"status": 200, "keys": [jwk1], "fetches": 0}

    def handler(_req: httpx.Request) -> httpx.Response:
        jwks_state["fetches"] += 1
        if jwks_state["status"] != 200:
            return httpx.Response(jwks_state["status"])
        return httpx.Response(200, json={"keys": jwks_state["keys"]})

    verifier = JWTTokenVerifier(
        _config(jwks_cache_ttl_seconds=100.0, jwks_refresh_cooldown_seconds=30.0),
        http_client=_mock_client(handler),
        clock=lambda: current_time[0],
    )

    tok1 = _sign(key1, headers={"kid": "kid-1"})
    tok2 = _sign(key2, headers={"kid": "kid-2"})

    assert await verifier.verify_token(tok1) is not None
    assert jwks_state["fetches"] == 1

    # Inside cooldown -> forced refresh throttled
    jwks_state["keys"] = [jwk1, jwk2]
    current_time[0] += 10.0
    assert await verifier.verify_token(tok2) is None
    assert jwks_state["fetches"] == 1

    # After cooldown -> unknown kid triggers single forced refresh -> succeeds
    current_time[0] += 25.0
    assert await verifier.verify_token(tok2) is not None
    assert jwks_state["fetches"] == 2

    # Unexpired cache hit
    assert await verifier.verify_token(tok2) is not None
    assert jwks_state["fetches"] == 2

    # JWKS fails after cache TTL expires -> fail closed (never reuse stale keys)
    jwks_state["status"] = 503
    current_time[0] += 120.0
    assert await verifier.verify_token(tok2) is None
    assert jwks_state["fetches"] == 3


async def test_issuer_discovery_path_handling_and_ssrf_defenses() -> None:
    key, jwk_dict = _gen_rsa_jwk("kid-disc")
    path_issuer = "https://idp.example.com/realms/demo"
    path_jwks = "https://idp.example.com/realms/demo/protocol/openid-connect/certs"
    tok_path = _sign(key, headers={"kid": "kid-disc"}, iss=path_issuer)
    tok = _sign(key, headers={"kid": "kid-disc"})

    # 1. RFC 8414 path insertion succeeds
    seen_paths: list[str] = []

    def rfc8414_path_handler(req: httpx.Request) -> httpx.Response:
        seen_paths.append(req.url.path)
        if req.url.path == "/.well-known/oauth-authorization-server/realms/demo":
            return httpx.Response(
                200, json={"issuer": path_issuer, "jwks_uri": path_jwks}
            )
        if req.url.path == "/realms/demo/protocol/openid-connect/certs":
            return httpx.Response(200, json={"keys": [jwk_dict]})
        return httpx.Response(404)

    v_path = JWTTokenVerifier(
        _config(issuer_url=path_issuer, jwks_url=None),
        http_client=_mock_client(rfc8414_path_handler),
    )
    assert await v_path.verify_token(tok_path) is not None
    assert seen_paths[0] == "/.well-known/oauth-authorization-server/realms/demo"

    # 2. RFC 8414 returns 404 -> falls back to OIDC discovery
    def oidc_fallback_handler(req: httpx.Request) -> httpx.Response:
        if req.url.path == "/.well-known/oauth-authorization-server":
            return httpx.Response(404)
        if req.url.path == "/.well-known/openid-configuration":
            return httpx.Response(200, json={"issuer": ISSUER, "jwks_uri": JWKS_URL})
        if req.url.path == "/.well-known/jwks.json":
            return httpx.Response(200, json={"keys": [jwk_dict]})
        return httpx.Response(404)

    v_oidc = JWTTokenVerifier(
        _config(jwks_url=None),
        http_client=_mock_client(oidc_fallback_handler),
    )
    assert await v_oidc.verify_token(tok) is not None

    # 3. Malicious / broken discovery & JWKS responses fail closed
    failing_handlers = [
        # Cross-origin jwks_uri SSRF
        lambda _r: httpx.Response(
            200,
            json={
                "issuer": ISSUER,
                "jwks_uri": "https://169.254.169.254/latest/meta-data",
            },
        ),
        # Issuer mismatch
        lambda _r: httpx.Response(
            200, json={"issuer": "https://other.example.com", "jwks_uri": JWKS_URL}
        ),
        # Missing jwks_uri
        lambda _r: httpx.Response(200, json={"issuer": ISSUER}),
        # Non-HTTPS jwks_uri
        lambda _r: httpx.Response(
            200, json={"issuer": ISSUER, "jwks_uri": "http://idp.example.com/jwks"}
        ),
        # All discovery endpoints 404
        lambda _r: httpx.Response(404),
    ]
    for handler in failing_handlers:
        v = JWTTokenVerifier(_config(jwks_url=None), http_client=_mock_client(handler))
        assert await v.verify_token(tok) is None

    # 4. Direct JWKS failures: redirect, timeout, bad JSON, non-object JSON, empty keys, oversized
    jwks_failures = [
        lambda _r: httpx.Response(302, headers={"Location": "https://evil.example/k"}),
        lambda _r: (_ for _ in ()).throw(httpx.ReadTimeout("timed out")),
        lambda _r: httpx.Response(200, content=b"not-json"),
        lambda _r: httpx.Response(200, json=["not", "an", "object"]),
        lambda _r: httpx.Response(200, json={"keys": []}),
        lambda _r: httpx.Response(200, content=b"x" * (MAX_JWKS_RESPONSE_BYTES + 1)),
    ]
    for handler in jwks_failures:
        v = JWTTokenVerifier(_config(), http_client=_mock_client(handler))
        assert await v.verify_token(tok) is None


async def test_custom_non_url_audience_keyless_ambiguity_and_aclose() -> None:
    key1, jwk1 = _gen_rsa_jwk("kid-a")
    _, jwk2 = _gen_rsa_jwk("kid-b")

    # Multi-key JWKS rejects JWT without kid (ambiguity)
    v_multi = JWTTokenVerifier(
        _config(audience="api://opensandbox-mcp"),
        http_client=_mock_client(
            lambda _req: httpx.Response(200, json={"keys": [jwk1, jwk2]})
        ),
    )
    tok_no_kid = _sign(key1, aud="api://opensandbox-mcp")
    assert await v_multi.verify_token(tok_no_kid) is None

    # Single-key JWKS accepts JWT without kid and custom non-URL audience
    v_single = JWTTokenVerifier(
        _config(audience="api://opensandbox-mcp"),
        http_client=_mock_client(
            lambda _req: httpx.Response(200, json={"keys": [jwk1]})
        ),
    )
    res = await v_single.verify_token(tok_no_kid)
    assert res is not None
    assert res.resource == "api://opensandbox-mcp"

    # Owned client closes cleanly
    owned = JWTTokenVerifier(_config())
    await owned.aclose()
