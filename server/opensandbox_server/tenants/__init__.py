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

from opensandbox_server.tenants.context import get_current_tenant, set_current_tenant
from opensandbox_server.tenants.file_provider import (
    DEFAULT_TENANTS_CONFIG_PATH,
    TENANTS_CONFIG_ENV_VAR,
    FileTenantProvider,
    resolve_tenants_path,
)
from opensandbox_server.tenants.http_provider import (
    HTTPTenantProvider,
    HTTPTenantProviderConfig,
)
from opensandbox_server.tenants.models import TenantEntry
from opensandbox_server.tenants.provider import TenantProvider, TenantProviderUnavailable

import logging
from typing import Iterable

logger = logging.getLogger(__name__)


def validate_tenant_config(app_config) -> None:
    """Validate tenant configuration against runtime and auth settings.

    Raises ValueError if:
    - runtime is docker (multi-tenancy requires Kubernetes namespaces)
    - server.api_key is set (conflicts with tenant-managed keys)
    """
    if app_config.tenants is None:
        return
    if app_config.runtime.type == "docker":
        raise ValueError(
            "[tenants] configured but runtime.type='docker'. "
            "Multi-tenancy requires Kubernetes namespaces."
        )
    api_key = getattr(app_config.server, "api_key", None)
    if api_key and api_key.strip():
        raise ValueError(
            "server.api_key must be removed from server.toml when using [tenants]. "
            "Tenant API keys are managed by the tenant provider."
        )


def validate_tenant_namespaces(
    tenants: Iterable[TenantEntry], core_v1_api
) -> None:
    """Validate that every tenant namespace exists and is accessible.

    Enforces the OSEP-0014 startup requirement that all tenant namespaces
    exist and are accessible before the server accepts traffic (fail-fast).

    Args:
        tenants: Tenant entries to validate.
        core_v1_api: A Kubernetes ``CoreV1Api`` used to read namespaces.

    Raises:
        ValueError: If any tenant namespace is missing or inaccessible. The
            error aggregates all failing namespaces so operators can fix the
            configuration in a single pass.
    """
    from kubernetes.client import ApiException

    failures: list[str] = []
    checked: set[str] = set()
    for tenant in tenants:
        namespace = tenant.namespace
        if namespace in checked:
            continue
        checked.add(namespace)
        try:
            core_v1_api.read_namespace(name=namespace)
        except ApiException as exc:
            if exc.status == 404:
                failures.append(
                    f"tenant '{tenant.name}': namespace '{namespace}' does not exist"
                )
            elif exc.status in (401, 403):
                failures.append(
                    f"tenant '{tenant.name}': namespace '{namespace}' is not accessible "
                    f"(HTTP {exc.status})"
                )
            else:
                failures.append(
                    f"tenant '{tenant.name}': failed to read namespace '{namespace}' "
                    f"(HTTP {exc.status})"
                )
        except Exception as exc:  # noqa: BLE001 - surface any client error as fatal
            failures.append(
                f"tenant '{tenant.name}': failed to read namespace '{namespace}': {exc}"
            )

    if failures:
        raise ValueError(
            "Tenant namespace validation failed; all tenant namespaces must exist "
            "and be accessible at startup:\n  - " + "\n  - ".join(failures)
        )

    logger.info("Validated %d tenant namespace(s) at startup", len(checked))


def validate_tenant_namespaces_on_startup(provider, core_v1_api) -> None:
    """Validate tenant namespaces at startup if the provider can enumerate them.

    Enforces the OSEP-0014 fail-fast requirement that all tenant namespaces
    exist and are accessible before the server accepts traffic. Providers that
    resolve tenants per API key (e.g. the HTTP provider) cannot enumerate all
    tenants at startup; validating their empty set would silently report
    success, so validation is skipped with a warning instead.

    Args:
        provider: The configured tenant provider.
        core_v1_api: A Kubernetes ``CoreV1Api`` used to read namespaces.

    Raises:
        ValueError: If any tenant namespace is missing or inaccessible (for
            enumerable providers).
    """
    if not getattr(provider, "supports_enumeration", True):
        logger.warning(
            "Skipping tenant namespace startup validation: the tenant provider "
            "cannot enumerate all tenants. Ensure tenant namespaces exist and "
            "are accessible before issuing tenant API keys."
        )
        return
    validate_tenant_namespaces(provider.list_tenants(), core_v1_api)


__all__ = [
    "TenantEntry",
    "TenantProvider",
    "TenantProviderUnavailable",
    "FileTenantProvider",
    "HTTPTenantProvider",
    "HTTPTenantProviderConfig",
    "DEFAULT_TENANTS_CONFIG_PATH",
    "TENANTS_CONFIG_ENV_VAR",
    "get_current_tenant",
    "set_current_tenant",
    "resolve_tenants_path",
    "validate_tenant_config",
    "validate_tenant_namespaces",
    "validate_tenant_namespaces_on_startup",
]
