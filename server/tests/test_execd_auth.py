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

from __future__ import annotations

import base64
import hashlib
import json
import os
import secrets
from datetime import datetime, timezone
from pathlib import Path
from unittest.mock import MagicMock

import pytest
from fastapi import HTTPException

from opensandbox_server.api.schema import Endpoint, ImageSpec
from opensandbox_server.config import (
    AppConfig,
    ExecdConfig,
    KubernetesRuntimeConfig,
    RuntimeConfig,
)
from opensandbox_server.services.constants import (
    DEFAULT_EXECD_PORT,
    EXECD_ACCESS_TOKEN_ENV,
    OPEN_SANDBOX_EXECD_ACCESS_TOKEN_HEADER,
    OPEN_SANDBOX_SECURE_ACCESS_HEADER,
)
from opensandbox_server.services.execd_auth import ExecdTokenManager
from opensandbox_server.services.k8s.agent_sandbox_provider import AgentSandboxProvider
from opensandbox_server.services.k8s.batchsandbox_provider import BatchSandboxProvider
from opensandbox_server.services.k8s.endpoint_resolver import _attach_execd_auth_headers


def _write_key_file(path: Path, active: str, keys: dict[str, str]) -> Path:
    path.write_text(json.dumps({"active": active, "keys": keys}), encoding="utf-8")
    return path


def _gen_master_key_b64() -> str:
    return base64.b64encode(secrets.token_bytes(32)).decode("ascii")


class TestExecdConfig:
    def test_defaults_and_normalization(self) -> None:
        cfg = ExecdConfig()
        assert cfg.auth_signing_key_file is None

        cfg_blank = ExecdConfig(auth_signing_key_file="   ")
        assert cfg_blank.auth_signing_key_file is None


class TestExecdTokenManager:
    def test_mint_and_resolve_round_trip(self, tmp_path: Path) -> None:
        key_v1 = _gen_master_key_b64()
        key_file = _write_key_file(tmp_path / "keys.json", "v1", {"v1": key_v1})

        manager = ExecdTokenManager(ExecdConfig(auth_signing_key_file=str(key_file)))
        env_val = manager.mint_env_value()
        parts = env_val.split(":")
        assert len(parts) == 4
        assert parts[0] == "sha256"
        assert parts[1] == "v1"
        assert len(parts[3]) == 64

        raw_token = manager.resolve_raw_token(env_val)
        assert raw_token != env_val
        assert hashlib.sha256(raw_token.encode("utf-8")).hexdigest() == parts[3]

        # Each mint produces a unique nonce and token
        env_val2 = manager.mint_env_value()
        assert env_val2 != env_val
        assert manager.resolve_raw_token(env_val2) != raw_token

        # Legacy plaintext value is returned trimmed
        assert manager.resolve_raw_token("  legacy-plaintext-token  ") == "legacy-plaintext-token"

    def test_hot_reload_rotation_and_corrupt_fallback(self, tmp_path: Path) -> None:
        key_v1 = _gen_master_key_b64()
        key_v2 = _gen_master_key_b64()
        key_file = _write_key_file(tmp_path / "keys.json", "v1", {"v1": key_v1})

        manager = ExecdTokenManager(
            ExecdConfig(auth_signing_key_file=str(key_file)),
            reload_check_interval=0.0,
        )
        old_env = manager.mint_env_value()
        old_raw = manager.resolve_raw_token(old_env)
        assert old_env.startswith("sha256:v1:")

        # Rotate to v2 while retaining v1 so existing sandboxes keep working
        tmp_new = _write_key_file(
            tmp_path / "keys.json.new", "v2", {"v1": key_v1, "v2": key_v2}
        )
        os.replace(tmp_new, key_file)

        new_env = manager.mint_env_value()
        assert new_env.startswith("sha256:v2:")
        assert manager.resolve_raw_token(old_env) == old_raw
        assert (
            hashlib.sha256(manager.resolve_raw_token(new_env).encode("utf-8")).hexdigest()
            == new_env.split(":")[3]
        )

        # Corrupt file -> retains previous valid key ring
        corrupt = tmp_path / "keys.json.bad"
        corrupt.write_text("{not-json", encoding="utf-8")
        os.replace(corrupt, key_file)

        assert manager.resolve_raw_token(old_env) == old_raw
        assert manager.mint_env_value().startswith("sha256:v2:")

    def test_rejects_invalid_keys_and_tampered_verifier(self, tmp_path: Path) -> None:
        key_v1 = _gen_master_key_b64()
        bad_active = _write_key_file(tmp_path / "bad_active.json", "missing", {"v1": key_v1})
        with pytest.raises(ValueError, match="not found"):
            ExecdTokenManager(ExecdConfig(auth_signing_key_file=str(bad_active)))

        short_key = _write_key_file(
            tmp_path / "short.json",
            "v1",
            {"v1": base64.b64encode(b"too-short").decode("ascii")},
        )
        with pytest.raises(ValueError, match="at least 32 bytes"):
            ExecdTokenManager(ExecdConfig(auth_signing_key_file=str(short_key)))

        key_file = _write_key_file(tmp_path / "ok.json", "v1", {"v1": key_v1})
        manager = ExecdTokenManager(ExecdConfig(auth_signing_key_file=str(key_file)))
        env_val = manager.mint_env_value()
        prefix, kid, nonce, verifier = env_val.split(":")
        tampered = f"{prefix}:{kid}:{nonce}:{'0' * 64}"
        with pytest.raises(ValueError, match="mismatch"):
            manager.resolve_raw_token(tampered)

        with pytest.raises(ValueError, match="Unknown"):
            manager.resolve_raw_token(f"{prefix}:v99:{nonce}:{verifier}")


