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

from opensandbox_server.config import AppConfig
from opensandbox_server.services.lifecycle_audit import lifecycle_audit_enabled


_MIN_RUNTIME = {"type": "kubernetes", "execd_image": "example/execd:latest"}


def test_lifecycle_audit_disabled_by_default() -> None:
    config = AppConfig.model_validate(
        {
            "runtime": _MIN_RUNTIME,
            "store": {"type": "postgresql", "postgresql": {"dsn": "postgresql://u:p@localhost/db"}},
        }
    )
    assert config.store.lifecycle_audit.enabled is False


def test_lifecycle_audit_enabled_requires_postgresql(monkeypatch) -> None:
    from opensandbox_server.services import lifecycle_audit as audit_module

    cfg = AppConfig.model_validate(
        {
            "runtime": _MIN_RUNTIME,
            "store": {
                "type": "postgresql",
                "postgresql": {"dsn": "postgresql://u:p@localhost/db"},
                "lifecycle_audit": {"enabled": True},
            },
        }
    )
    monkeypatch.setattr(audit_module, "get_config", lambda: cfg)
    assert lifecycle_audit_enabled() is True


def test_audit_pool_caps_minimum_to_the_maximum(monkeypatch) -> None:
    from opensandbox_server.repositories.lifecycle_audit import factory as factory_mod

    captured: dict[str, int] = {}

    class _Repo:
        def __init__(self, _dsn: str, *, min_pool_size: int, max_pool_size: int, **_kwargs: object) -> None:
            captured["min"] = min_pool_size
            captured["max"] = max_pool_size

    monkeypatch.setattr(factory_mod, "PostgreSQLLifecycleAuditRepository", _Repo)
    cfg = AppConfig.model_validate(
        {
            "runtime": _MIN_RUNTIME,
            "store": {
                "type": "postgresql",
                "postgresql": {
                    "dsn": "postgresql://u:p@localhost/db",
                    "min_pool_size": 6,
                    "max_pool_size": 10,
                },
                "lifecycle_audit": {"enabled": True},
            },
        }
    )
    factory_mod.create_lifecycle_audit_repository(cfg)
    assert captured == {"min": 5, "max": 5}
