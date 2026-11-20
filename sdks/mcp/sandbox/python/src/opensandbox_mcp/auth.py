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

"""Optional OAuth 2.1 Resource Server support for OpenSandbox MCP (Streamable HTTP)."""

from __future__ import annotations

import asyncio
import json
import logging
import math
import os
import re
import time
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from typing import Any, Literal
from urllib.parse import parse_qsl, urlparse

import httpx
import jwt
from cryptography.hazmat.primitives.asymmetric import ec, ed448, ed25519, rsa
from jwt import InvalidTokenError, PyJWK
from mcp.server.auth.provider import AccessToken, TokenVerifier
from mcp.server.auth.routes import build_resource_metadata_url
from mcp.server.auth.settings import AuthSettings
from mcp.server.transport_security import TransportSecuritySettings
from pydantic import AnyHttpUrl
from starlette.types import ASGIApp, Receive, Scope, Send

logger = logging.getLogger(__name__)

OAuthMode = Literal["none", "jwt"]

SUPPORTED_ASYMMETRIC_ALGORITHMS: frozenset[str] = frozenset(
    {
        "RS256",
        "RS384",
        "RS512",
        "PS256",
        "PS384",
        "PS512",
        "ES256",
        "ES384",
        "ES512",
        "EdDSA",
    }
)

DEFAULT_ALLOWED_ALGORITHMS: tuple[str, ...] = (
    "RS256",
    "RS384",
    "RS512",
    "PS256",
    "PS384",
    "PS512",
    "ES256",
    "ES384",
    "ES512",
    "EdDSA",
)

ALLOWED_TOKEN_TYPES: frozenset[str] = frozenset({"jwt", "at+jwt", "application/at+jwt"})
FORBIDDEN_HEADER_PARAMS: frozenset[str] = frozenset({"jku", "x5u", "jwk"})
ID_TOKEN_ONLY_CLAIMS: frozenset[str] = frozenset({"nonce", "at_hash", "c_hash"})

MAX_BEARER_TOKEN_BYTES = 16_384
MAX_AUTHORIZATION_HEADER_BYTES = 16_500
MAX_JWKS_RESPONSE_BYTES = 2 * 1024 * 1024

MIN_RSA_KEY_BITS = 2048
MIN_EC_KEY_BITS = 224

LOOPBACK_HOSTS: frozenset[str] = frozenset({"127.0.0.1", "localhost", "::1"})

ENV_PREFIX = "OPEN_SANDBOX_MCP_OAUTH_"
ENV_MODE = f"{ENV_PREFIX}MODE"
ENV_ISSUER_URL = f"{ENV_PREFIX}ISSUER_URL"
ENV_RESOURCE_URL = f"{ENV_PREFIX}RESOURCE_URL"
ENV_AUDIENCE = f"{ENV_PREFIX}AUDIENCE"
ENV_JWKS_URL = f"{ENV_PREFIX}JWKS_URL"
ENV_REQUIRED_SCOPES = f"{ENV_PREFIX}REQUIRED_SCOPES"
ENV_ALLOWED_ALGS = f"{ENV_PREFIX}ALLOWED_ALGS"
ENV_CLOCK_SKEW_SECONDS = f"{ENV_PREFIX}CLOCK_SKEW_SECONDS"
ENV_HTTP_TIMEOUT_SECONDS = f"{ENV_PREFIX}HTTP_TIMEOUT_SECONDS"
ENV_JWKS_CACHE_TTL_SECONDS = f"{ENV_PREFIX}JWKS_CACHE_TTL_SECONDS"
ENV_JWKS_REFRESH_COOLDOWN_SECONDS = f"{ENV_PREFIX}JWKS_REFRESH_COOLDOWN_SECONDS"
ENV_ALLOWED_HOSTS = f"{ENV_PREFIX}ALLOWED_HOSTS"
ENV_ALLOW_INSECURE_HTTP = f"{ENV_PREFIX}ALLOW_INSECURE_HTTP"

_ALL_OAUTH_SETTING_ENV_VARS: tuple[str, ...] = (
    ENV_ISSUER_URL,
    ENV_RESOURCE_URL,
    ENV_AUDIENCE,
    ENV_JWKS_URL,
    ENV_REQUIRED_SCOPES,
    ENV_ALLOWED_ALGS,
    ENV_CLOCK_SKEW_SECONDS,
    ENV_HTTP_TIMEOUT_SECONDS,
    ENV_JWKS_CACHE_TTL_SECONDS,
    ENV_JWKS_REFRESH_COOLDOWN_SECONDS,
    ENV_ALLOWED_HOSTS,
    ENV_ALLOW_INSECURE_HTTP,
)

_SCOPE_TOKEN = re.compile(r"[\x21\x23-\x5b\x5d-\x7e]+")
_WHITESPACE_RE = re.compile(r"\s")
_TOKEN_DELIMITER_RE = re.compile(r"[\s,]+")

