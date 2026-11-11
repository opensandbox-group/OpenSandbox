# Copyright 2025 The OpenSandbox Authors
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

import textwrap

import pytest
from pydantic import SecretStr, ValidationError

from opensandbox_server import config as config_module
from opensandbox_server.cli import render_full_config
from opensandbox_server.config import (
    AppConfig,
    LogConfig,
    RenewIntentRedisConfig,
    EGRESS_MODE_DNS,
    EGRESS_MODE_DNS_NFT,
    EgressConfig,
    EgressUpstreamProxyConfig,
    ExecdInitResources,
    GatewayConfig,
    GatewayRouteModeConfig,
    IngressConfig,
    RuntimeConfig,
    SecureAccessConfig,
    SecureAccessKey,
    ServerConfig,
    POSTGRESQL_DSN_ENV_VAR,
    PostgreSQLStoreConfig,
    StoreConfig,
    StorageConfig,
)


def _reset_config(monkeypatch):
    monkeypatch.setattr(config_module, "_config", None, raising=False)
    monkeypatch.setattr(config_module, "_config_path", None, raising=False)


def test_load_config_from_file(tmp_path, monkeypatch):
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000
        api_key = "secret"
        max_sandbox_timeout_seconds = 172800

        [log]
        level = "DEBUG"

        [runtime]
        type = "kubernetes"
        execd_image = "opensandbox/execd:test"

        [ingress]
        mode = "gateway"
        gateway.address = "*.opensandbox.io"
        gateway.route.mode = "wildcard"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.server.host == "127.0.0.1"
    assert loaded.server.port == 9000
    assert loaded.log.level == "DEBUG"
    assert loaded.server.api_key == "secret"
    assert loaded.server.max_sandbox_timeout_seconds == 172800
    assert loaded.runtime.type == "kubernetes"
    assert loaded.runtime.execd_image == "opensandbox/execd:test"
    assert loaded.runtime.execd_run_as_init is False
    assert loaded.ingress is not None
    assert loaded.ingress.mode == "gateway"
    assert loaded.ingress.gateway is not None
    assert loaded.ingress.gateway.address == "*.opensandbox.io"
    assert loaded.ingress.gateway.route.mode == "wildcard"
    assert loaded.kubernetes is not None


def test_load_config_env_override_api_key(tmp_path, monkeypatch):
    """OPENSANDBOX_SERVER_API_KEY should override server.api_key from TOML."""
    _reset_config(monkeypatch)
    monkeypatch.setenv("OPENSANDBOX_SERVER_API_KEY", "env-secret-key")
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000
        api_key = "toml-secret-key"

        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.server.api_key == "env-secret-key"


def test_load_config_env_api_key_without_toml_key(tmp_path, monkeypatch):
    """OPENSANDBOX_SERVER_API_KEY should work even when TOML omits api_key."""
    _reset_config(monkeypatch)
    monkeypatch.setenv("OPENSANDBOX_SERVER_API_KEY", "env-only-key")
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.server.api_key == "env-only-key"


def test_load_config_without_env_uses_toml_api_key(tmp_path, monkeypatch):
    """When OPENSANDBOX_SERVER_API_KEY is unset, TOML api_key should be used."""
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000
        api_key = "toml-secret-key"

        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.server.api_key == "toml-secret-key"


def test_runtime_execd_run_as_init_parses_from_toml(tmp_path):
    toml = """
        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"
        execd_run_as_init = true
        """
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.runtime.execd_run_as_init is True


def test_runtime_execd_run_as_init_defaults_false(tmp_path):
    toml = """
        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"
        """
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.runtime.execd_run_as_init is False


def test_docker_runtime_disallows_kubernetes_block():
    server_cfg = ServerConfig()
    runtime_cfg = RuntimeConfig(type="docker", execd_image="busybox:latest")
    kubernetes_cfg = config_module.KubernetesRuntimeConfig(namespace="sandbox")
    with pytest.raises(ValueError):
        AppConfig(server=server_cfg, runtime=runtime_cfg, kubernetes=kubernetes_cfg)


def test_server_config_defaults_include_max_sandbox_timeout():
    server_cfg = ServerConfig()
    assert server_cfg.max_sandbox_timeout_seconds is None


def test_kubernetes_pool_acquisition_timeout_defaults_and_validates():
    kubernetes_cfg = config_module.KubernetesRuntimeConfig()
    assert kubernetes_cfg.pool_acquisition_timeout_seconds == 30

    with pytest.raises(ValueError):
        config_module.KubernetesRuntimeConfig(pool_acquisition_timeout_seconds=0)


def test_kubernetes_template_publish_secrets_defaults():
    """native templates fall back to the S3 secret; the OCI publish secret
    for overlaybd templates is opt-in and defaults to unset."""
    kubernetes_cfg = config_module.KubernetesRuntimeConfig()
    assert kubernetes_cfg.template_s3_publish_secret == "sandbox-oss-credentials"
    assert kubernetes_cfg.template_oci_publish_secret is None

    configured = config_module.KubernetesRuntimeConfig(
        template_oci_publish_secret="registry-credentials"
    )
    assert configured.template_oci_publish_secret == "registry-credentials"

    with pytest.raises(ValueError):
        config_module.KubernetesRuntimeConfig(template_oci_publish_secret="")


def test_kubernetes_template_oci_publish_secret_from_toml():
    app_cfg = config_module.AppConfig.model_validate(
        {
            "server": {"host": "127.0.0.1", "port": 8000},
            "runtime": {"type": "kubernetes", "execd_image": "opensandbox/execd:latest"},
            "kubernetes": {
                "template_oci_publish_secret": "registry-credentials",
            },
        }
    )
    assert app_cfg.kubernetes is not None
    assert app_cfg.kubernetes.template_oci_publish_secret == "registry-credentials"


def test_server_config_uvicorn_tuning_defaults():
    """ServerConfig exposes uvicorn concurrency knobs with sensible defaults."""
    server_cfg = ServerConfig()
    assert server_cfg.limit_concurrency == 1024
    assert server_cfg.backlog == 2048
    assert server_cfg.thread_pool_size == 200
    assert server_cfg.loop == "auto"
    assert server_cfg.http == "auto"


def test_server_config_uvicorn_tuning_overrides():
    server_cfg = ServerConfig(
        limit_concurrency=256,
        backlog=4096,
        loop="uvloop",
        http="httptools",
    )
    assert server_cfg.limit_concurrency == 256
    assert server_cfg.backlog == 4096
    assert server_cfg.loop == "uvloop"
    assert server_cfg.http == "httptools"


def test_server_config_limit_concurrency_zero_disables_cap():
    """0 is the TOML-friendly disable sentinel and must collapse to None so
    uvicorn applies no concurrency limit."""
    cfg = ServerConfig(limit_concurrency=0)
    assert cfg.limit_concurrency is None


def test_server_config_limit_concurrency_accepts_none_and_positive():
    cfg = ServerConfig(limit_concurrency=None)
    assert cfg.limit_concurrency is None
    cfg = ServerConfig(limit_concurrency=512)
    assert cfg.limit_concurrency == 512


def test_server_config_limit_concurrency_rejects_negative():
    with pytest.raises(ValidationError):
        ServerConfig(limit_concurrency=-1)


def test_server_config_backlog_must_be_positive():
    with pytest.raises(ValidationError):
        ServerConfig(backlog=0)


def test_server_config_thread_pool_size_must_be_positive():
    with pytest.raises(ValidationError):
        ServerConfig(thread_pool_size=0)
    cfg = ServerConfig(thread_pool_size=512)
    assert cfg.thread_pool_size == 512


def test_server_config_loop_and_http_reject_unknown_values():
    with pytest.raises(ValidationError):
        ServerConfig(loop="trio")  # type: ignore[arg-type]
    with pytest.raises(ValidationError):
        ServerConfig(http="hyper")  # type: ignore[arg-type]


def test_store_defaults_to_sqlite():
    cfg = StoreConfig()
    assert cfg.type == "sqlite"
    assert cfg.path.endswith("opensandbox.db")


def test_postgresql_store_requires_dsn():
    with pytest.raises(ValidationError, match="must be set"):
        StoreConfig(type="postgresql")


def test_postgresql_store_validates_pool_size():
    with pytest.raises(ValidationError, match="min_pool_size"):
        PostgreSQLStoreConfig(
            dsn=SecretStr("postgresql://localhost/opensandbox"),
            min_pool_size=3,
            max_pool_size=2,
        )


def test_postgresql_store_snapshot_recovery_interval_is_positive():
    config = PostgreSQLStoreConfig(
        dsn=SecretStr("postgresql://localhost/opensandbox"),
    )

    assert config.snapshot_recovery_interval_seconds == 15

    with pytest.raises(ValueError):
        PostgreSQLStoreConfig(
            dsn=SecretStr("postgresql://localhost/opensandbox"),
            snapshot_recovery_interval_seconds=0,
        )


