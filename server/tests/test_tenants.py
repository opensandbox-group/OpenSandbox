# Copyright 2026 Alibaba Group Holding Ltd.
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

"""Tests for the tenants package: file provider, HTTP provider, context, and validation."""

from __future__ import annotations

import time
from pathlib import Path
from unittest.mock import MagicMock

import pytest

from opensandbox_server.tenants import validate_tenant_config
from opensandbox_server.tenants.context import get_current_tenant, set_current_tenant
from opensandbox_server.tenants.file_provider import (
    FileTenantProvider,
    _build_lookup_dict,
    _parse_tenants_file,
    resolve_tenants_path,
)
from opensandbox_server.tenants.http_provider import (
    HTTPTenantProvider,
    HTTPTenantProviderConfig,
)
from opensandbox_server.tenants.models import TenantEntry
from opensandbox_server.tenants.provider import TenantProviderUnavailable


SAMPLE_TOML = """\
[[tenants]]
name = "team-alpha"
namespace = "ns-alpha"
api_keys = ["key-alpha-1", "key-alpha-2"]

[[tenants]]
name = "team-beta"
namespace = "ns-beta"
api_keys = ["key-beta-1"]
"""

DUPLICATE_KEY_TOML = """\
[[tenants]]
name = "team-a"
namespace = "ns-a"
api_keys = ["shared-key"]

[[tenants]]
name = "team-b"
namespace = "ns-b"
api_keys = ["shared-key"]
"""

EMPTY_KEYS_TOML = """\
[[tenants]]
name = "team-empty"
namespace = "ns-empty"
api_keys = []
"""


# --- context tests ---


def test_context_default_is_none():
    assert get_current_tenant() is None


def test_context_set_and_get():
    entry = TenantEntry(name="t", namespace="ns-t", api_keys=("k",))
    set_current_tenant(entry)
    assert get_current_tenant() is entry
    set_current_tenant(None)
    assert get_current_tenant() is None


# --- resolve_tenants_path ---


def test_resolve_tenants_path_explicit():
    p = resolve_tenants_path("/etc/tenants.toml")
    assert p == Path("/etc/tenants.toml")


def test_resolve_tenants_path_env(monkeypatch):
    monkeypatch.setenv("SANDBOX_TENANTS_CONFIG_PATH", "/tmp/custom.toml")
    assert resolve_tenants_path() == Path("/tmp/custom.toml")


def test_resolve_tenants_path_default(monkeypatch):
    monkeypatch.delenv("SANDBOX_TENANTS_CONFIG_PATH", raising=False)
    p = resolve_tenants_path()
    assert p == Path.home() / ".opensandbox" / "tenants.toml"


# --- _parse_tenants_file / _build_lookup_dict ---


def test_parse_tenants_file(tmp_path):
    f = tmp_path / "tenants.toml"
    f.write_text(SAMPLE_TOML)
    entries = _parse_tenants_file(f)
    assert len(entries) == 2
    assert entries[0].name == "team-alpha"
    assert entries[0].namespace == "ns-alpha"
    assert entries[0].api_keys == ("key-alpha-1", "key-alpha-2")
    assert entries[1].name == "team-beta"


def test_parse_tenants_file_duplicate_key(tmp_path):
    f = tmp_path / "tenants.toml"
    f.write_text(DUPLICATE_KEY_TOML)
    with pytest.raises(ValueError, match="Duplicate api_key"):
        _parse_tenants_file(f)


def test_parse_tenants_file_empty_keys(tmp_path):
    f = tmp_path / "tenants.toml"
    f.write_text(EMPTY_KEYS_TOML)
    with pytest.raises(ValueError, match="no api_keys"):
        _parse_tenants_file(f)


def test_build_lookup_dict():
    entries = [
        TenantEntry(name="a", namespace="ns-a", api_keys=("k1", "k2")),
        TenantEntry(name="b", namespace="ns-b", api_keys=("k3",)),
    ]
    lookup = _build_lookup_dict(entries)
    assert lookup["k1"].name == "a"
    assert lookup["k2"].name == "a"
    assert lookup["k3"].name == "b"
    assert "k4" not in lookup