_ALG_TO_KTY: dict[str, str] = {
    "RS256": "RSA",
    "RS384": "RSA",
    "RS512": "RSA",
    "PS256": "RSA",
    "PS384": "RSA",
    "PS512": "RSA",
    "ES256": "EC",
    "ES384": "EC",
    "ES512": "EC",
    "EdDSA": "OKP",
}


class OAuthConfigError(ValueError):
    """Raised when OAuth Resource Server configuration is invalid or incomplete."""


class _JWKSError(RuntimeError):
    """Internal error raised when JWKS metadata or keys cannot be verified."""


def _is_loopback_host(hostname: str | None) -> bool:
    return hostname is not None and hostname.lower() in LOOPBACK_HOSTS


def _validate_http_url(
    url: str,
    field_name: str,
    *,
    allow_insecure_http: bool,
) -> str:
    if not isinstance(url, str) or not url or url != url.strip():
        raise OAuthConfigError(f"{field_name} must be a non-empty URL without padding.")
    try:
        parsed = urlparse(url)
        outbound = httpx.URL(url)
        AnyHttpUrl(url)
    except (ValueError, httpx.InvalidURL) as exc:
        raise OAuthConfigError(f"{field_name} is not a valid HTTP(S) URL.") from exc
    if "\\" in url or any(ord(c) < 33 or ord(c) == 127 for c in url):
        raise OAuthConfigError(f"{field_name} contains invalid URL characters.")
    scheme = parsed.scheme.lower()
    if scheme not in ("https", "http"):
        raise OAuthConfigError(f"{field_name} must use https:// (or loopback http://).")
    if not parsed.hostname or not parsed.netloc:
        raise OAuthConfigError(f"{field_name} must include a valid host.")
    if parsed.username is not None or parsed.password is not None:
        raise OAuthConfigError(f"{field_name} must not contain embedded credentials.")
    if parsed.query:
        raise OAuthConfigError(f"{field_name} must not contain a query string.")
    if parsed.fragment:
        raise OAuthConfigError(f"{field_name} must not contain a URL fragment.")
    if scheme == "http":
        if not allow_insecure_http or not _is_loopback_host(outbound.host):
            raise OAuthConfigError(
                f"{field_name} must use https:// "
                "(http:// is only allowed on loopback hosts with "
                "--oauth-allow-insecure-http)."
            )
    return url


def _origin_tuple(url: str) -> tuple[str, str, int | None]:
    parsed = httpx.URL(url)
    return parsed.scheme, parsed.host, parsed.port


def _split_tokens(values: Sequence[str] | str | None) -> tuple[str, ...]:
    if values is None:
        return ()
    raw_items = [values] if isinstance(values, str) else list(values)
    result: list[str] = []
    seen: set[str] = set()
    for item in raw_items:
        for part in _TOKEN_DELIMITER_RE.split(item.strip()):
            if part and part not in seen:
                seen.add(part)
                result.append(part)
    return tuple(result)


def _parse_bool_env(raw: str, name: str) -> bool:
    normalized = raw.strip().lower()
    if normalized in ("1", "true", "yes", "on"):
        return True
    if normalized in ("0", "false", "no", "off"):
        return False
    raise OAuthConfigError(f"Invalid boolean value for {name}.")


def _parse_float(raw: str | float, name: str) -> float:
    if isinstance(raw, bool):
        raise OAuthConfigError(f"{name} must be a number.")
    try:
        value = float(raw)
    except (TypeError, ValueError) as exc:
        raise OAuthConfigError(f"{name} must be a valid number.") from exc
    if not math.isfinite(value):
        raise OAuthConfigError(f"{name} must be a finite number.")
    return value


