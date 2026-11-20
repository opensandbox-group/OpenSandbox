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

"""Unit tests for OAuth 2.1 Resource Server configuration and fail-closed validation."""

from __future__ import annotations

import pytest

from opensandbox_mcp.auth import (
    DEFAULT_ALLOWED_ALGORITHMS,
    ENV_ALLOW_INSECURE_HTTP,
    ENV_ALLOWED_ALGS,
    ENV_ALLOWED_HOSTS,
    ENV_AUDIENCE,
    ENV_CLOCK_SKEW_SECONDS,
    ENV_HTTP_TIMEOUT_SECONDS,
    ENV_ISSUER_URL,
    ENV_JWKS_CACHE_TTL_SECONDS,
    ENV_JWKS_REFRESH_COOLDOWN_SECONDS,
    ENV_JWKS_URL,
    ENV_MODE,
    ENV_REQUIRED_SCOPES,
    ENV_RESOURCE_URL,
    OAuthConfig,
    OAuthConfigError,
    build_auth_settings,
    build_transport_security,
    canonical_resource_metadata_url,
    load_oauth_config,
)
from opensandbox_mcp.server import create_server, create_streamable_http_app


def _valid_config(**overrides) -> OAuthConfig:
    params = {
        "issuer_url": "https://idp.example.com",
        "resource_url": "https://mcp.example.com/mcp",
        "required_scopes": ("opensandbox:mcp",),
    }
    params.update(overrides)
    return OAuthConfig(**params)


def test_default_mode_none_returns_none() -> None:
    cfg = load_oauth_config(transport="stdio", env={})
    assert cfg is None

    cfg_http = load_oauth_config(transport="streamable-http", env={})
    assert cfg_http is None


def test_mode_none_fails_closed_when_cli_oauth_option_set() -> None:
    with pytest.raises(OAuthConfigError, match="OAuth mode is 'none'"):
        load_oauth_config(
            transport="streamable-http",
            issuer_url="https://idp.example.com",
            env={},
        )


def test_mode_none_fails_closed_when_env_oauth_option_set() -> None:
    with pytest.raises(OAuthConfigError, match="OAuth mode is 'none'"):
        load_oauth_config(
            transport="streamable-http",
            env={ENV_ISSUER_URL: "https://idp.example.com"},
        )


def test_invalid_mode_fails_closed() -> None:
    with pytest.raises(OAuthConfigError, match="Unsupported OAuth mode"):
        load_oauth_config(transport="streamable-http", mode="introspection", env={})


def test_jwt_mode_rejected_for_stdio() -> None:
    with pytest.raises(
        OAuthConfigError, match="only supported with --transport streamable-http"
    ):
        load_oauth_config(
            transport="stdio",
            mode="jwt",
            issuer_url="https://idp.example.com",
            resource_url="https://mcp.example.com/mcp",
            required_scopes=["opensandbox:mcp"],
            env={},
        )


@pytest.mark.parametrize("missing_field", ["issuer", "resource", "scopes"])
def test_jwt_mode_requires_issuer_resource_and_scopes(missing_field: str) -> None:
    kwargs = {
        "transport": "streamable-http",
        "mode": "jwt",
        "issuer_url": None if missing_field == "issuer" else "https://idp.example.com",
        "resource_url": None
        if missing_field == "resource"
        else "https://mcp.example.com/mcp",
        "required_scopes": None if missing_field == "scopes" else ["opensandbox:mcp"],
        "env": {},
    }
    with pytest.raises(OAuthConfigError):
        load_oauth_config(**kwargs)


