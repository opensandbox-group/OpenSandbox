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

from dataclasses import dataclass
from pathlib import Path

try:
    import tomllib
except ModuleNotFoundError:
    import tomli as tomllib  # type: ignore[no-redef]


@dataclass(frozen=True, slots=True)
class TenantRecord:
    name: str
    namespace: str
    api_key: str


def _load_toml(path: Path) -> dict:
    """Parse TOML file: stdlib tomllib (3.11+) wants str; tomli backport wants bytes."""
    raw = path.read_bytes()
    try:
        return tomllib.loads(raw)
    except TypeError:
        return tomllib.loads(raw.decode("utf-8"))


def _read_tenants_file(path: Path) -> list[TenantRecord]:
    data = _load_toml(path)
    records: list[TenantRecord] = []
    seen_keys: dict[str, str] = {}

    for raw in data.get("tenants", []):
        name = raw["name"]
        namespace = raw["namespace"]
        api_keys = raw.get("api_keys") or []
        if not api_keys:
            raise ValueError(f"Tenant {name!r} has no api_keys")
        for key in api_keys:
            if not isinstance(key, str) or not key.strip():
                raise ValueError(f"Tenant {name!r} has invalid api_key")
            if key in seen_keys:
                raise ValueError(f"Duplicate api_key: {name!r} vs {seen_keys[key]!r}")
            seen_keys[key] = name
        records.append(
            TenantRecord(name=name, namespace=namespace, api_key=api_keys[0]),
        )
    return records


def load_tenants(path: str | Path) -> list[TenantRecord]:
    return _read_tenants_file(Path(path))


def get_tenant_by_name(path: str | Path, name: str) -> TenantRecord | None:
    for record in load_tenants(path):
        if record.name == name:
            return record
    return None


def lookup_by_api_key(path: str | Path, api_key: str) -> TenantRecord | None:
    data = _load_toml(Path(path))
    for raw in data.get("tenants", []):
        name = raw["name"]
        namespace = raw["namespace"]
        for key in raw.get("api_keys", []):
            if key == api_key:
                return TenantRecord(name=name, namespace=namespace, api_key=key)
    return None