@dataclass(frozen=True)
class OAuthConfig:
    """Validated OAuth 2.1 Resource Server configuration for Streamable HTTP.

    Attributes:
        issuer_url: Exact trusted Authorization Server issuer URL.
        resource_url: Canonical public URL of the MCP HTTP endpoint (RFC 8707/9728).
        required_scopes: Scopes required on every protected MCP HTTP request.
        audience: Optional expected JWT ``aud`` claim (defaults to ``resource_url``).
        jwks_url: Optional explicit JWKS URL; discovered from ``issuer_url`` if omitted.
        allowed_algorithms: Asymmetric JWS algorithms accepted for token verification.
        clock_skew_seconds: Clock-skew leeway in seconds for ``exp`` and ``nbf``.
        http_timeout_seconds: Timeout in seconds for outbound metadata/JWKS requests.
        jwks_cache_ttl_seconds: Maximum lifetime in seconds for cached JWKS keys.
        jwks_refresh_cooldown_seconds: Minimum interval between forced refreshes on unknown ``kid``.
        allowed_hosts: Optional additional Host header values allowed by DNS rebinding checks.
        allow_insecure_http: Permit ``http://`` strictly on loopback hosts for local testing.
    """

    issuer_url: str
    resource_url: str
    required_scopes: tuple[str, ...]
    audience: str | None = None
    jwks_url: str | None = None
    allowed_algorithms: tuple[str, ...] = DEFAULT_ALLOWED_ALGORITHMS
    clock_skew_seconds: float = 30.0
    http_timeout_seconds: float = 5.0
    jwks_cache_ttl_seconds: float = 300.0
    jwks_refresh_cooldown_seconds: float = 30.0
    allowed_hosts: tuple[str, ...] = ()
    allow_insecure_http: bool = False

    def __post_init__(self) -> None:
        _validate_http_url(
            self.issuer_url,
            "issuer_url",
            allow_insecure_http=self.allow_insecure_http,
        )
        _validate_http_url(
            self.resource_url,
            "resource_url",
            allow_insecure_http=self.allow_insecure_http,
        )
        if self.jwks_url is not None:
            _validate_http_url(
                self.jwks_url,
                "jwks_url",
                allow_insecure_http=self.allow_insecure_http,
            )

        if not self.required_scopes:
            raise OAuthConfigError(
                "At least one required scope must be configured when OAuth is enabled."
            )
        for scope in self.required_scopes:
            if not _SCOPE_TOKEN.fullmatch(scope):
                raise OAuthConfigError(
                    "required_scopes entries must be valid ASCII OAuth scope tokens without whitespace."
                )

        if self.audience is not None:
            if not self.audience or self.audience != self.audience.strip():
                raise OAuthConfigError(
                    "audience must be a non-empty string without surrounding whitespace."
                )

        if not self.allowed_algorithms:
            raise OAuthConfigError("allowed_algorithms must not be empty.")
        unsupported = [
            alg
            for alg in self.allowed_algorithms
            if alg not in SUPPORTED_ASYMMETRIC_ALGORITHMS
        ]
        if unsupported:
            raise OAuthConfigError(
                f"Unsupported or insecure JWT algorithm(s): {', '.join(unsupported)}. "
                f"Allowed asymmetric algorithms: "
                f"{', '.join(sorted(SUPPORTED_ASYMMETRIC_ALGORITHMS))}."
            )

        if not (
            math.isfinite(self.clock_skew_seconds)
            and 0.0 <= self.clock_skew_seconds <= 300.0
        ):
            raise OAuthConfigError("clock_skew_seconds must be between 0 and 300.")
        if not (
            math.isfinite(self.http_timeout_seconds)
            and 0.0 < self.http_timeout_seconds <= 60.0
        ):
            raise OAuthConfigError(
                "http_timeout_seconds must be greater than 0 and at most 60."
            )
        if not (
            math.isfinite(self.jwks_cache_ttl_seconds)
            and 0.0 < self.jwks_cache_ttl_seconds <= 86_400.0
        ):
            raise OAuthConfigError(
                "jwks_cache_ttl_seconds must be greater than 0 and at most 86400."
            )
        if not (
            math.isfinite(self.jwks_refresh_cooldown_seconds)
            and 0.0 <= self.jwks_refresh_cooldown_seconds <= 3600.0
        ):
            raise OAuthConfigError(
                "jwks_refresh_cooldown_seconds must be between 0 and 3600."
            )

        for host in self.allowed_hosts:
            if not host or _WHITESPACE_RE.search(host) or "://" in host or "/" in host:
                raise OAuthConfigError(
                    f"Invalid allowed_hosts entry {host!r}: expected host or host:port."
                )

    @property
    def expected_audience(self) -> str:
        """Expected JWT ``aud`` claim value."""
        return self.audience if self.audience is not None else self.resource_url

    @property
    def uses_url_resource_audience(self) -> bool:
        """Whether expected audience matches the canonical URL ``resource_url``."""
        return self.expected_audience == self.resource_url