def test_renew_intent_defaults():
    cfg = AppConfig(runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:latest"))
    ar = cfg.renew_intent
    assert ar.enabled is False
    assert ar.min_interval_seconds == 60
    assert ar.redis.enabled is False
    assert ar.redis.dsn is None
    assert ar.redis.queue_key == "opensandbox:renew:intent"
    assert ar.redis.consumer_concurrency == 8


def test_idle_pause_defaults():
    cfg = AppConfig(runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:latest"))
    ip = cfg.automation.idle_pause
    assert ip.enabled is False
    assert ip.sweep_interval_seconds == 15
    assert ip.activity_ttl_seconds == 1800
    assert ip.redis.dsn is None
    assert ip.redis.lock_ttl_seconds == 30


def test_idle_pause_redis_requires_dsn_when_enabled():
    from opensandbox_server.config import IdlePauseConfig

    with pytest.raises(ValidationError):
        IdlePauseConfig(enabled=True)
    with pytest.raises(ValidationError):
        IdlePauseConfig(enabled=True, redis={"dsn": "   "})
    ok = IdlePauseConfig(enabled=True, redis={"dsn": "redis://x", "lock_ttl_seconds": 45})
    assert ok.redis.lock_ttl_seconds == 45


def test_load_config_idle_pause_dotted_redis_keys(tmp_path, monkeypatch):
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [automation.idle_pause]
        enabled = true
        sweep_interval_seconds = 10
        activity_ttl_seconds = 3600
        redis.dsn = "redis://example:6379/2"
        redis.lock_ttl_seconds = 45

        [runtime]
        type = "kubernetes"
        execd_image = "opensandbox/execd:test"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    ip = loaded.automation.idle_pause
    assert ip.enabled is True
    assert ip.sweep_interval_seconds == 10
    assert ip.activity_ttl_seconds == 3600
    assert ip.redis.dsn == "redis://example:6379/2"
    assert ip.redis.lock_ttl_seconds == 45


def test_renew_intent_redis_requires_dsn_when_enabled():
    with pytest.raises(ValidationError):
        RenewIntentRedisConfig(enabled=True, dsn=None)
    with pytest.raises(ValidationError):
        RenewIntentRedisConfig(enabled=True, dsn="   ")
    cfg = RenewIntentRedisConfig(enabled=True, dsn="redis://127.0.0.1:6379/0")
    assert cfg.dsn == "redis://127.0.0.1:6379/0"


def test_load_config_renew_intent_dotted_redis_keys(tmp_path, monkeypatch):
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [renew_intent]
        enabled = true
        min_interval_seconds = 30
        redis.enabled = true
        redis.dsn = "redis://example:6379/1"
        redis.queue_key = "custom:renew"
        redis.consumer_concurrency = 4

        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    ar = loaded.renew_intent
    assert ar.enabled is True
    assert ar.min_interval_seconds == 30
    assert ar.redis.enabled is True
    assert ar.redis.dsn == "redis://example:6379/1"
    assert ar.redis.queue_key == "custom:renew"
    assert ar.redis.consumer_concurrency == 4


def test_load_config_store_block(tmp_path, monkeypatch):
    _reset_config(monkeypatch)
    db_path = tmp_path / "snapshots.sqlite3"
    escaped_db_path = str(db_path).replace("\\", "\\\\").replace('"', '\\"')
    toml = textwrap.dedent(
        f"""
        [store]
        type = "sqlite"
        path = "{escaped_db_path}"

        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.store.type == "sqlite"
    assert loaded.store.path == str(db_path)


def test_load_config_postgresql_dsn_from_environment(tmp_path, monkeypatch):
    _reset_config(monkeypatch)
    monkeypatch.setenv(
        POSTGRESQL_DSN_ENV_VAR,
        "postgresql://opensandbox:secret@postgres.example/opensandbox",
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(
        textwrap.dedent(
            """
            [store]
            type = "postgresql"

            [store.postgresql]
            min_pool_size = 2
            max_pool_size = 4

            [runtime]
            type = "docker"
            execd_image = "opensandbox/execd:test"
            """
        )
    )

    loaded = config_module.load_config(config_path)

    assert loaded.store.type == "postgresql"
    assert loaded.store.postgresql.dsn is not None
    assert (
        loaded.store.postgresql.dsn.get_secret_value()
        == "postgresql://opensandbox:secret@postgres.example/opensandbox"
    )
    assert loaded.store.postgresql.min_pool_size == 2
    assert loaded.store.postgresql.max_pool_size == 4


def test_postgresql_dsn_environment_overrides_toml(tmp_path, monkeypatch):
    _reset_config(monkeypatch)
    monkeypatch.setenv(POSTGRESQL_DSN_ENV_VAR, "postgresql://environment/opensandbox")
    config_path = tmp_path / "config.toml"
    config_path.write_text(
        textwrap.dedent(
            """
            [store]
            type = "postgresql"

            [store.postgresql]
            dsn = "postgresql://toml/opensandbox"

            [runtime]
            type = "docker"
            execd_image = "opensandbox/execd:test"
            """
        )
    )

    loaded = config_module.load_config(config_path)

    assert loaded.store.postgresql.dsn is not None
    assert loaded.store.postgresql.dsn.get_secret_value() == "postgresql://environment/opensandbox"


def test_postgresql_dsn_is_redacted_from_validation_errors(tmp_path, monkeypatch):
    _reset_config(monkeypatch)
    secret_dsn = "postgresql://opensandbox:sensitive-password@postgres/opensandbox"
    monkeypatch.setenv(POSTGRESQL_DSN_ENV_VAR, secret_dsn)
    config_path = tmp_path / "config.toml"
    config_path.write_text(
        textwrap.dedent(
            """
            [store]
            type = "postgresql"

            [store.postgresql]
            min_pool_size = 3
            max_pool_size = 2

            [runtime]
            type = "docker"
            execd_image = "opensandbox/execd:test"
            """
        )
    )

    with pytest.raises(ValidationError) as exc_info:
        config_module.load_config(config_path)

    assert secret_dsn not in str(exc_info.value)


def test_full_config_includes_store_sections(tmp_path) -> None:
    config_path = render_full_config(tmp_path / "generated.toml")

    content = config_path.read_text()
    assert "[store]" in content
    assert "[store.postgresql]" in content
    assert "min_pool_size" in content


def test_full_config_documents_container_runtime_network_modes(tmp_path) -> None:
    """`osb config init` renders pydantic field descriptions, so they must list every accepted mode."""
    config_path = render_full_config(tmp_path / "generated.toml")

    content = config_path.read_text()
    for mode in ("host", "bridge", "none", "pasta", "slirp4netns"):
        assert mode in content


def test_load_config_renew_intent_legacy_redis_subtable(tmp_path, monkeypatch):
    """[renew_intent.redis] remains accepted (same parsed shape as dotted keys)."""
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [renew_intent]
        enabled = true

        [renew_intent.redis]
        enabled = true
        dsn = "redis://legacy:6379/0"

        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.renew_intent.redis.enabled is True
    assert loaded.renew_intent.redis.dsn == "redis://legacy:6379/0"


def test_load_config_ignores_legacy_pause_block(tmp_path, monkeypatch):
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [runtime]
        type = "kubernetes"
        execd_image = "opensandbox/execd:test"

        [pause]
        snapshot_registry = "registry.example.com/sandboxes"
        snapshot_push_secret = "registry-snapshot-push-secret"
        resume_pull_secret = "registry-pull-secret"
        snapshot_type = "Rootfs"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.runtime.type == "kubernetes"
    assert not hasattr(loaded, "pause")


def test_kubernetes_runtime_fills_missing_block():
    server_cfg = ServerConfig()
    runtime_cfg = RuntimeConfig(type="kubernetes", execd_image="opensandbox/execd:latest")
    app_cfg = AppConfig(server=server_cfg, runtime=runtime_cfg)
    assert app_cfg.kubernetes is not None


def test_ingress_gateway_requires_gateway_block():
    with pytest.raises(ValueError):
        IngressConfig(mode="gateway")
    cfg = IngressConfig(
        mode="gateway",
        gateway=GatewayConfig(
            address="gateway.opensandbox.io",
            route=GatewayRouteModeConfig(mode="uri"),
        ),
    )
    assert cfg.gateway.route.mode == "uri"


def test_gateway_address_validation_for_wildcard_mode():
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="gateway.opensandbox.io",
                route=GatewayRouteModeConfig(mode="wildcard"),
            ),
        )
    cfg = IngressConfig(
        mode="gateway",
        gateway=GatewayConfig(
            address="*.opensandbox.io",
            route=GatewayRouteModeConfig(mode="wildcard"),
        ),
    )
    assert cfg.gateway.address == "*.opensandbox.io"
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="10.0.0.1",
                route=GatewayRouteModeConfig(mode="wildcard"),
            ),
        )
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="http://10.0.0.1:8080",
                route=GatewayRouteModeConfig(mode="wildcard"),
            ),
        )
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="10.0.0.1:8080",
                route=GatewayRouteModeConfig(mode="wildcard"),
            ),
        )
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="https://*.opensandbox.io",
                route=GatewayRouteModeConfig(mode="wildcard"),
            ),
        )


def test_gateway_route_mode_allows_wildcard_alias():
    cfg = IngressConfig(
        mode="gateway",
        gateway=GatewayConfig(
            address="*.opensandbox.io",
            route=GatewayRouteModeConfig(mode="wildcard"),
        ),
    )
    assert cfg.gateway.route.mode == "wildcard"


def test_gateway_address_validation_for_non_wildcard_mode():
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="*.opensandbox.io",
                route=GatewayRouteModeConfig(mode="header"),
            ),
        )
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="*.opensandbox.io",
                route=GatewayRouteModeConfig(mode="uri"),
            ),
        )
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="10.0.0.1:70000",
                route=GatewayRouteModeConfig(mode="header"),
            ),
        )
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="ftp://gateway.opensandbox.io",
                route=GatewayRouteModeConfig(mode="header"),
            ),
        )
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="http://",
                route=GatewayRouteModeConfig(mode="header"),
            ),
        )
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="http://user:pass@gateway.opensandbox.io",
                route=GatewayRouteModeConfig(mode="header"),
            ),
        )
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="http://gateway.opensandbox.io:8080",
                route=GatewayRouteModeConfig(mode="header"),
            ),
        )
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="http://gateway.opensandbox.io:8080",
                route=GatewayRouteModeConfig(mode="uri"),
            ),
        )
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="http://[::1]",
                route=GatewayRouteModeConfig(mode="header"),
            ),
        )
    cfg = IngressConfig(
        mode="gateway",
        gateway=GatewayConfig(
            address="gateway.opensandbox.io",
            route=GatewayRouteModeConfig(mode="uri"),
        ),
    )
    assert cfg.gateway.address == "gateway.opensandbox.io"
    cfg_hostname = IngressConfig(
        mode="gateway",
        gateway=GatewayConfig(
            address="gateway",
            route=GatewayRouteModeConfig(mode="header"),
        ),
    )
    assert cfg_hostname.gateway.address == "gateway"
    cfg_hostname_port = IngressConfig(
        mode="gateway",
        gateway=GatewayConfig(
            address="gateway.opensandbox.io:8080",
            route=GatewayRouteModeConfig(mode="header"),
        ),
    )
    assert cfg_hostname_port.gateway.address == "gateway.opensandbox.io:8080"
    cfg_ip = IngressConfig(
        mode="gateway",
        gateway=GatewayConfig(
            address="10.0.0.1",
            route=GatewayRouteModeConfig(mode="header"),
        ),
    )
    assert cfg_ip.gateway.address == "10.0.0.1"
    cfg_ip_port = IngressConfig(
        mode="gateway",
        gateway=GatewayConfig(
            address="10.0.0.1:8080",
            route=GatewayRouteModeConfig(mode="header"),
        ),
    )
    assert cfg_ip_port.gateway.address == "10.0.0.1:8080"
    cfg_uri_freeform = IngressConfig(
        mode="gateway",
        gateway=GatewayConfig(
            address="not a host",
            route=GatewayRouteModeConfig(mode="uri"),
        ),
    )
    assert cfg_uri_freeform.gateway.address == "not a host"
    cfg_uri_port_like = IngressConfig(
        mode="gateway",
        gateway=GatewayConfig(
            address="10.0.0.1:abc",
            route=GatewayRouteModeConfig(mode="uri"),
        ),
    )
    assert cfg_uri_port_like.gateway.address == "10.0.0.1:abc"


@pytest.mark.parametrize("whitespace", ["\n", "\r\n", "\r", "\t", " "])
@pytest.mark.parametrize(
    "address,route_mode",
    [
        ("gateway.opensandbox.io", "header"),
        ("gateway.opensandbox.io:8080", "header"),
        ("10.0.0.1", "header"),
        ("10.0.0.1:8080", "header"),
        ("*.opensandbox.io", "wildcard"),
    ],
    ids=["hostname", "hostname-port", "ip", "ip-port", "wildcard"],
)
def test_gateway_address_rejects_trailing_whitespace(address, route_mode, whitespace):
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address=address + whitespace,
                route=GatewayRouteModeConfig(mode=route_mode),
            ),
        )


@pytest.mark.parametrize(
    "address,route_mode",
    [
        ("gateway .opensandbox.io", "header"),
        ("gateway.opensandbox.io\nx", "header"),
        ("10.0.0.1\n:8080", "header"),
        ("*.opensandbox.io\nx", "wildcard"),
    ],
    ids=["hostname-space", "hostname-newline", "ip-newline-port", "wildcard-newline"],
)
def test_gateway_address_rejects_embedded_whitespace(address, route_mode):
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address=address,
                route=GatewayRouteModeConfig(mode=route_mode),
            ),
        )


def test_gateway_address_allows_scheme_less_defaults():
    cfg = IngressConfig(
        mode="gateway",
        gateway=GatewayConfig(
            address="*.example.com",
            route=GatewayRouteModeConfig(mode="wildcard"),
        ),
    )
    assert cfg.gateway.address == "*.example.com"
    with pytest.raises(ValueError):
        IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="https://*.example.com",
                route=GatewayRouteModeConfig(mode="wildcard"),
            ),
        )


def test_direct_mode_rejects_gateway_block():
    with pytest.raises(ValueError):
        IngressConfig(
            mode="direct",
            gateway=GatewayConfig(
                address="gateway.opensandbox.io",
                route=GatewayRouteModeConfig(mode="header"),
            ),
        )


def test_docker_runtime_rejects_gateway_ingress():
    server_cfg = ServerConfig()
    runtime_cfg = RuntimeConfig(type="docker", execd_image="busybox:latest")
    with pytest.raises(ValueError):
        AppConfig(
            server=server_cfg,
            runtime=runtime_cfg,
            ingress=IngressConfig(
                mode="gateway",
                gateway=GatewayConfig(
                    address="gateway.opensandbox.io",
                    route=GatewayRouteModeConfig(mode="header"),
                ),
            ),
        )
    # direct remains valid
    app_cfg = AppConfig(
        server=server_cfg,
        runtime=runtime_cfg,
        ingress=IngressConfig(mode="direct"),
    )
    assert app_cfg.ingress.mode == "direct"


def test_storage_config_defaults():
    """StorageConfig should default to empty allowed_host_paths list."""
    cfg = StorageConfig()
    assert cfg.allowed_host_paths == []


def test_storage_config_with_paths():
    """StorageConfig should accept explicit allowed_host_paths."""
    cfg = StorageConfig(allowed_host_paths=["/data/opensandbox", "/tmp/sandbox"])
    assert cfg.allowed_host_paths == ["/data/opensandbox", "/tmp/sandbox"]


def test_app_config_default_storage():
    """AppConfig should include default StorageConfig when not specified."""
    server_cfg = ServerConfig()
    runtime_cfg = RuntimeConfig(type="docker", execd_image="busybox:latest")
    app_cfg = AppConfig(server=server_cfg, runtime=runtime_cfg)
    assert app_cfg.storage is not None
    assert app_cfg.storage.allowed_host_paths == []


def test_load_config_with_storage_block(tmp_path, monkeypatch):
    """StorageConfig should be loaded from [storage] TOML block."""
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [runtime]
        type = "docker"
        execd_image = "ghcr.io/opensandbox/platform:test"

        [router]
        domain = "opensandbox.io"

        [storage]
        allowed_host_paths = ["/data/opensandbox", "/tmp/sandbox"]
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.storage is not None
    assert loaded.storage.allowed_host_paths == ["/data/opensandbox", "/tmp/sandbox"]


def test_load_config_without_storage_block_uses_defaults(tmp_path, monkeypatch):
    """AppConfig should use default StorageConfig when [storage] is not in TOML."""
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [runtime]
        type = "docker"
        execd_image = "ghcr.io/opensandbox/platform:test"

        [router]
        domain = "opensandbox.io"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.storage is not None
    assert loaded.storage.allowed_host_paths == []


def test_secure_runtime_empty_type_is_valid():
    """Empty type (default runc) should be valid."""
    cfg = config_module.SecureRuntimeConfig(type="")
    assert cfg.type == ""
    assert cfg.docker_runtime is None
    assert cfg.k8s_runtime_class is None


def test_secure_runtime_gvisor_with_docker_runtime_is_valid():
    cfg = config_module.SecureRuntimeConfig(
        type="gvisor",
        docker_runtime="runsc",
        k8s_runtime_class="gvisor",
    )
    assert cfg.type == "gvisor"
    assert cfg.docker_runtime == "runsc"
    assert cfg.k8s_runtime_class == "gvisor"


def test_secure_runtime_gvisor_with_k8s_runtime_class_is_valid():
    """gVisor with only k8s_runtime_class should be valid."""
    cfg = config_module.SecureRuntimeConfig(
        type="gvisor",
        docker_runtime=None,
        k8s_runtime_class="gvisor",
    )
    assert cfg.type == "gvisor"
    assert cfg.docker_runtime is None
    assert cfg.k8s_runtime_class == "gvisor"


def test_secure_runtime_kata_with_runtimes_is_valid():
    cfg = config_module.SecureRuntimeConfig(
        type="kata",
        docker_runtime="kata-runtime",
        k8s_runtime_class="kata-qemu",
    )
    assert cfg.type == "kata"
    assert cfg.docker_runtime == "kata-runtime"
    assert cfg.k8s_runtime_class == "kata-qemu"


def test_secure_runtime_firecracker_with_k8s_runtime_is_valid():
    cfg = config_module.SecureRuntimeConfig(
        type="firecracker",
        docker_runtime="",
        k8s_runtime_class="kata-fc",
    )
    assert cfg.type == "firecracker"
    assert cfg.docker_runtime == ""
    assert cfg.k8s_runtime_class == "kata-fc"


def test_secure_runtime_firecracker_without_k8s_runtime_raises_error():
    with pytest.raises(ValueError) as exc:
        config_module.SecureRuntimeConfig(
            type="firecracker",
            docker_runtime="",
            k8s_runtime_class=None,
        )
    assert "k8s_runtime_class" in str(exc.value).lower()


def test_secure_runtime_gvisor_without_any_runtime_raises_error():
    with pytest.raises(ValueError) as exc:
        config_module.SecureRuntimeConfig(
            type="gvisor",
            docker_runtime=None,
            k8s_runtime_class=None,
        )
    assert "docker_runtime" in str(exc.value).lower() or "k8s_runtime_class" in str(exc.value).lower()


def test_secure_runtime_kata_without_any_runtime_raises_error():
    with pytest.raises(ValueError) as exc:
        config_module.SecureRuntimeConfig(
            type="kata",
            docker_runtime=None,
            k8s_runtime_class=None,
        )
    assert "docker_runtime" in str(exc.value).lower() or "k8s_runtime_class" in str(exc.value).lower()


def test_secure_runtime_invalid_type_raises_error():
    with pytest.raises(Exception):
        config_module.SecureRuntimeConfig(type="invalid_runtime")


def test_app_config_with_secure_runtime():
    cfg = AppConfig(
        runtime={"type": "docker", "execd_image": "execd:v1"},
        secure_runtime={
            "type": "gvisor",
            "docker_runtime": "runsc",
            "k8s_runtime_class": "gvisor",
        },
    )
    assert cfg.secure_runtime is not None
    assert cfg.secure_runtime.type == "gvisor"
    assert cfg.secure_runtime.docker_runtime == "runsc"


def test_app_config_without_secure_runtime():
    cfg = AppConfig(
        runtime={"type": "docker", "execd_image": "execd:v1"},
    )
    assert cfg.secure_runtime is None


def test_load_config_with_secure_runtime(tmp_path, monkeypatch):
    """SecureRuntimeConfig should be loaded from [secure_runtime] TOML block."""
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [runtime]
        type = "docker"
        execd_image = "ghcr.io/opensandbox/platform:test"

        [secure_runtime]
        type = "gvisor"
        docker_runtime = "runsc"
        k8s_runtime_class = "gvisor"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.secure_runtime is not None
    assert loaded.secure_runtime.type == "gvisor"
    assert loaded.secure_runtime.docker_runtime == "runsc"
    assert loaded.secure_runtime.k8s_runtime_class == "gvisor"


def test_docker_runtime_with_firecracker_raises_error():
    """Docker runtime with Firecracker secure runtime should raise error.

    Firecracker (kata-fc) is only available as a Kubernetes RuntimeClass,
    not as a Docker OCI runtime. This test prevents the silent fallback
    to runc which would bypass the intended microVM isolation.
    """
    with pytest.raises(ValueError) as exc:
        AppConfig(
            runtime={"type": "docker", "execd_image": "execd:v1"},
            secure_runtime={
                "type": "firecracker",
                "k8s_runtime_class": "kata-fc",
            },
        )
    assert "firecracker" in str(exc.value).lower()
    assert "kubernetes" in str(exc.value).lower()


def test_kubernetes_runtime_with_firecracker_is_valid():
    cfg = AppConfig(
        runtime={"type": "kubernetes", "execd_image": "execd:v1"},
        kubernetes={"namespace": "default"},
        secure_runtime={
            "type": "firecracker",
            "k8s_runtime_class": "kata-fc",
        },
    )
    assert cfg.runtime.type == "kubernetes"
    assert cfg.secure_runtime is not None
    assert cfg.secure_runtime.type == "firecracker"
    assert cfg.secure_runtime.k8s_runtime_class == "kata-fc"


def test_egress_config_mode_literal():
    base = EgressConfig(image="opensandbox/egress:v1")
    assert base.mode == EGRESS_MODE_DNS
    assert base.disable_ipv6 is True
    assert base.readiness_timeout_seconds == 30.0
    assert base.requests is None
    assert base.limits is None
    cfg = EgressConfig(image="opensandbox/egress:v1", mode=EGRESS_MODE_DNS_NFT)
    assert cfg.mode == EGRESS_MODE_DNS_NFT


def test_egress_config_otlp_endpoint_default_and_http_urls():
    base = EgressConfig(image="opensandbox/egress:v1")
    assert base.otlp_endpoint is None
    for endpoint in (
        "http://otel-collector.observability:4318",
        "https://otel-collector.observability:4318",
    ):
        cfg = EgressConfig(image="opensandbox/egress:v1", otlp_endpoint=endpoint)
        assert cfg.otlp_endpoint == endpoint


def test_egress_config_empty_otlp_endpoint_normalizes_to_none():
    cfg = EgressConfig(image="opensandbox/egress:v1", otlp_endpoint="")
    assert cfg.otlp_endpoint is None


@pytest.mark.parametrize(
    "endpoint",
    [
        "grpc://otel-collector:4317",
        "otel-collector.observability:4318",
        "unix:///var/run/otel.sock",
        "http://",
        "https://",
        "http:///v1/metrics",
        "https:///v1/metrics",
    ],
)
def test_egress_config_rejects_non_http_otlp_endpoint(endpoint):
    with pytest.raises(ValidationError, match=r"otlp_endpoint must be an http\(s\) URL"):
        EgressConfig(image="opensandbox/egress:v1", otlp_endpoint=endpoint)


@pytest.mark.parametrize(
    ("resource_name", "quantity"),
    [
        ("cpu", "1"),
        ("memory", "1Gi"),
        ("ephemeral-storage", "1Gi"),
        ("hugepages-2Mi", "2Mi"),
        ("nvidia.com/gpu", "1"),
    ],
)
def test_egress_config_accepts_kubernetes_container_resource_names(
    resource_name, quantity
):
    config = EgressConfig(
        requests={resource_name: quantity},
        limits={resource_name: quantity},
    )

    assert config.requests == {resource_name: quantity}


@pytest.mark.parametrize(
    "resource_name",
    [
        "",
        "gpu",
        "bad resource name",
        "hugepages-invalid",
        "/gpu",
        "example.com/",
        "Example.com/gpu",
        "example..com/gpu",
        "requests.example.com/gpu",
        f"{'a' * 64}.com/gpu",
        f"example.com/{'a' * 64}",
    ],
)
def test_egress_config_rejects_invalid_kubernetes_container_resource_names(
    resource_name,
):
    with pytest.raises(
        ValidationError,
        match=r"invalid Kubernetes container resource name",
    ):
        EgressConfig(requests={resource_name: "1"})


@pytest.mark.parametrize(
    ("requests", "limits"),
    [
        ({"cpu": "500m"}, {"cpu": "250m"}),
        ({"memory": "1Gi"}, {"memory": "512Mi"}),
    ],
)
def test_egress_config_rejects_requests_greater_than_limits(requests, limits):
    with pytest.raises(
        ValidationError,
        match=r"resource request .* must not exceed limit",
    ):
        EgressConfig(requests=requests, limits=limits)


def test_execd_init_resources_preserve_existing_unvalidated_quantity_semantics():
    resources = ExecdInitResources(requests={"cpu": "not-a-quantity"})

    assert resources.requests == {"cpu": "not-a-quantity"}


@pytest.mark.parametrize(
    "quantity",
    ["not-a-quantity", "-1", "NaN", "1K"],
)
@pytest.mark.parametrize("resource_kind", ["requests", "limits"])
def test_egress_config_rejects_invalid_kubernetes_resource_quantities(
    resource_kind, quantity
):
    with pytest.raises(
        ValidationError,
        match=r"invalid Kubernetes resource quantity for 'cpu'",
    ):
        EgressConfig.model_validate({resource_kind: {"cpu": quantity}})


def test_egress_config_readiness_timeout_must_be_positive():
    cfg = EgressConfig(readiness_timeout_seconds=75.5)
    assert cfg.readiness_timeout_seconds == 75.5

    with pytest.raises(ValidationError):
        EgressConfig(readiness_timeout_seconds=0)


def test_load_config_with_egress_readiness_timeout(tmp_path, monkeypatch):
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"

        [egress]
        image = "opensandbox/egress:test"
        readiness_timeout_seconds = 75.5
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)

    assert loaded.egress is not None
    assert loaded.egress.readiness_timeout_seconds == 75.5


def test_load_config_with_egress_resources(tmp_path, monkeypatch):
    _reset_config(monkeypatch)
    config_path = tmp_path / "config.toml"
    config_path.write_text(
        textwrap.dedent(
            """
            [runtime]
            type = "kubernetes"
            execd_image = "opensandbox/execd:test"

            [egress]
            image = "opensandbox/egress:test"
            requests = { cpu = "25m", memory = "64Mi" }
            limits = { cpu = "250m", memory = "256Mi" }
            """
        )
    )

    loaded = config_module.load_config(config_path)

    assert loaded.egress is not None
    assert loaded.egress.requests == {"cpu": "25m", "memory": "64Mi"}
    assert loaded.egress.limits == {"cpu": "250m", "memory": "256Mi"}


@pytest.mark.parametrize(
    ("resource_config", "error"),
    [
        ('requests = { gpu = "1" }', "invalid Kubernetes container resource name"),
        (
            'requests = { cpu = "500m" }\nlimits = { cpu = "250m" }',
            "must not exceed limit",
        ),
    ],
)
def test_load_config_rejects_invalid_egress_resources_at_startup(
    tmp_path, monkeypatch, resource_config, error
):
    _reset_config(monkeypatch)
    config_path = tmp_path / "config.toml"
    config_path.write_text(
        textwrap.dedent(
            f"""
            [runtime]
            type = "kubernetes"
            execd_image = "opensandbox/execd:test"

            [egress]
            {resource_config}
            """
        )
    )

    with pytest.raises(ValidationError, match=error):
        config_module.load_config(config_path)


# Expected results verified against the egress Go parseUpstreamProxy
# (components/egress/pkg/mitmproxy/upstream.go); `%C3%A9` is intentionally
# stricter than Go.
@pytest.mark.parametrize(
    "url",
    [
        "http://proxy.local:3128",
        "https://proxy.local",
        "http://[::1]:3128",
        "HTTP://proxy:3128",
        "Https://proxy",
        "http://pro_xy:3128",
        "http://pro!xy:3128",
        "http://proxy:",
        "http://proxy.local:3128/",
        "http://[fe80::1%25eth0]:3128",
    ],
)
def test_egress_upstream_proxy_accepts_valid_urls(url):
    cfg = EgressConfig(
        image="opensandbox/egress:v1",
        mode=EGRESS_MODE_DNS_NFT,
        upstream_proxy=EgressUpstreamProxyConfig(url=url),
    )
    assert cfg.upstream_proxy is not None
    assert cfg.upstream_proxy.url == url
    assert cfg.upstream_proxy.authorization is None


def test_egress_upstream_proxy_authorization_is_secret():
    cfg = EgressConfig(
        image="opensandbox/egress:v1",
        mode=EGRESS_MODE_DNS_NFT,
        upstream_proxy=EgressUpstreamProxyConfig(
            url="http://proxy.local:3128",
            authorization=SecretStr("Basic s3cr3t-value"),
        ),
    )
    assert cfg.upstream_proxy is not None
    assert isinstance(cfg.upstream_proxy.authorization, SecretStr)
    assert (
        cfg.upstream_proxy.authorization.get_secret_value()
        == "Basic s3cr3t-value"
    )
    assert "s3cr3t-value" not in repr(cfg)
    assert "s3cr3t-value" not in str(cfg)


def test_egress_upstream_proxy_defaults_to_none():
    cfg = EgressConfig(image="opensandbox/egress:v1")
    assert cfg.upstream_proxy is None


@pytest.mark.parametrize(
    "url",
    [
        "proxy:3128",
        "ftp://proxy:21",
        "http://:3128",
        "http://proxy:3128/x",
        "http://proxy:3128?x=1",
        "http://proxy:3128#f",
        "http://proxy:0",
        "http://proxy:70000",
        "http://proxy:abc",
        "http://proxy\\corp:3128",
        "http://exa\tmple.com:3128",
        "http://exa\rmple.com:3128",
        "http://exa\x00mple:3128",
        "http://exa\x7fmple:3128",
        "http://pro%xy:3128",
        "http://proxy%20name:3128",
        "http://pro%C3%A9xy:3128",
        "http://prox y:3128",
        "http://proxy|a:3128",
        "http://proxy{a}:3128",
        "http://proxy^a:3128",
        "http://proxy`a:3128",
    ],
)
def test_egress_upstream_proxy_rejects_invalid_urls(url):
    with pytest.raises(ValidationError):
        EgressConfig(
            image="opensandbox/egress:v1",
            mode=EGRESS_MODE_DNS_NFT,
            upstream_proxy=EgressUpstreamProxyConfig(url=url),
        )


def test_egress_upstream_proxy_userinfo_error_does_not_leak_credentials():
    with pytest.raises(ValidationError) as exc_info:
        EgressConfig(
            image="opensandbox/egress:v1",
            mode=EGRESS_MODE_DNS_NFT,
            upstream_proxy=EgressUpstreamProxyConfig(
                url="http://user:s3cr3t@proxy:3128"
            ),
        )
    assert "s3cr3t" not in str(exc_info.value)


def test_egress_upstream_proxy_authorization_rejects_newlines_without_leak():
    with pytest.raises(ValidationError) as exc_info:
        EgressConfig(
            image="opensandbox/egress:v1",
            mode=EGRESS_MODE_DNS_NFT,
            upstream_proxy=EgressUpstreamProxyConfig(
                url="http://proxy.local:3128",
                authorization=SecretStr("Basic s3cr3t\nInjected: x"),
            ),
        )
    assert "s3cr3t" not in str(exc_info.value)


@pytest.mark.parametrize("authorization", ["   ", "\t"])
def test_egress_upstream_proxy_whitespace_authorization_normalizes_to_none(
    authorization,
):
    cfg = EgressUpstreamProxyConfig(
        url="http://proxy.local:3128", authorization=SecretStr(authorization)
    )
    assert cfg.authorization is None


def test_load_config_with_egress_upstream_proxy(tmp_path, monkeypatch):
    _reset_config(monkeypatch)
    config_path = tmp_path / "config.toml"
    config_path.write_text(
        textwrap.dedent(
            """
            [runtime]
            type = "docker"
            execd_image = "opensandbox/execd:test"

            [egress]
            image = "opensandbox/egress:test"
            mode = "dns+nft"

            [egress.upstream_proxy]
            url = "http://proxy.local:3128"
            authorization = "Basic dGVzdDp0ZXN0"
            """
        )
    )

    loaded = config_module.load_config(config_path)

    assert loaded.egress is not None
    assert loaded.egress.upstream_proxy is not None
    assert loaded.egress.upstream_proxy.url == "http://proxy.local:3128"
    assert loaded.egress.upstream_proxy.authorization is not None
    assert (
        loaded.egress.upstream_proxy.authorization.get_secret_value()
        == "Basic dGVzdDp0ZXN0"
    )


def test_load_config_rejects_upstream_proxy_userinfo_without_leak(
    tmp_path, monkeypatch
):
    _reset_config(monkeypatch)
    config_path = tmp_path / "config.toml"
    config_path.write_text(
        textwrap.dedent(
            """
            [runtime]
            type = "docker"
            execd_image = "opensandbox/execd:test"

            [egress]
            image = "opensandbox/egress:test"
            mode = "dns+nft"

            [egress.upstream_proxy]
            url = "http://user:s3cr3t@proxy.local:3128"
            """
        )
    )

    with pytest.raises(ValidationError) as exc_info:
        config_module.load_config(config_path)

    assert "s3cr3t" not in str(exc_info.value)


def test_egress_upstream_proxy_requires_dns_nft_mode():
    with pytest.raises(ValidationError, match='egress.mode = "dns\\+nft"'):
        EgressConfig(
            image="opensandbox/egress:v1",
            upstream_proxy=EgressUpstreamProxyConfig(
                url="http://proxy.local:3128"
            ),
        )


def test_load_config_rejects_upstream_proxy_without_dns_nft(
    tmp_path, monkeypatch
):
    _reset_config(monkeypatch)
    config_path = tmp_path / "config.toml"
    config_path.write_text(
        textwrap.dedent(
            """
            [runtime]
            type = "docker"
            execd_image = "opensandbox/execd:test"

            [egress]
            image = "opensandbox/egress:test"

            [egress.upstream_proxy]
            url = "http://proxy.local:3128"
            """
        )
    )

    with pytest.raises(ValidationError, match="dns\\+nft"):
        config_module.load_config(config_path)


@pytest.mark.parametrize(
    "path",
    [
        "/etc/ssl/certs/upstream-ca.pem",
        " /opt/ca/proxy.pem ",
        "/var/lib/ca.pem",
    ],
)
def test_egress_upstream_proxy_ca_cert_path_valid(path):
    cfg = EgressUpstreamProxyConfig(
        url="http://proxy.local:3128", ca_cert_path=path
    )
    assert cfg.ca_cert_path == path.strip()


@pytest.mark.parametrize(
    "path",
    [
        "",
        "   ",
        "relative/ca.pem",
        "./ca.pem",
        "ca.pem",
        "C:\\ca\\proxy.pem",
        "/has:colon/ca.pem",
        "/bad/\x00ca.pem",
    ],
)
def test_egress_upstream_proxy_ca_cert_path_invalid(path):
    with pytest.raises(ValidationError):
        EgressUpstreamProxyConfig(
            url="http://proxy.local:3128", ca_cert_path=path
        )


@pytest.mark.parametrize(
    "name",
    [
        "corp-proxy-ca",
        " proxy-ca.ns1 ",
        "a",
        "ca-bundle.example.org",
        "ca-" + "b" * 60,
    ],
)
def test_egress_upstream_proxy_ca_secret_name_valid(name):
    cfg = EgressUpstreamProxyConfig(
        url="http://proxy.local:3128", ca_secret_name=name
    )
    assert cfg.ca_secret_name == name.strip()


@pytest.mark.parametrize(
    "name",
    [
        "",
        "   ",
        "UPPER_CASE",
        "has_underscore",
        "-leading-dash",
        "trailing-dash-",
        "a..b",
        "a" * 64,
        "a" * 254,
    ],
)
def test_egress_upstream_proxy_ca_secret_name_invalid(name):
    with pytest.raises(ValidationError):
        EgressUpstreamProxyConfig(
            url="http://proxy.local:3128", ca_secret_name=name
        )


def test_egress_upstream_proxy_ca_fields_default_to_none():
    cfg = EgressUpstreamProxyConfig(url="http://proxy.local:3128")
    assert cfg.ca_cert_path is None
    assert cfg.ca_secret_name is None


def test_load_config_with_egress_upstream_proxy_ca_cert_path_docker(
    tmp_path, monkeypatch
):
    _reset_config(monkeypatch)
    config_path = tmp_path / "config.toml"
    config_path.write_text(
        textwrap.dedent(
            """
            [runtime]
            type = "docker"
            execd_image = "opensandbox/execd:test"

            [egress]
            image = "opensandbox/egress:test"
            mode = "dns+nft"

            [egress.upstream_proxy]
            url = "https://proxy.local:8443"
            ca_cert_path = "/etc/ssl/private-ca/upstream.pem"
            """
        )
    )

    loaded = config_module.load_config(config_path)

    assert loaded.egress is not None
    assert loaded.egress.upstream_proxy is not None
    assert (
        loaded.egress.upstream_proxy.ca_cert_path
        == "/etc/ssl/private-ca/upstream.pem"
    )


def test_load_config_with_egress_upstream_proxy_ca_secret_name_kubernetes(
    tmp_path, monkeypatch
):
    _reset_config(monkeypatch)
    config_path = tmp_path / "config.toml"
    config_path.write_text(
        textwrap.dedent(
            """
            [runtime]
            type = "kubernetes"
            execd_image = "opensandbox/execd:test"

            [egress]
            image = "opensandbox/egress:test"
            mode = "dns+nft"

            [egress.upstream_proxy]
            url = "https://proxy.local:8443"
            ca_secret_name = "corp-proxy-ca"
            """
        )
    )

    loaded = config_module.load_config(config_path)

    assert loaded.egress is not None
    assert loaded.egress.upstream_proxy is not None
    assert loaded.egress.upstream_proxy.ca_secret_name == "corp-proxy-ca"


def test_load_config_rejects_ca_secret_name_on_docker(tmp_path, monkeypatch):
    _reset_config(monkeypatch)
    config_path = tmp_path / "config.toml"
    config_path.write_text(
        textwrap.dedent(
            """
            [runtime]
            type = "docker"
            execd_image = "opensandbox/execd:test"

            [egress]
            image = "opensandbox/egress:test"
            mode = "dns+nft"

            [egress.upstream_proxy]
            url = "https://proxy.local:8443"
            ca_secret_name = "corp-proxy-ca"
            """
        )
    )

    with pytest.raises(
        ValidationError, match="ca_secret_name requires runtime.type"
    ):
        config_module.load_config(config_path)


def test_load_config_rejects_ca_cert_path_on_kubernetes(
    tmp_path, monkeypatch
):
    _reset_config(monkeypatch)
    config_path = tmp_path / "config.toml"
    config_path.write_text(
        textwrap.dedent(
            """
            [runtime]
            type = "kubernetes"
            execd_image = "opensandbox/execd:test"

            [egress]
            image = "opensandbox/egress:test"
            mode = "dns+nft"

            [egress.upstream_proxy]
            url = "https://proxy.local:8443"
            ca_cert_path = "/etc/ssl/private-ca/upstream.pem"
            """
        )
    )

    with pytest.raises(
        ValidationError, match="ca_cert_path requires runtime.type"
    ):
        config_module.load_config(config_path)


def test_upstream_proxy_egress_env_emits_extra_ca_only_when_ca_source_set():
    from opensandbox_server.services.helpers import (
        upstream_proxy_egress_env,
    )

    no_ca = upstream_proxy_egress_env(
        EgressUpstreamProxyConfig(url="http://proxy.local:3128")
    )
    assert "OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_EXTRA_CA" not in no_ca

    with_path = upstream_proxy_egress_env(
        EgressUpstreamProxyConfig(
            url="http://proxy.local:3128",
            ca_cert_path="/etc/ssl/certs/ca.pem",
        )
    )
    assert (
        with_path["OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_EXTRA_CA"]
        == "/etc/ssl/certs/opensandbox-upstream-extra-ca.pem"
    )

    with_secret = upstream_proxy_egress_env(
        EgressUpstreamProxyConfig(
            url="http://proxy.local:3128",
            ca_secret_name="corp-proxy-ca",
        )
    )
    assert (
        with_secret["OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_EXTRA_CA"]
        == "/etc/ssl/certs/opensandbox-upstream-extra-ca.pem"
    )


def test_log_config_defaults():
    cfg = LogConfig()
    assert cfg.level == "INFO"
    assert cfg.file_enabled is False
    assert cfg.file_path is None
    assert cfg.access_file_path is None
    assert cfg.file_max_bytes == 100 * 1024 * 1024  # 100MB
    assert cfg.file_backup_count == 5


def test_log_config_resolved_file_path():
    cfg = LogConfig(file_enabled=False)
    assert cfg.resolved_file_path() is None

    # file_enabled=True without file_path uses default
    cfg = LogConfig(file_enabled=True)
    assert cfg.resolved_file_path() == LogConfig.DEFAULT_FILE_PATH

    # file_enabled=True with file_path uses custom path
    cfg = LogConfig(file_enabled=True, file_path="/custom/path.log")
    assert cfg.resolved_file_path() == "/custom/path.log"


def test_log_config_resolved_access_file_path():
    # file_enabled=False always returns None
    cfg = LogConfig(file_enabled=False, access_file_path="/path/access.log")
    assert cfg.resolved_access_file_path() is None

    # file_enabled=True without access_file_path returns default path
    cfg = LogConfig(file_enabled=True)
    assert cfg.resolved_access_file_path() == LogConfig.DEFAULT_ACCESS_FILE_PATH

    # file_enabled=True with access_file_path returns the custom path
    cfg = LogConfig(file_enabled=True, access_file_path="/custom/access.log")
    assert cfg.resolved_access_file_path() == "/custom/access.log"


def test_log_config_level_min_length():
    """LogConfig level must be at least 3 characters."""
    with pytest.raises(ValidationError):
        LogConfig(level="AB")
    cfg = LogConfig(level="DEBUG")
    assert cfg.level == "DEBUG"


def test_log_config_file_max_bytes_validation():
    """LogConfig file_max_bytes must be at least 1."""
    with pytest.raises(ValidationError):
        LogConfig(file_max_bytes=0)
    cfg = LogConfig(file_max_bytes=50 * 1024 * 1024)  # 50MB
    assert cfg.file_max_bytes == 50 * 1024 * 1024


def test_log_config_file_backup_count_validation():
    """LogConfig file_backup_count must be >= 0."""
    with pytest.raises(ValidationError):
        LogConfig(file_backup_count=-1)
    cfg = LogConfig(file_backup_count=10)
    assert cfg.file_backup_count == 10
    cfg_zero = LogConfig(file_backup_count=0)
    assert cfg_zero.file_backup_count == 0


def test_app_config_log_defaults():
    cfg = AppConfig(
        runtime=RuntimeConfig(type="docker", execd_image="test:latest")
    )
    assert cfg.log is not None
    assert cfg.log.level == "INFO"
    assert cfg.log.file_path is None


def test_load_config_with_log_subsection(tmp_path, monkeypatch):
    """LogConfig should be loaded from [log] TOML section."""
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [log]
        level = "DEBUG"
        file_path = "/var/log/opensandbox/server.log"
        file_max_bytes = 52428800
        file_backup_count = 3

        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.log.level == "DEBUG"
    assert loaded.log.file_path == "/var/log/opensandbox/server.log"
    assert loaded.log.file_max_bytes == 52428800
    assert loaded.log.file_backup_count == 3


def test_load_config_without_log_subsection_uses_defaults(tmp_path, monkeypatch):
    """AppConfig should use default LogConfig when [log] is not in TOML."""
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.log.level == "INFO"
    assert loaded.log.file_path is None
    assert loaded.log.file_max_bytes == 100 * 1024 * 1024
    assert loaded.log.file_backup_count == 5


def test_load_config_log_file_path_only(tmp_path, monkeypatch):
    """LogConfig should accept only file_path with other defaults."""
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [log]
        file_path = "/var/log/test.log"

        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.log.level == "INFO"  # default
    assert loaded.log.file_path == "/var/log/test.log"
    assert loaded.log.access_file_path is None  # default
    assert loaded.log.file_max_bytes == 100 * 1024 * 1024  # default
    assert loaded.log.file_backup_count == 5  # default


def test_load_config_log_access_file_path(tmp_path, monkeypatch):
    """LogConfig should accept access_file_path for separate access log file."""
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [log]
        file_path = "/var/log/opensandbox/server.log"
        access_file_path = "/var/log/opensandbox/access.log"

        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.log.file_path == "/var/log/opensandbox/server.log"
    assert loaded.log.access_file_path == "/var/log/opensandbox/access.log"


def test_load_config_log_file_enabled(tmp_path, monkeypatch):
    """LogConfig file_enabled should enable file logging with default paths."""
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [log]
        file_enabled = true

        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.log.file_enabled is True
    assert loaded.log.file_path is None  # not set, uses default
    assert loaded.log.access_file_path is None
    # resolved_* methods should return default paths
    assert loaded.log.resolved_file_path() == LogConfig.DEFAULT_FILE_PATH
    assert loaded.log.resolved_access_file_path() == LogConfig.DEFAULT_ACCESS_FILE_PATH


def test_load_config_log_file_enabled_with_custom_paths(tmp_path, monkeypatch):
    """LogConfig file_enabled with custom paths should use those paths."""
    _reset_config(monkeypatch)
    toml = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [log]
        file_enabled = true
        file_path = "/custom/server.log"
        access_file_path = "/custom/access.log"

        [runtime]
        type = "docker"
        execd_image = "opensandbox/execd:test"
        """
    )
    config_path = tmp_path / "config.toml"
    config_path.write_text(toml)

    loaded = config_module.load_config(config_path)
    assert loaded.log.file_enabled is True
    assert loaded.log.resolved_file_path() == "/custom/server.log"
    assert loaded.log.resolved_access_file_path() == "/custom/access.log"


# ============================================================
# SecureAccessKey / SecureAccessConfig
# ============================================================


class TestSecureAccessKey:
    def test_valid_key(self) -> None:
        import base64

        raw = b"my-secret-key-32-bytes!"
        key = SecureAccessKey(key_id="a", key=base64.b64encode(raw).decode())
        assert key.get_secret_bytes() == raw

    def test_key_id_must_be_single_char(self) -> None:
        with pytest.raises(ValidationError):
            SecureAccessKey(key_id="ab", key="AAAA")

    def test_key_id_must_be_alphanumeric_lowercase(self) -> None:
        with pytest.raises(ValidationError):
            SecureAccessKey(key_id="A", key="AAAA")
        with pytest.raises(ValidationError):
            SecureAccessKey(key_id="-", key="AAAA")
        with pytest.raises(ValidationError):
            SecureAccessKey(key_id="", key="AAAA")

    def test_key_id_edge_cases(self) -> None:
        import base64

        b64 = base64.b64encode(b"x").decode()
        for valid in "0abcxyz":
            key = SecureAccessKey(key_id=valid, key=b64)
            assert key.key_id == valid

    def test_key_must_be_valid_base64(self) -> None:
        with pytest.raises(ValidationError, match="not valid base64"):
            SecureAccessKey(key_id="a", key="!!!invalid-base64!!!")

    def test_key_must_decode_to_at_least_one_byte(self) -> None:
        with pytest.raises(ValidationError):
            SecureAccessKey(key_id="a", key="")

    def test_key_accepts_padded_and_unpadded_base64(self) -> None:
        import base64

        raw = b"hello"
        padded = base64.b64encode(raw).decode()  # "aGVsbG8="
        unpadded = padded.rstrip("=")
        key_a = SecureAccessKey(key_id="a", key=padded)
        key_b = SecureAccessKey(key_id="b", key=unpadded)
        assert key_a.get_secret_bytes() == raw
        assert key_b.get_secret_bytes() == raw


class TestSecureAccessConfig:
    def test_valid_config(self) -> None:
        import base64

        keys = [
            SecureAccessKey(key_id="a", key=base64.b64encode(b"key-a").decode()),
            SecureAccessKey(key_id="b", key=base64.b64encode(b"key-b").decode()),
        ]
        cfg = SecureAccessConfig(active_key="a", keys=keys)
        assert cfg.active_key == "a"
        assert len(cfg.keys) == 2

    def test_get_active_secret_bytes(self) -> None:
        import base64

        raw_a, raw_b = b"key-a-secret", b"key-b-secret"
        keys = [
            SecureAccessKey(key_id="a", key=base64.b64encode(raw_a).decode()),
            SecureAccessKey(key_id="b", key=base64.b64encode(raw_b).decode()),
        ]
        cfg = SecureAccessConfig(active_key="b", keys=keys)
        assert cfg.get_active_secret_bytes() == raw_b

    def test_active_key_must_exist_in_keys(self) -> None:
        import base64

        keys = [SecureAccessKey(key_id="a", key=base64.b64encode(b"key").decode())]
        with pytest.raises(ValidationError, match="not found in secure_access.keys"):
            SecureAccessConfig(active_key="z", keys=keys)

    def test_active_key_must_be_single_char(self) -> None:
        import base64

        keys = [SecureAccessKey(key_id="a", key=base64.b64encode(b"key").decode())]
        with pytest.raises(ValidationError):
            SecureAccessConfig(active_key="ab", keys=keys)

    def test_must_have_at_least_one_key(self) -> None:
        with pytest.raises(ValidationError):
            SecureAccessConfig(active_key="a", keys=[])

    def test_rejects_duplicate_key_ids(self) -> None:
        import base64

        keys = [
            SecureAccessKey(key_id="a", key=base64.b64encode(b"key-a").decode()),
            SecureAccessKey(key_id="a", key=base64.b64encode(b"key-a-dup").decode()),
        ]
        with pytest.raises(ValidationError, match="duplicate secure_access key_id"):
            SecureAccessConfig(active_key="a", keys=keys)


class TestSecureAccessInIngressConfig:
    def test_secure_access_requires_gateway_mode(self) -> None:
        import base64

        keys = [SecureAccessKey(key_id="a", key=base64.b64encode(b"secret").decode())]
        secure = SecureAccessConfig(active_key="a", keys=keys)
        with pytest.raises(ValueError, match="secure_access block requires ingress.mode = 'gateway'"):
            IngressConfig(mode="direct", secure_access=secure)

    def test_gateway_with_secure_access_is_valid(self) -> None:
        import base64

        keys = [SecureAccessKey(key_id="a", key=base64.b64encode(b"secret").decode())]
        ingress = IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="*.sandbox.example.com",
                route=GatewayRouteModeConfig(mode="wildcard"),
            ),
            secure_access=SecureAccessConfig(active_key="a", keys=keys),
        )
        assert ingress.secure_access is not None
        assert ingress.secure_access.active_key == "a"
        assert ingress.secure_access.get_active_secret_bytes() == b"secret"