# --- FileTenantProvider ---


def test_file_provider_start_and_lookup(tmp_path):
    f = tmp_path / "tenants.toml"
    f.write_text(SAMPLE_TOML)
    provider = FileTenantProvider(f)
    provider.start()
    try:
        assert provider.ready()
        result = provider.lookup("key-alpha-1")
        assert result is not None
        assert result.namespace == "ns-alpha"
        assert provider.lookup("nonexistent") is None
    finally:
        provider.close()


def test_file_provider_list_tenants(tmp_path):
    f = tmp_path / "tenants.toml"
    f.write_text(SAMPLE_TOML)
    provider = FileTenantProvider(f)
    provider.start()
    try:
        tenants = provider.list_tenants()
        assert len(tenants) == 2
        names = {t.name for t in tenants}
        assert names == {"team-alpha", "team-beta"}
    finally:
        provider.close()


def test_file_provider_start_file_not_found(tmp_path):
    provider = FileTenantProvider(tmp_path / "missing.toml")
    with pytest.raises(FileNotFoundError):
        provider.start()


def test_file_provider_hot_reload(tmp_path):
    f = tmp_path / "tenants.toml"
    f.write_text(SAMPLE_TOML)
    provider = FileTenantProvider(f)
    provider.start()
    try:
        assert provider.lookup("key-new") is None
        # Modify file
        time.sleep(0.05)
        f.write_text("""\
[[tenants]]
name = "team-new"
namespace = "ns-new"
api_keys = ["key-new"]
""")
        provider._reload()
        assert provider.lookup("key-new") is not None
        assert provider.lookup("key-new").namespace == "ns-new"
        assert provider.lookup("key-alpha-1") is None
    finally:
        provider.close()


def test_file_provider_reload_bad_file_keeps_previous(tmp_path):
    f = tmp_path / "tenants.toml"
    f.write_text(SAMPLE_TOML)
    provider = FileTenantProvider(f)
    provider.start()
    try:
        f.write_text("invalid toml [[[")
        provider._reload()
        # Should keep previous state
        assert provider.lookup("key-alpha-1") is not None
    finally:
        provider.close()


def test_file_provider_reload_deleted_clears(tmp_path):
    f = tmp_path / "tenants.toml"
    f.write_text(SAMPLE_TOML)
    provider = FileTenantProvider(f)
    provider.start()
    try:
        f.unlink()
        provider._reload()
        assert provider.lookup("key-alpha-1") is None
        assert provider.list_tenants() == []
    finally:
        provider.close()


def test_file_provider_on_reload_callback(tmp_path):
    f = tmp_path / "tenants.toml"
    f.write_text(SAMPLE_TOML)
    provider = FileTenantProvider(f)
    provider.start()
    calls = []
    provider.on_reload(lambda entries: calls.append(entries))
    try:
        f.write_text(SAMPLE_TOML)
        provider._reload()
        assert len(calls) == 1
        assert len(calls[0]) == 2
    finally:
        provider.close()


# --- HTTPTenantProvider ---


def test_http_provider_start_close():
    cfg = HTTPTenantProviderConfig(endpoint="http://localhost:9999/tenants")
    provider = HTTPTenantProvider(cfg)
    assert not provider.ready()
    provider.start()
    assert provider.ready()
    provider.close()
    assert not provider.ready()


def test_http_provider_lookup_cache_hit():
    cfg = HTTPTenantProviderConfig(endpoint="http://localhost:9999/tenants")
    provider = HTTPTenantProvider(cfg)
    provider.start()
    try:
        # Manually inject a cache entry
        from opensandbox_server.tenants.http_provider import _CacheEntry

        entry = TenantEntry(name="cached", namespace="ns-cached", api_keys=("key-c",))
        provider._cache["key-c"] = _CacheEntry(
            tenant=entry, fetched_at=time.monotonic(), ttl=300
        )
        result = provider.lookup("key-c")
        assert result is not None
        assert result.namespace == "ns-cached"
    finally:
        provider.close()