def load_oauth_config(
    *,
    transport: str,
    mode: str | None = None,
    issuer_url: str | None = None,
    resource_url: str | None = None,
    audience: str | None = None,
    jwks_url: str | None = None,
    required_scopes: Sequence[str] | None = None,
    allowed_algorithms: str | Sequence[str] | None = None,
    clock_skew_seconds: float | None = None,
    http_timeout_seconds: float | None = None,
    jwks_cache_ttl_seconds: float | None = None,
    jwks_refresh_cooldown_seconds: float | None = None,
    allowed_hosts: Sequence[str] | None = None,
    allow_insecure_http: bool | None = None,
    env: Mapping[str, str] | None = None,
) -> OAuthConfig | None:
    """Resolve and validate OAuth configuration from CLI options and environment.

    Fails closed when OAuth settings are partially specified, when ``mode`` is
    invalid, or when OAuth is enabled for a non-HTTP transport.

    Returns:
        A validated ``OAuthConfig`` when OAuth is enabled, or ``None`` when
        ``mode`` is ``"none"`` and no OAuth settings are present.

    Raises:
        OAuthConfigError: If any OAuth setting is invalid, inconsistent, or missing.
    """
    environ = os.environ if env is None else env

    raw_mode = mode if mode is not None else environ.get(ENV_MODE, "none")
    normalized_mode = raw_mode.strip().lower()
    if normalized_mode not in ("none", "jwt"):
        raise OAuthConfigError(
            f"Unsupported OAuth mode {raw_mode!r}; expected 'none' or 'jwt'."
        )

    cli_overrides_present = any(
        value is not None and value is not False and value != []
        for value in (
            issuer_url,
            resource_url,
            audience,
            jwks_url,
            required_scopes,
            allowed_algorithms,
            clock_skew_seconds,
            http_timeout_seconds,
            jwks_cache_ttl_seconds,
            jwks_refresh_cooldown_seconds,
            allowed_hosts,
            allow_insecure_http,
        )
    )
    env_overrides_present = any(
        bool(environ.get(var, "").strip()) for var in _ALL_OAUTH_SETTING_ENV_VARS
    )

    if normalized_mode == "none":
        if cli_overrides_present or env_overrides_present:
            raise OAuthConfigError(
                "OAuth configuration options were provided while OAuth mode is 'none'. "
                "Set --oauth-mode jwt (or OPEN_SANDBOX_MCP_OAUTH_MODE=jwt) to enable "
                "OAuth, or remove the OAuth options."
            )
        return None

    if transport != "streamable-http":
        raise OAuthConfigError(
            "OAuth Resource Server mode is only supported with "
            "--transport streamable-http."
        )

    resolved_issuer = (
        issuer_url if issuer_url is not None else environ.get(ENV_ISSUER_URL)
    )
    if not resolved_issuer:
        raise OAuthConfigError(
            f"OAuth mode 'jwt' requires --oauth-issuer-url (or {ENV_ISSUER_URL})."
        )

    resolved_resource = (
        resource_url if resource_url is not None else environ.get(ENV_RESOURCE_URL)
    )
    if not resolved_resource:
        raise OAuthConfigError(
            f"OAuth mode 'jwt' requires --oauth-resource-url (or {ENV_RESOURCE_URL})."
        )

    resolved_audience = audience if audience is not None else environ.get(ENV_AUDIENCE)
    if resolved_audience == "":
        resolved_audience = None

    resolved_jwks = jwks_url if jwks_url is not None else environ.get(ENV_JWKS_URL)
    if resolved_jwks == "":
        resolved_jwks = None

    resolved_scopes = _split_tokens(
        required_scopes
        if required_scopes is not None
        else environ.get(ENV_REQUIRED_SCOPES)
    )
    if not resolved_scopes:
        raise OAuthConfigError(
            "OAuth mode 'jwt' requires at least one --oauth-required-scope "
            f"(or {ENV_REQUIRED_SCOPES})."
        )

    raw_algs = (
        allowed_algorithms
        if allowed_algorithms is not None
        else environ.get(ENV_ALLOWED_ALGS)
    )
    resolved_algs = (
        _split_tokens(raw_algs) if raw_algs is not None else DEFAULT_ALLOWED_ALGORITHMS
    )

    resolved_clock_skew = (
        _parse_float(clock_skew_seconds, "clock_skew_seconds")
        if clock_skew_seconds is not None
        else (
            _parse_float(environ[ENV_CLOCK_SKEW_SECONDS], ENV_CLOCK_SKEW_SECONDS)
            if environ.get(ENV_CLOCK_SKEW_SECONDS, "").strip()
            else 30.0
        )
    )
    resolved_timeout = (
        _parse_float(http_timeout_seconds, "http_timeout_seconds")
        if http_timeout_seconds is not None
        else (
            _parse_float(environ[ENV_HTTP_TIMEOUT_SECONDS], ENV_HTTP_TIMEOUT_SECONDS)
            if environ.get(ENV_HTTP_TIMEOUT_SECONDS, "").strip()
            else 5.0
        )
    )
    resolved_cache_ttl = (
        _parse_float(jwks_cache_ttl_seconds, "jwks_cache_ttl_seconds")
        if jwks_cache_ttl_seconds is not None
        else (
            _parse_float(
                environ[ENV_JWKS_CACHE_TTL_SECONDS], ENV_JWKS_CACHE_TTL_SECONDS
            )
            if environ.get(ENV_JWKS_CACHE_TTL_SECONDS, "").strip()
            else 300.0
        )
    )
    resolved_cooldown = (
        _parse_float(jwks_refresh_cooldown_seconds, "jwks_refresh_cooldown_seconds")
        if jwks_refresh_cooldown_seconds is not None
        else (
            _parse_float(
                environ[ENV_JWKS_REFRESH_COOLDOWN_SECONDS],
                ENV_JWKS_REFRESH_COOLDOWN_SECONDS,
            )
            if environ.get(ENV_JWKS_REFRESH_COOLDOWN_SECONDS, "").strip()
            else 30.0
        )
    )

    resolved_hosts = _split_tokens(
        allowed_hosts if allowed_hosts is not None else environ.get(ENV_ALLOWED_HOSTS)
    )

    if allow_insecure_http is not None:
        resolved_insecure = bool(allow_insecure_http)
    elif environ.get(ENV_ALLOW_INSECURE_HTTP, "").strip():
        resolved_insecure = _parse_bool_env(
            environ[ENV_ALLOW_INSECURE_HTTP], ENV_ALLOW_INSECURE_HTTP
        )
    else:
        resolved_insecure = False

    return OAuthConfig(
        issuer_url=resolved_issuer,
        resource_url=resolved_resource,
        required_scopes=resolved_scopes,
        audience=resolved_audience,
        jwks_url=resolved_jwks,
        allowed_algorithms=resolved_algs,
        clock_skew_seconds=resolved_clock_skew,
        http_timeout_seconds=resolved_timeout,
        jwks_cache_ttl_seconds=resolved_cache_ttl,
        jwks_refresh_cooldown_seconds=resolved_cooldown,
        allowed_hosts=resolved_hosts,
        allow_insecure_http=resolved_insecure,
    )