def test_env_loading_and_cli_precedence() -> None:
    env = {
        ENV_MODE: "jwt",
        ENV_ISSUER_URL: "https://env-idp.example.com",
        ENV_RESOURCE_URL: "https://env-mcp.example.com/mcp",
        ENV_AUDIENCE: "env-audience",
        ENV_JWKS_URL: "https://env-idp.example.com/keys",
        ENV_REQUIRED_SCOPES: "scope:a, scope:b",
        ENV_ALLOWED_ALGS: "RS256,ES256",
        ENV_CLOCK_SKEW_SECONDS: "15",
        ENV_HTTP_TIMEOUT_SECONDS: "8",
        ENV_JWKS_CACHE_TTL_SECONDS: "600",
        ENV_JWKS_REFRESH_COOLDOWN_SECONDS: "45",
        ENV_ALLOWED_HOSTS: "internal.example.local",
        ENV_ALLOW_INSECURE_HTTP: "false",
    }

    from_env = load_oauth_config(transport="streamable-http", env=env)
    assert from_env is not None
    assert from_env.issuer_url == "https://env-idp.example.com"
    assert from_env.resource_url == "https://env-mcp.example.com/mcp"
    assert from_env.expected_audience == "env-audience"
    assert from_env.uses_url_resource_audience is False
    assert from_env.jwks_url == "https://env-idp.example.com/keys"
    assert from_env.required_scopes == ("scope:a", "scope:b")
    assert from_env.allowed_algorithms == ("RS256", "ES256")
    assert from_env.clock_skew_seconds == 15.0
    assert from_env.http_timeout_seconds == 8.0
    assert from_env.jwks_cache_ttl_seconds == 600.0
    assert from_env.jwks_refresh_cooldown_seconds == 45.0
    assert from_env.allowed_hosts == ("internal.example.local",)

    overridden = load_oauth_config(
        transport="streamable-http",
        issuer_url="https://cli-idp.example.com",
        required_scopes=["cli:scope"],
        env=env,
    )
    assert overridden is not None
    assert overridden.issuer_url == "https://cli-idp.example.com"
    assert overridden.required_scopes == ("cli:scope",)


@pytest.mark.parametrize(
    "bad_url",
    [
        "http://idp.example.com",
        "http://127.0.0.1:9000",
        "ftp://idp.example.com",
        "https://user:pass@idp.example.com",
        "https://idp.example.com?foo=bar",
        "https://idp.example.com#frag",
        " https://idp.example.com ",
        "",
    ],
)
def test_url_validation_rejects_insecure_or_malformed_urls(bad_url: str) -> None:
    with pytest.raises(OAuthConfigError):
        _valid_config(issuer_url=bad_url)


def test_allow_insecure_http_only_permits_loopback() -> None:
    cfg = _valid_config(
        issuer_url="http://127.0.0.1:9000",
        resource_url="http://localhost:8000/mcp",
        jwks_url="http://[::1]:9000/jwks.json",
        allow_insecure_http=True,
    )
    assert cfg.allow_insecure_http is True

    with pytest.raises(OAuthConfigError, match="https://"):
        _valid_config(
            issuer_url="http://idp.example.com",
            allow_insecure_http=True,
        )


@pytest.mark.parametrize("bad_algs", [(), ("none",), ("HS256",), ("RS256", "HS512")])
def test_disallowed_or_symmetric_algorithms_rejected(bad_algs: tuple[str, ...]) -> None:
    with pytest.raises(OAuthConfigError):
        _valid_config(allowed_algorithms=bad_algs)


def test_numeric_and_host_bounds_validation() -> None:
    with pytest.raises(OAuthConfigError):
        _valid_config(clock_skew_seconds=-1)
    with pytest.raises(OAuthConfigError):
        _valid_config(clock_skew_seconds=301)
    with pytest.raises(OAuthConfigError):
        _valid_config(http_timeout_seconds=0)
    with pytest.raises(OAuthConfigError):
        _valid_config(jwks_cache_ttl_seconds=0)
    with pytest.raises(OAuthConfigError):
        _valid_config(jwks_refresh_cooldown_seconds=-5)
    with pytest.raises(OAuthConfigError):
        _valid_config(allowed_hosts=("https://bad.example.com",))
    with pytest.raises(OAuthConfigError):
        load_oauth_config(
            transport="streamable-http",
            mode="jwt",
            issuer_url="https://idp.example.com",
            resource_url="https://mcp.example.com/mcp",
            required_scopes=["s"],
            env={ENV_ALLOW_INSECURE_HTTP: "maybe"},
        )
    with pytest.raises(OAuthConfigError):
        load_oauth_config(
            transport="streamable-http",
            mode="jwt",
            issuer_url="https://idp.example.com",
            resource_url="https://mcp.example.com/mcp",
            required_scopes=["s"],
            env={ENV_CLOCK_SKEW_SECONDS: "not-a-number"},
        )