class TestSecureAccessTomlLoading:
    def test_load_secure_access_from_toml(self, tmp_path, monkeypatch) -> None:
        import base64

        raw_key = b"my-base64-secret"
        b64 = base64.b64encode(raw_key).decode()
        toml = textwrap.dedent(
            f"""
            [server]
            host = "127.0.0.1"
            port = 9000

            [runtime]
            type = "kubernetes"
            execd_image = "opensandbox/execd:test"

            [ingress]
            mode = "gateway"

            [ingress.gateway]
            address = "*.sandbox.example.com"

            [ingress.gateway.route]
            mode = "wildcard"

            [ingress.secure_access]
            active_key = "a"

            [[ingress.secure_access.keys]]
            key_id = "a"
            key = "{b64}"
            """
        )
        config_path = tmp_path / "config.toml"
        config_path.write_text(toml)

        loaded = config_module.load_config(config_path)
        assert loaded.ingress is not None
        assert loaded.ingress.secure_access is not None
        assert loaded.ingress.secure_access.active_key == "a"
        assert loaded.ingress.secure_access.keys[0].key_id == "a"
        assert loaded.ingress.secure_access.get_active_secret_bytes() == raw_key

    def test_load_secure_access_with_multiple_keys(self, tmp_path, monkeypatch) -> None:
        import base64

        b64_a = base64.b64encode(b"key-a").decode()
        b64_b = base64.b64encode(b"key-b").decode()
        toml = textwrap.dedent(
            f"""
            [server]
            host = "127.0.0.1"
            port = 9000

            [runtime]
            type = "kubernetes"
            execd_image = "opensandbox/execd:test"

            [ingress]
            mode = "gateway"

            [ingress.gateway]
            address = "*.example.com"

            [ingress.gateway.route]
            mode = "wildcard"

            [ingress.secure_access]
            active_key = "b"

            [[ingress.secure_access.keys]]
            key_id = "a"
            key = "{b64_a}"

            [[ingress.secure_access.keys]]
            key_id = "b"
            key = "{b64_b}"
            """
        )
        config_path = tmp_path / "config.toml"
        config_path.write_text(toml)

        loaded = config_module.load_config(config_path)
        assert loaded.ingress.secure_access.active_key == "b"
        assert loaded.ingress.secure_access.get_active_secret_bytes() == b"key-b"

    def test_secure_access_direct_mode_rejected(self, tmp_path, monkeypatch) -> None:
        import base64

        b64 = base64.b64encode(b"key").decode()
        toml = textwrap.dedent(
            f"""
            [server]
            host = "127.0.0.1"
            port = 9000

            [runtime]
            type = "kubernetes"
            execd_image = "opensandbox/execd:test"

            [ingress.secure_access]
            active_key = "a"

            [[ingress.secure_access.keys]]
            key_id = "a"
            key = "{b64}"
            """
        )
        config_path = tmp_path / "config.toml"
        config_path.write_text(toml)

        with pytest.raises(ValidationError, match="secure_access block requires ingress.mode"):
            config_module.load_config(config_path)

    def test_secure_access_active_key_mismatch(self, tmp_path, monkeypatch) -> None:
        import base64

        b64 = base64.b64encode(b"key").decode()
        toml = textwrap.dedent(
            f"""
            [server]
            host = "127.0.0.1"
            port = 9000

            [runtime]
            type = "kubernetes"
            execd_image = "opensandbox/execd:test"

            [ingress]
            mode = "gateway"

            [ingress.gateway]
            address = "*.example.com"

            [ingress.gateway.route]
            mode = "wildcard"

            [ingress.secure_access]
            active_key = "z"

            [[ingress.secure_access.keys]]
            key_id = "a"
            key = "{b64}"
            """
        )
        config_path = tmp_path / "config.toml"
        config_path.write_text(toml)

        with pytest.raises(ValidationError, match="not found in secure_access.keys"):
            config_module.load_config(config_path)