def build_auth_settings(config: OAuthConfig) -> AuthSettings:
    """Build MCP SDK ``AuthSettings`` from a validated ``OAuthConfig``.

    Uses ``model_validate`` with raw URL strings so ``AuthSettings``'s
    ``url_preserve_empty_path=True`` config preserves exact canonical issuer
    and resource URLs without appending a spurious trailing slash.
    """
    return AuthSettings.model_validate(
        {
            "issuer_url": config.issuer_url,
            "resource_server_url": config.resource_url,
            "required_scopes": list(config.required_scopes),
            "validate_token_resource": config.uses_url_resource_audience,
        }
    )


def build_transport_security(
    config: OAuthConfig,
    *,
    bind_host: str = "127.0.0.1",
) -> TransportSecuritySettings:
    """Build DNS-rebinding protection settings aligned with ``resource_url``.

    Uses only static configuration (never request headers) so reverse-proxied
    canonical ``Host`` headers succeed while spoofed ``Host`` or ``Origin``
    headers are rejected.
    """
    parsed = urlparse(config.resource_url)
    scheme = parsed.scheme.lower()
    netloc = parsed.netloc
    default_port = 443 if scheme == "https" else 80

    allowed_hosts: list[str] = [netloc]
    if parsed.port is None:
        allowed_hosts.append(f"{netloc}:{default_port}")
    elif parsed.port == default_port and parsed.hostname:
        host_only = (
            f"[{parsed.hostname}]" if ":" in parsed.hostname else parsed.hostname
        )
        if host_only not in allowed_hosts:
            allowed_hosts.append(host_only)

    for extra_host in config.allowed_hosts:
        if extra_host not in allowed_hosts:
            allowed_hosts.append(extra_host)

    allowed_origins: list[str] = [f"{scheme}://{netloc}"]

    if config.allow_insecure_http and _is_loopback_host(bind_host):
        for loopback_host in ("127.0.0.1:*", "localhost:*", "[::1]:*"):
            if loopback_host not in allowed_hosts:
                allowed_hosts.append(loopback_host)
        for loopback_origin in (
            "http://127.0.0.1:*",
            "http://localhost:*",
            "http://[::1]:*",
        ):
            if loopback_origin not in allowed_origins:
                allowed_origins.append(loopback_origin)

    return TransportSecuritySettings(
        enable_dns_rebinding_protection=True,
        allowed_hosts=allowed_hosts,
        allowed_origins=allowed_origins,
    )


@dataclass(frozen=True)
class _CachedJWK:
    kid: str | None
    kty: str
    alg: str | None
    key: Any


def _validate_public_key_strength(public_key: Any) -> bool:
    """Enforce minimum cryptographic key strength aligned with SECURITY.md."""
    if isinstance(public_key, rsa.RSAPublicKey):
        return public_key.key_size >= MIN_RSA_KEY_BITS
    if isinstance(public_key, ec.EllipticCurvePublicKey):
        return public_key.curve.key_size >= MIN_EC_KEY_BITS
    if isinstance(public_key, (ed25519.Ed25519PublicKey, ed448.Ed448PublicKey)):
        return True
    return False


def _build_discovery_urls(issuer_url: str) -> tuple[str, str]:
    parsed = urlparse(issuer_url)
    path = parsed.path.rstrip("/")
    rfc8414_url = f"{parsed.scheme}://{parsed.netloc}/.well-known/oauth-authorization-server{path}"
    oidc_url = f"{issuer_url.rstrip('/')}/.well-known/openid-configuration"
    return rfc8414_url, oidc_url