def test_http_provider_lookup_cache_miss_fetch(monkeypatch):
    cfg = HTTPTenantProviderConfig(endpoint="http://localhost:9999/tenants")
    provider = HTTPTenantProvider(cfg)
    provider.start()
    try:
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.json.return_value = {"namespace": "ns-fetched", "ttl": 60}
        mock_response.raise_for_status = MagicMock()

        provider._client.get = MagicMock(return_value=mock_response)
        result = provider.lookup("key-fetch")
        assert result is not None
        assert result.namespace == "ns-fetched"
        assert "key-fetch" in provider._cache
    finally:
        provider.close()


def test_http_provider_lookup_401_returns_none(monkeypatch):
    cfg = HTTPTenantProviderConfig(endpoint="http://localhost:9999/tenants")
    provider = HTTPTenantProvider(cfg)
    provider.start()
    try:
        mock_response = MagicMock()
        mock_response.status_code = 401

        provider._client.get = MagicMock(return_value=mock_response)
        result = provider.lookup("bad-key")
        assert result is None
    finally:
        provider.close()


def test_http_provider_lookup_network_error_raises_unavailable():
    cfg = HTTPTenantProviderConfig(endpoint="http://localhost:9999/tenants")
    provider = HTTPTenantProvider(cfg)
    provider.start()
    try:
        provider._client.get = MagicMock(side_effect=Exception("connection refused"))
        with pytest.raises(TenantProviderUnavailable):
            provider.lookup("any-key")
    finally:
        provider.close()


def test_http_provider_stale_cache_served_on_error():
    cfg = HTTPTenantProviderConfig(
        endpoint="http://localhost:9999/tenants",
        max_stale_seconds=300.0,
    )
    provider = HTTPTenantProvider(cfg)
    provider.start()
    try:
        from opensandbox_server.tenants.http_provider import _CacheEntry

        entry = TenantEntry(name="stale", namespace="ns-stale", api_keys=("key-s",))
        # TTL expired (fetched 10s ago, TTL=1s) but within max_stale
        provider._cache["key-s"] = _CacheEntry(
            tenant=entry, fetched_at=time.monotonic() - 10, ttl=1
        )
        provider._client.get = MagicMock(side_effect=Exception("timeout"))
        result = provider.lookup("key-s")
        assert result is not None
        assert result.namespace == "ns-stale"
    finally:
        provider.close()


def test_http_provider_stale_beyond_max_stale_raises():
    cfg = HTTPTenantProviderConfig(
        endpoint="http://localhost:9999/tenants",
        max_stale_seconds=5.0,
    )
    provider = HTTPTenantProvider(cfg)
    provider.start()
    try:
        from opensandbox_server.tenants.http_provider import _CacheEntry

        entry = TenantEntry(name="old", namespace="ns-old", api_keys=("key-o",))
        # Fetched 100s ago, TTL=1, max_stale=5 → age(100) > ttl(1)+max_stale(5) → reject
        provider._cache["key-o"] = _CacheEntry(
            tenant=entry, fetched_at=time.monotonic() - 100, ttl=1
        )
        provider._client.get = MagicMock(side_effect=Exception("timeout"))
        with pytest.raises(TenantProviderUnavailable):
            provider.lookup("key-o")
    finally:
        provider.close()