def test_build_auth_settings_and_transport_security() -> None:
    cfg = _valid_config(
        resource_url="https://mcp.example.com/mcp",
        allowed_hosts=("mcp-internal:8000",),
    )
    assert cfg.allowed_algorithms == DEFAULT_ALLOWED_ALGORITHMS
    assert cfg.expected_audience == "https://mcp.example.com/mcp"
    assert cfg.uses_url_resource_audience is True

    settings = build_auth_settings(cfg)
    assert str(settings.issuer_url) == "https://idp.example.com"
    assert str(settings.resource_server_url) == "https://mcp.example.com/mcp"
    assert settings.required_scopes == ["opensandbox:mcp"]
    assert settings.validate_token_resource is True

    assert (
        canonical_resource_metadata_url(cfg)
        == "https://mcp.example.com/.well-known/oauth-protected-resource/mcp"
    )

    sec = build_transport_security(cfg, bind_host="127.0.0.1")
    assert sec.enable_dns_rebinding_protection is True
    assert "mcp.example.com" in sec.allowed_hosts
    assert "mcp.example.com:443" in sec.allowed_hosts
    assert "mcp-internal:8000" in sec.allowed_hosts
    assert "127.0.0.1:*" not in sec.allowed_hosts
    assert sec.allowed_origins == ["https://mcp.example.com"]

    custom_aud_cfg = _valid_config(audience="api://opensandbox-mcp")
    assert build_auth_settings(custom_aud_cfg).validate_token_resource is False

    # Explicit default port and loopback insecure HTTP dev mode
    explicit_port_cfg = _valid_config(resource_url="https://mcp.example.com:443/mcp")
    sec_port = build_transport_security(explicit_port_cfg, bind_host="0.0.0.0")
    assert "mcp.example.com:443" in sec_port.allowed_hosts
    assert "mcp.example.com" in sec_port.allowed_hosts

    local_cfg = _valid_config(
        issuer_url="http://127.0.0.1:9000",
        resource_url="http://127.0.0.1:8000/mcp",
        allow_insecure_http=True,
    )
    sec_local = build_transport_security(local_cfg, bind_host="127.0.0.1")
    assert "127.0.0.1:*" in sec_local.allowed_hosts
    assert "http://localhost:*" in sec_local.allowed_origins


def test_required_scopes_and_audience_edge_validation() -> None:
    with pytest.raises(OAuthConfigError, match="At least one required scope"):
        _valid_config(required_scopes=())
    with pytest.raises(OAuthConfigError, match="without whitespace"):
        _valid_config(required_scopes=("bad scope",))
    with pytest.raises(OAuthConfigError, match="audience must be a non-empty"):
        _valid_config(audience=" ")
    assert (
        _valid_config(
            resource_url="https://mcp.example.com/mcp",
            audience="https://mcp.example.com/mcp/",
        ).uses_url_resource_audience
        is True
    )


def test_server_and_app_reject_inconsistent_auth_wiring() -> None:
    cfg = _valid_config()
    with pytest.raises(ValueError, match="token_verifier requires oauth_config"):
        create_server(token_verifier=object())  # type: ignore[arg-type]

    unauthed_server = create_server()
    with pytest.raises(ValueError, match="created without OAuth auth settings"):
        create_streamable_http_app(unauthed_server, oauth_config=cfg)