class JWKSClient:
    """Bounded, fail-closed JWKS fetcher with optional RFC 8414 / OIDC discovery."""

    def __init__(
        self,
        config: OAuthConfig,
        *,
        http_client: httpx.AsyncClient | None = None,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self._config = config
        self._clock = clock
        self._owns_client = http_client is None
        self._client = http_client or httpx.AsyncClient(
            timeout=httpx.Timeout(config.http_timeout_seconds),
            follow_redirects=False,
        )
        self._lock = asyncio.Lock()
        self._retry_after: float = -math.inf
        self._keys: tuple[_CachedJWK, ...] = ()
        self._expires_at: float = 0.0
        self._last_refresh_at: float = -math.inf

    async def aclose(self) -> None:
        """Close the underlying HTTP client if owned by this instance."""
        if self._owns_client:
            await self._client.aclose()

    async def get_signing_keys(
        self, *, force_refresh: bool = False
    ) -> tuple[_CachedJWK, ...]:
        """Return cached signing keys, refreshing when expired or forced."""
        now = self._clock()
        if not force_refresh and self._keys and now < self._expires_at:
            return self._keys

        async with self._lock:
            now = self._clock()
            if not force_refresh and self._keys and now < self._expires_at:
                return self._keys
            if (
                force_refresh
                and self._keys
                and now < self._expires_at
                and (now - self._last_refresh_at)
                < self._config.jwks_refresh_cooldown_seconds
            ):
                return self._keys

            if now < self._retry_after:
                raise _JWKSError("Waiting to retry a failed JWKS refresh.")
            try:
                jwks_url = await self._resolve_jwks_url()
                payload = await self._fetch_json(jwks_url, purpose="JWKS")
                keys = self._parse_jwks(payload)
                if not keys:
                    raise _JWKSError("JWKS document contained no usable signing keys.")
            except _JWKSError:
                self._retry_after = (
                    self._clock() + self._config.jwks_refresh_cooldown_seconds
                )
                raise

            refreshed_at = self._clock()
            self._keys = keys
            self._last_refresh_at = refreshed_at
            self._expires_at = refreshed_at + self._config.jwks_cache_ttl_seconds
            return self._keys

    async def _resolve_jwks_url(self) -> str:
        if self._config.jwks_url is not None:
            return self._config.jwks_url

        issuer_origin = _origin_tuple(self._config.issuer_url)
        last_error: Exception | None = None
        for discovery_url in _build_discovery_urls(self._config.issuer_url):
            try:
                metadata = await self._fetch_json(discovery_url, purpose="AS metadata")
            except _JWKSError as exc:
                last_error = exc
                continue

            if metadata.get("issuer") != self._config.issuer_url:
                raise _JWKSError(
                    "Authorization Server metadata issuer does not match configured issuer_url."
                )
            jwks_uri = metadata.get("jwks_uri")
            if not isinstance(jwks_uri, str) or not jwks_uri:
                raise _JWKSError(
                    "Authorization Server metadata is missing a valid jwks_uri."
                )
            try:
                _validate_http_url(
                    jwks_uri,
                    "jwks_uri",
                    allow_insecure_http=self._config.allow_insecure_http,
                )
            except OAuthConfigError as exc:
                raise _JWKSError(str(exc)) from exc

            if _origin_tuple(jwks_uri) != issuer_origin:
                raise _JWKSError(
                    "Discovered jwks_uri origin does not match configured issuer_url; "
                    "pin an explicit --oauth-jwks-url if cross-origin JWKS is intended."
                )

            return jwks_uri

        raise _JWKSError(
            "Unable to discover Authorization Server metadata for JWKS."
        ) from last_error

    async def _fetch_json(self, url: str, *, purpose: str) -> dict[str, Any]:
        try:
            async with self._client.stream(
                "GET",
                url,
                headers={"Accept": "application/json", "Accept-Encoding": "identity"},
                follow_redirects=False,
                timeout=self._config.http_timeout_seconds,
            ) as response:
                if response.status_code != 200:
                    raise _JWKSError(
                        f"Unexpected HTTP status while fetching {purpose}."
                    )
                if (
                    response.headers.get("content-encoding", "identity").lower()
                    != "identity"
                ):
                    raise _JWKSError(f"Unexpected content encoding for {purpose}.")
                body = bytearray()
                async for chunk in response.aiter_bytes():
                    if len(body) + len(chunk) > MAX_JWKS_RESPONSE_BYTES:
                        raise _JWKSError(
                            f"{purpose} response exceeded maximum allowed size."
                        )
                    body.extend(chunk)
        except httpx.HTTPError as exc:
            raise _JWKSError(f"Failed to fetch {purpose}: network error.") from exc

        try:
            data = json.loads(body)
        except (ValueError, RecursionError) as exc:
            raise _JWKSError(f"{purpose} response was not valid JSON.") from exc

        if not isinstance(data, dict):
            raise _JWKSError(f"{purpose} response must be a JSON object.")
        return data

    def _parse_jwks(self, payload: dict[str, Any]) -> tuple[_CachedJWK, ...]:
        raw_keys = payload.get("keys")
        if not isinstance(raw_keys, list) or not raw_keys:
            raise _JWKSError("JWKS payload missing non-empty 'keys' array.")

        parsed: list[_CachedJWK] = []
        for entry in raw_keys:
            if not isinstance(entry, dict):
                continue
            use = entry.get("use")
            if use is not None and use != "sig":
                continue
            kty = entry.get("kty")
            if kty not in ("RSA", "EC", "OKP"):
                continue
            key_ops = entry.get("key_ops")
            if key_ops is not None and (
                not isinstance(key_ops, list) or "verify" not in key_ops
            ):
                continue
            try:
                jwk_obj = PyJWK.from_dict(entry)
            except Exception:
                continue
            if not _validate_public_key_strength(jwk_obj.key):
                continue
            kid_raw = entry.get("kid")
            kid = kid_raw if isinstance(kid_raw, str) and kid_raw else None
            alg_raw = entry.get("alg")
            alg = alg_raw if isinstance(alg_raw, str) and alg_raw else None
            parsed.append(_CachedJWK(kid=kid, kty=kty, alg=alg, key=jwk_obj.key))
        return tuple(parsed)


class JWTTokenVerifier(TokenVerifier):
    """Fail-closed OAuth 2.1 JWT access-token verifier backed by JWKS."""

    def __init__(
        self,
        config: OAuthConfig,
        *,
        jwks_client: JWKSClient | None = None,
        http_client: httpx.AsyncClient | None = None,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self.config = config
        self._jwks_client = jwks_client or JWKSClient(
            config, http_client=http_client, clock=clock
        )

    async def aclose(self) -> None:
        """Release underlying HTTP resources."""
        await self._jwks_client.aclose()

    async def verify_token(self, token: str) -> AccessToken | None:
        """Verify a JWT bearer access token and return ``AccessToken`` or ``None``."""
        if not isinstance(token, str) or not token:
            return None
        if _WHITESPACE_RE.search(token):
            return None
        if len(token.encode("utf-8")) > MAX_BEARER_TOKEN_BYTES:
            return None
        if token.count(".") != 2:
            return None

        try:
            header = jwt.get_unverified_header(token)
        except (InvalidTokenError, RecursionError):
            return None

        if not isinstance(header, dict):
            return None
        if any(param in header for param in FORBIDDEN_HEADER_PARAMS):
            logger.warning("Rejected JWT carrying forbidden header key reference.")
            return None

        alg = header.get("alg")
        if not isinstance(alg, str) or alg not in self.config.allowed_algorithms:
            return None

        typ = header.get("typ")
        if typ is not None:
            if not isinstance(typ, str) or typ.lower() not in ALLOWED_TOKEN_TYPES:
                return None

        kid_raw = header.get("kid")
        if kid_raw is not None and (not isinstance(kid_raw, str) or not kid_raw):
            return None
        kid: str | None = kid_raw

        signing_key = await self._select_signing_key(alg=alg, kid=kid)
        if signing_key is None:
            return None

        try:
            claims = jwt.decode(
                token,
                key=signing_key.key,
                algorithms=[alg],
                issuer=self.config.issuer_url,
                audience=self.config.expected_audience,
                leeway=self.config.clock_skew_seconds,
                options={
                    "require": ["exp", "iss", "aud"],
                    "verify_signature": True,
                    "verify_exp": True,
                    "verify_nbf": True,
                    "verify_iss": True,
                    "verify_aud": True,
                },
            )
        except InvalidTokenError:
            return None
        except Exception:
            logger.warning("Rejected JWT due to unexpected verification failure.")
            return None

        if not isinstance(claims, dict):
            return None

        return self._build_access_token(token, claims)

    async def _select_signing_key(
        self, *, alg: str, kid: str | None
    ) -> _CachedJWK | None:
        expected_kty = _ALG_TO_KTY.get(alg)
        if expected_kty is None:
            return None

        try:
            keys = await self._jwks_client.get_signing_keys(force_refresh=False)
        except _JWKSError:
            logger.warning("JWKS fetch failed; failing closed.")
            return None

        match = self._match_key(keys, expected_kty=expected_kty, alg=alg, kid=kid)
        if match is not None or kid is None:
            return match

        try:
            refreshed_keys = await self._jwks_client.get_signing_keys(
                force_refresh=True
            )
        except _JWKSError:
            logger.warning("JWKS refresh failed for unknown kid; failing closed.")
            return None

        return self._match_key(
            refreshed_keys, expected_kty=expected_kty, alg=alg, kid=kid
        )

    @staticmethod
    def _match_key(
        keys: tuple[_CachedJWK, ...],
        *,
        expected_kty: str,
        alg: str,
        kid: str | None,
    ) -> _CachedJWK | None:
        compatible = [
            key
            for key in keys
            if key.kty == expected_kty and (key.alg is None or key.alg == alg)
        ]
        if kid is not None:
            kid_matches = [key for key in compatible if key.kid == kid]
            return kid_matches[0] if len(kid_matches) == 1 else None
        return compatible[0] if len(compatible) == 1 else None

    def _build_access_token(
        self, raw_token: str, claims: dict[str, Any]
    ) -> AccessToken | None:
        if claims.get("token_use", "access") != "access" or any(
            claim in claims for claim in ID_TOKEN_ONLY_CLAIMS
        ):
            return None

        iss = claims.get("iss")
        if iss != self.config.issuer_url:
            return None

        for name in ("exp", "iat", "nbf"):
            if name in claims:
                value = claims[name]
                if isinstance(value, bool) or not isinstance(value, (int, float)):
                    return None
        exp = claims["exp"]

        sub_raw = claims.get("sub")
        if sub_raw is not None and (
            not isinstance(sub_raw, str) or not sub_raw.strip()
        ):
            return None
        subject = sub_raw if isinstance(sub_raw, str) else None

        client_id = self._extract_client_id(claims, subject)
        if client_id is None:
            return None

        scopes = self._extract_scopes(claims)
        if scopes is None:
            return None

        # Preserve integer NumericDates without round-tripping through a float.
        if isinstance(exp, int):
            effective_expires_at = exp + math.floor(self.config.clock_skew_seconds)
        else:
            effective_expires_at = math.floor(exp + self.config.clock_skew_seconds)

        return AccessToken(
            token=raw_token,
            client_id=client_id,
            scopes=scopes,
            expires_at=effective_expires_at,
            resource=self.config.expected_audience,
            subject=subject,
            claims={"iss": iss},
        )

    @staticmethod
    def _extract_client_id(claims: dict[str, Any], subject: str | None) -> str | None:
        for key in ("client_id", "azp"):
            val = claims.get(key)
            if val is not None:
                if not isinstance(val, str) or not val.strip():
                    return None
                return val
        return subject

    @staticmethod
    def _extract_scopes(claims: dict[str, Any]) -> list[str] | None:
        scopes: list[str] = []
        for claim in ("scope", "scp"):
            value = claims.get(claim)
            if value is None:
                continue
            if isinstance(value, str):
                items = [item for item in value.split(" ") if item]
            elif claim == "scp" and isinstance(value, list):
                items = value
            else:
                return None
            for item in items:
                if not isinstance(item, str) or not _SCOPE_TOKEN.fullmatch(item):
                    return None
                scopes.append(item)
        return list(dict.fromkeys(scopes))


class StrictBearerMiddleware:
    """ASGI guard rejecting malformed/duplicate Authorization headers and query tokens."""

    def __init__(
        self,
        app: ASGIApp,
        *,
        protected_path: str,
        resource_metadata_url: str,
    ) -> None:
        self.app = app
        self.protected_path = protected_path.rstrip("/") or "/"
        self.resource_metadata_url = resource_metadata_url

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return

        req_path = (scope.get("path") or "/").rstrip("/") or "/"
        if req_path != self.protected_path:
            await self.app(scope, receive, send)
            return

        query_bytes: bytes = scope.get("query_string", b"")
        if query_bytes:
            query_pairs = parse_qsl(
                query_bytes.decode("latin-1"), keep_blank_values=True
            )
            if any(key.lower() == "access_token" for key, _ in query_pairs):
                await self._send_401(
                    send,
                    error="invalid_request",
                    description="Bearer tokens are not permitted in query parameters",
                )
                return

        auth_headers = [
            value
            for name, value in scope.get("headers", ())
            if name.lower() == b"authorization"
        ]
        if len(auth_headers) > 1:
            await self._send_401(
                send,
                error="invalid_request",
                description="Multiple Authorization headers are not allowed",
            )
            return

        if len(auth_headers) == 1:
            raw_value = auth_headers[0]
            if len(raw_value) > MAX_AUTHORIZATION_HEADER_BYTES:
                await self._send_401(
                    send,
                    error="invalid_token",
                    description="Authorization header exceeds maximum size",
                )
                return
            try:
                header_text = raw_value.decode("ascii")
            except UnicodeDecodeError:
                await self._send_401(
                    send,
                    error="invalid_token",
                    description="Invalid Authorization header encoding",
                )
                return
            if not header_text.lower().startswith("bearer "):
                await self._send_401(
                    send,
                    error="invalid_token",
                    description="Unsupported authentication scheme",
                )
                return
            token_part = header_text[7:]
            if not token_part or _WHITESPACE_RE.search(token_part):
                await self._send_401(
                    send,
                    error="invalid_token",
                    description="Malformed Bearer token",
                )
                return

        await self.app(scope, receive, send)

    async def _send_401(self, send: Send, *, error: str, description: str) -> None:
        www_auth = (
            f'Bearer error="{error}", '
            f'error_description="{description}", '
            f'resource_metadata="{self.resource_metadata_url}"'
        )
        body_bytes = json.dumps(
            {"error": error, "error_description": description}
        ).encode("utf-8")
        await send(
            {
                "type": "http.response.start",
                "status": 401,
                "headers": [
                    (b"content-type", b"application/json"),
                    (b"content-length", str(len(body_bytes)).encode("ascii")),
                    (b"www-authenticate", www_auth.encode("ascii")),
                ],
            }
        )
        await send({"type": "http.response.body", "body": body_bytes})


def canonical_resource_metadata_url(config: OAuthConfig) -> str:
    """Return the RFC 9728 protected-resource metadata URL for ``config``."""
    return str(build_resource_metadata_url(AnyHttpUrl(config.resource_url)))