def test_http_provider_list_tenants():
    cfg = HTTPTenantProviderConfig(endpoint="http://localhost:9999/tenants")
    provider = HTTPTenantProvider(cfg)
    provider.start()
    try:
        from opensandbox_server.tenants.http_provider import _CacheEntry

        e1 = TenantEntry(name="t1", namespace="ns-1", api_keys=("k1",))
        e2 = TenantEntry(name="t2", namespace="ns-2", api_keys=("k2",))
        provider._cache["k1"] = _CacheEntry(tenant=e1, fetched_at=time.monotonic(), ttl=60)
        provider._cache["k2"] = _CacheEntry(tenant=e2, fetched_at=time.monotonic(), ttl=60)
        tenants = provider.list_tenants()
        names = {t.name for t in tenants}
        assert names == {"t1", "t2"}
    finally:
        provider.close()


def test_http_provider_expired_cache_refresh_success():
    cfg = HTTPTenantProviderConfig(endpoint="http://localhost:9999/tenants")
    provider = HTTPTenantProvider(cfg)
    provider.start()
    try:
        from opensandbox_server.tenants.http_provider import _CacheEntry

        entry = TenantEntry(name="old", namespace="ns-old", api_keys=("key-r",))
        provider._cache["key-r"] = _CacheEntry(
            tenant=entry, fetched_at=time.monotonic() - 100, ttl=1
        )

        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.json.return_value = {"namespace": "ns-refreshed", "ttl": 60}
        mock_response.raise_for_status = MagicMock()
        provider._client.get = MagicMock(return_value=mock_response)

        result = provider.lookup("key-r")
        assert result is not None
        assert result.namespace == "ns-refreshed"
    finally:
        provider.close()


def test_http_provider_expired_401_clears_cache():
    cfg = HTTPTenantProviderConfig(endpoint="http://localhost:9999/tenants")
    provider = HTTPTenantProvider(cfg)
    provider.start()
    try:
        from opensandbox_server.tenants.http_provider import _CacheEntry

        entry = TenantEntry(name="revoked", namespace="ns-r", api_keys=("key-rev",))
        provider._cache["key-rev"] = _CacheEntry(
            tenant=entry, fetched_at=time.monotonic() - 100, ttl=1
        )

        mock_response = MagicMock()
        mock_response.status_code = 401
        provider._client.get = MagicMock(return_value=mock_response)

        result = provider.lookup("key-rev")
        assert result is None
        assert "key-rev" not in provider._cache
    finally:
        provider.close()


def test_http_provider_auth_header():
    cfg = HTTPTenantProviderConfig(
        endpoint="http://localhost:9999/tenants",
        auth_header="X-Service-Token",
        auth_token="secret-123",
    )
    provider = HTTPTenantProvider(cfg)
    provider.start()
    try:
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.json.return_value = {"namespace": "ns-auth", "ttl": 30}
        mock_response.raise_for_status = MagicMock()
        provider._client.get = MagicMock(return_value=mock_response)

        provider.lookup("key-auth")
        call_kwargs = provider._client.get.call_args
        headers = call_kwargs.kwargs.get("headers") or call_kwargs[1].get("headers")
        assert headers["X-Service-Token"] == "secret-123"
        assert headers["OPEN-SANDBOX-API-KEY"] == "key-auth"
    finally:
        provider.close()


# --- validate_tenant_config ---


def test_validate_tenant_config_no_tenants():
    cfg = MagicMock()
    cfg.tenants = None
    validate_tenant_config(cfg)


def test_validate_tenant_config_docker_raises():
    cfg = MagicMock()
    cfg.tenants = MagicMock()
    cfg.runtime.type = "docker"
    with pytest.raises(ValueError, match="docker"):
        validate_tenant_config(cfg)


def test_validate_tenant_config_api_key_raises():
    cfg = MagicMock()
    cfg.tenants = MagicMock()
    cfg.runtime.type = "kubernetes"
    cfg.server.api_key = "some-key"
    with pytest.raises(ValueError, match="api_key"):
        validate_tenant_config(cfg)


def test_validate_tenant_config_ok():
    cfg = MagicMock()
    cfg.tenants = MagicMock()
    cfg.runtime.type = "kubernetes"
    cfg.server.api_key = None
    validate_tenant_config(cfg)