class TestK8sExecdAuthWiring:
    def _make_manager(self, tmp_path: Path) -> ExecdTokenManager:
        key_file = _write_key_file(tmp_path / "keys.json", "v1", {"v1": _gen_master_key_b64()})
        return ExecdTokenManager(ExecdConfig(auth_signing_key_file=str(key_file)))

    def test_batchsandbox_normal_and_pool_inject_verifier_and_resolve(self, tmp_path: Path) -> None:
        manager = self._make_manager(tmp_path)
        app_config = AppConfig(
            runtime=RuntimeConfig(type="kubernetes", execd_image="execd:test"),
            kubernetes=KubernetesRuntimeConfig(namespace="test-ns"),
        )
        mock_k8s = MagicMock()
        mock_k8s.create_custom_object.return_value = {
            "metadata": {"name": "real-sbx-id", "uid": "uid-1"}
        }
        provider = BatchSandboxProvider(mock_k8s, app_config=app_config)
        provider.set_execd_token_manager(manager)

        # Normal workload creation path
        provider.create_workload(
            sandbox_id="real-sbx-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["sleep", "3600"],
            env={"USER_VAR": "1", EXECD_ACCESS_TOKEN_ENV: "user-spoofed"},
            resource_limits={"cpu": "500m", "memory": "512Mi"},
            labels={"opensandbox.io/id": "real-sbx-id"},
            expires_at=datetime(2026, 12, 1, tzinfo=timezone.utc),
            execd_image="execd:test",
        )
        created_body = mock_k8s.create_custom_object.call_args.kwargs["body"]
        main_env = {
            e["name"]: e["value"]
            for e in created_body["spec"]["template"]["spec"]["containers"][0]["env"]
        }
        assert main_env[EXECD_ACCESS_TOKEN_ENV].startswith("sha256:v1:")
        assert main_env["USER_VAR"] == "1"

        # Endpoint resolution extracts verifier from CR and returns derived raw token
        ep = Endpoint(
            endpoint="sbx-1-44772.example.com",
            headers={OPEN_SANDBOX_SECURE_ACCESS_HEADER: "sa-token"},
        )
        _attach_execd_auth_headers(ep, created_body, DEFAULT_EXECD_PORT, manager)
        assert ep.headers is not None
        assert ep.headers[OPEN_SANDBOX_SECURE_ACCESS_HEADER] == "sa-token"
        raw = ep.headers[OPEN_SANDBOX_EXECD_ACCESS_TOKEN_HEADER]
        assert raw == manager.resolve_raw_token(main_env[EXECD_ACCESS_TOKEN_ENV])

        # Non-execd port does not attach X-EXECD-ACCESS-TOKEN
        ep_other = Endpoint(endpoint="sbx-1-8080.example.com")
        _attach_execd_auth_headers(ep_other, created_body, 8080, manager)
        assert ep_other.headers is None

        # Pool task template path
        task_tmpl = provider._build_task_template(
            entrypoint=["sleep", "3600"],
            env={"USER_VAR": "2", EXECD_ACCESS_TOKEN_ENV: "user-spoofed"},
            sandbox_id="pool-sbx-id",
        )
        pool_env = {e["name"]: e["value"] for e in task_tmpl["spec"]["process"]["env"]}
        assert pool_env[EXECD_ACCESS_TOKEN_ENV].startswith("sha256:v1:")

        pool_workload = {"spec": {"taskTemplate": task_tmpl}}
        ep_pool = Endpoint(endpoint="pool-sbx-44772.example.com")
        _attach_execd_auth_headers(ep_pool, pool_workload, DEFAULT_EXECD_PORT, manager)
        assert ep_pool.headers is not None
        assert ep_pool.headers[OPEN_SANDBOX_EXECD_ACCESS_TOKEN_HEADER] == manager.resolve_raw_token(
            pool_env[EXECD_ACCESS_TOKEN_ENV]
        )

    def test_agent_sandbox_inject_and_legacy_plaintext_compat(self, tmp_path: Path) -> None:
        manager = self._make_manager(tmp_path)
        app_config = AppConfig(
            runtime=RuntimeConfig(type="kubernetes", execd_image="execd:test"),
            kubernetes=KubernetesRuntimeConfig(
                namespace="test-ns", workload_provider="agent-sandbox"
            ),
        )
        provider = AgentSandboxProvider(MagicMock(), app_config=app_config)
        provider.set_execd_token_manager(manager)

        pod_spec = provider._build_pod_spec(
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["sleep", "3600"],
            env={EXECD_ACCESS_TOKEN_ENV: "spoofed"},
            resource_limits={"cpu": "500m", "memory": "512Mi"},
            execd_image="execd:test",
            sandbox_id="agent-sbx-id",
        )
        main_env = {e["name"]: e["value"] for e in pod_spec["containers"][0]["env"]}
        assert main_env[EXECD_ACCESS_TOKEN_ENV].startswith("sha256:v1:")

        agent_workload = {"spec": {"podTemplate": {"spec": pod_spec}}}
        ep_agent = Endpoint(endpoint="agent-44772.example.com")
        _attach_execd_auth_headers(ep_agent, agent_workload, DEFAULT_EXECD_PORT, manager)
        assert ep_agent.headers is not None
        assert ep_agent.headers[OPEN_SANDBOX_EXECD_ACCESS_TOKEN_HEADER] == manager.resolve_raw_token(
            main_env[EXECD_ACCESS_TOKEN_ENV]
        )

        # Legacy plaintext workload still resolves even when manager is None
        legacy_workload = {
            "spec": {
                "template": {
                    "spec": {
                        "containers": [
                            {"env": [{"name": EXECD_ACCESS_TOKEN_ENV, "value": "old-plain-token"}]}
                        ]
                    }
                }
            }
        }
        ep_legacy = Endpoint(endpoint="legacy-44772.example.com")
        _attach_execd_auth_headers(ep_legacy, legacy_workload, DEFAULT_EXECD_PORT, None)
        assert ep_legacy.headers == {OPEN_SANDBOX_EXECD_ACCESS_TOKEN_HEADER: "old-plain-token"}

    def test_lifecycle_rejects_server_proxy_for_execd_when_enabled(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        from opensandbox_server.api import lifecycle

        key_file = _write_key_file(tmp_path / "keys.json", "v1", {"v1": _gen_master_key_b64()})
        cfg = AppConfig(
            runtime=RuntimeConfig(type="kubernetes", execd_image="execd:test"),
            kubernetes=KubernetesRuntimeConfig(namespace="test-ns"),
            execd=ExecdConfig(auth_signing_key_file=str(key_file)),
        )
        monkeypatch.setattr(lifecycle, "get_config", lambda: cfg)

        with pytest.raises(HTTPException) as exc_info:
            lifecycle.get_sandbox_endpoint(
                request=MagicMock(),
                sandbox_id="sbx-1",
                port=DEFAULT_EXECD_PORT,
                use_server_proxy=True,
                expires=None,
            )
        assert exc_info.value.status_code == 400
        assert "use_server_proxy is not supported" in exc_info.value.detail["message"]

    def test_docker_runtime_rejects_execd_auth_signing_key_file(self) -> None:
        with pytest.raises(ValueError, match="only supported when runtime.type = 'kubernetes'"):
            AppConfig(
                runtime=RuntimeConfig(type="docker", execd_image="execd:test"),
                execd=ExecdConfig(auth_signing_key_file="/var/run/secrets/keys.json"),
            )

    def test_windows_profile_rejected_when_execd_auth_enabled(self, tmp_path: Path) -> None:
        from opensandbox_server.api.schema import PlatformSpec

        manager = self._make_manager(tmp_path)
        app_config = AppConfig(
            runtime=RuntimeConfig(type="kubernetes", execd_image="execd:test"),
            kubernetes=KubernetesRuntimeConfig(namespace="test-ns"),
        )
        provider = BatchSandboxProvider(MagicMock(), app_config=app_config)
        provider.set_execd_token_manager(manager)

        with pytest.raises(ValueError, match="Windows sandbox profile is not supported"):
            provider.create_workload(
                sandbox_id="win-sbx-id",
                namespace="test-ns",
                image_spec=ImageSpec(uri="dockurr/windows:latest"),
                entrypoint=["/run/entry.sh"],
                env={},
                resource_limits={"cpu": "2", "memory": "4Gi"},
                labels={"opensandbox.io/id": "win-sbx-id"},
                expires_at=datetime(2026, 12, 1, tzinfo=timezone.utc),
                execd_image="execd:test",
                platform=PlatformSpec(os="windows", arch="amd64"),
            )

    def test_proxy_never_auto_injects_execd_token_and_rejects_execd_port(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        from opensandbox_server.api import proxy

        # _filter_proxy_headers must never inject X-EXECD-ACCESS-TOKEN from endpoint headers
        filtered = proxy._filter_proxy_headers(
            {},
            {OPEN_SANDBOX_EXECD_ACCESS_TOKEN_HEADER: "derived-secret"},
        )
        assert OPEN_SANDBOX_EXECD_ACCESS_TOKEN_HEADER not in filtered

        key_file = _write_key_file(tmp_path / "keys.json", "v1", {"v1": _gen_master_key_b64()})
        cfg = AppConfig(
            runtime=RuntimeConfig(type="kubernetes", execd_image="execd:test"),
            kubernetes=KubernetesRuntimeConfig(namespace="test-ns"),
            execd=ExecdConfig(auth_signing_key_file=str(key_file)),
        )
        monkeypatch.setattr(proxy, "get_config", lambda: cfg)

        with pytest.raises(HTTPException) as exc_info:
            proxy._reject_proxy_if_execd_auth_enabled(DEFAULT_EXECD_PORT)
        assert exc_info.value.status_code == 400

        # Non-execd ports are unaffected
        proxy._reject_proxy_if_execd_auth_enabled(8080)

    def test_create_sandbox_rejects_client_execd_access_token_env_when_enabled(
        self, tmp_path: Path
    ) -> None:
        from opensandbox_server.api.schema import CreateSandboxRequest, ResourceLimits
        from opensandbox_server.services.k8s.kubernetes_service import KubernetesSandboxService

        manager = self._make_manager(tmp_path)
        svc = KubernetesSandboxService.__new__(KubernetesSandboxService)
        svc._execd_token_manager = manager

        req_with_token = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            entrypoint=["sleep", "3600"],
            resourceLimits=ResourceLimits(root={"cpu": "500m", "memory": "512Mi"}),
            env={EXECD_ACCESS_TOKEN_ENV: "client-supplied-token"},
        )
        with pytest.raises(HTTPException) as exc_info:
            svc._ensure_execd_auth_env_allowed(req_with_token)
        assert exc_info.value.status_code == 400
        assert EXECD_ACCESS_TOKEN_ENV in exc_info.value.detail["message"]

        # Request without EXECD_ACCESS_TOKEN is allowed
        req_clean = CreateSandboxRequest(
            image=ImageSpec(uri="python:3.11"),
            entrypoint=["sleep", "3600"],
            resourceLimits=ResourceLimits(root={"cpu": "500m", "memory": "512Mi"}),
            env={"OTHER_VAR": "ok"},
        )
        svc._ensure_execd_auth_env_allowed(req_clean)

        # When execd auth is disabled, client-supplied EXECD_ACCESS_TOKEN is allowed
        svc._execd_token_manager = None
        svc._ensure_execd_auth_env_allowed(req_with_token)