class TestSecureAccessEnvOverride:
    """OPENSANDBOX_SECURE_ACCESS_KEYS / OPENSANDBOX_SECURE_ACCESS_ACTIVE_KEY."""

    _GATEWAY_TOML = textwrap.dedent(
        """
        [server]
        host = "127.0.0.1"
        port = 9000

        [runtime]
        type = "kubernetes"
        execd_image = "opensandbox/execd:test"

        [ingress]
        mode = "gateway"

        [ingress.gateway]
        address = "gw.example.com"

        [ingress.gateway.route]
        mode = "header"
        """
    )

    def _write_config(self, tmp_path, toml: str):
        config_path = tmp_path / "config.toml"
        config_path.write_text(toml)
        return config_path

    def test_env_secure_access_without_toml_block(self, tmp_path, monkeypatch) -> None:
        import base64

        _reset_config(monkeypatch)
        b64_a = base64.b64encode(b"key-a").decode()
        b64_b = base64.b64encode(b"key-b").decode()
        monkeypatch.setenv("OPENSANDBOX_SECURE_ACCESS_KEYS", f"a={b64_a},b={b64_b}")
        monkeypatch.setenv("OPENSANDBOX_SECURE_ACCESS_ACTIVE_KEY", "b")
        config_path = self._write_config(tmp_path, self._GATEWAY_TOML)

        loaded = config_module.load_config(config_path)
        assert loaded.ingress is not None
        assert loaded.ingress.secure_access is not None
        assert loaded.ingress.secure_access.active_key == "b"
        assert loaded.ingress.secure_access.get_active_secret_bytes() == b"key-b"

    def test_env_secure_access_overrides_toml_block(self, tmp_path, monkeypatch) -> None:
        import base64

        _reset_config(monkeypatch)
        b64_toml = base64.b64encode(b"toml-key").decode()
        b64_env = base64.b64encode(b"env-key").decode()
        monkeypatch.setenv("OPENSANDBOX_SECURE_ACCESS_KEYS", f"a={b64_env}")
        monkeypatch.setenv("OPENSANDBOX_SECURE_ACCESS_ACTIVE_KEY", "a")
        toml = self._GATEWAY_TOML + textwrap.dedent(
            f"""
            [ingress.secure_access]
            active_key = "a"

            [[ingress.secure_access.keys]]
            key_id = "a"
            key = "{b64_toml}"
            """
        )
        config_path = self._write_config(tmp_path, toml)

        loaded = config_module.load_config(config_path)
        assert loaded.ingress.secure_access.get_active_secret_bytes() == b"env-key"

    def test_env_secure_access_requires_both_vars(self, tmp_path, monkeypatch) -> None:
        import base64

        b64 = base64.b64encode(b"key").decode()
        config_path = self._write_config(tmp_path, self._GATEWAY_TOML)

        _reset_config(monkeypatch)
        monkeypatch.setenv("OPENSANDBOX_SECURE_ACCESS_KEYS", f"a={b64}")
        with pytest.raises(ValueError, match="must be set together"):
            config_module.load_config(config_path)

        _reset_config(monkeypatch)
        monkeypatch.delenv("OPENSANDBOX_SECURE_ACCESS_KEYS")
        monkeypatch.setenv("OPENSANDBOX_SECURE_ACCESS_ACTIVE_KEY", "a")
        with pytest.raises(ValueError, match="must be set together"):
            config_module.load_config(config_path)

    def test_env_secure_access_requires_gateway_mode(self, tmp_path, monkeypatch) -> None:
        import base64

        _reset_config(monkeypatch)
        b64 = base64.b64encode(b"key").decode()
        monkeypatch.setenv("OPENSANDBOX_SECURE_ACCESS_KEYS", f"a={b64}")
        monkeypatch.setenv("OPENSANDBOX_SECURE_ACCESS_ACTIVE_KEY", "a")
        toml = textwrap.dedent(
            """
            [server]
            host = "127.0.0.1"

            [runtime]
            type = "docker"
            execd_image = "opensandbox/execd:test"
            """
        )
        config_path = self._write_config(tmp_path, toml)

        with pytest.raises(ValueError, match="requires ingress.mode"):
            config_module.load_config(config_path)

    def test_env_secure_access_rejects_malformed_entry(self, tmp_path, monkeypatch) -> None:
        _reset_config(monkeypatch)
        monkeypatch.setenv("OPENSANDBOX_SECURE_ACCESS_KEYS", "not-a-pair")
        monkeypatch.setenv("OPENSANDBOX_SECURE_ACCESS_ACTIVE_KEY", "a")
        config_path = self._write_config(tmp_path, self._GATEWAY_TOML)

        with pytest.raises(ValueError, match="key_id=base64"):
            config_module.load_config(config_path)

    def test_env_secure_access_rejects_invalid_base64(self, tmp_path, monkeypatch) -> None:
        _reset_config(monkeypatch)
        monkeypatch.setenv("OPENSANDBOX_SECURE_ACCESS_KEYS", "a=!!!invalid!!!")
        monkeypatch.setenv("OPENSANDBOX_SECURE_ACCESS_ACTIVE_KEY", "a")
        config_path = self._write_config(tmp_path, self._GATEWAY_TOML)

        with pytest.raises(ValidationError, match="not valid base64"):
            config_module.load_config(config_path)

    def test_env_secure_access_active_key_must_exist(self, tmp_path, monkeypatch) -> None:
        import base64

        _reset_config(monkeypatch)
        b64 = base64.b64encode(b"key").decode()
        monkeypatch.setenv("OPENSANDBOX_SECURE_ACCESS_KEYS", f"a={b64}")
        monkeypatch.setenv("OPENSANDBOX_SECURE_ACCESS_ACTIVE_KEY", "z")
        config_path = self._write_config(tmp_path, self._GATEWAY_TOML)

        with pytest.raises(ValidationError, match="not found in secure_access.keys"):
            config_module.load_config(config_path)
