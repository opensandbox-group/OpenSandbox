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

import logging
from dataclasses import dataclass
from threading import Lock
from typing import Callable, Optional

from fastapi import HTTPException
from kubernetes.client import ApiException

from opensandbox_server.config import KubernetesRuntimeConfig
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.services.constants import SandboxErrorCodes

GROUP = "sandbox.fast.io"
VERSION = "v1alpha2"
PLURAL = "sandboxes"
POOL_PLURAL = "sandboxpools"

logger = logging.getLogger(__name__)

# fast-sandbox counts any non-null member as declared; "" and {} are rejected
# by the CRD schema and only make the gate fail safe here.
_ABSENT = (None, "", {})
_RESOURCE_AWARE_MODE = "ResourceAware"


def pool_not_found(namespace: str, pool_name: str) -> HTTPException:
    return HTTPException(
        404,
        detail={
            "code": SandboxErrorCodes.FSB_POOL_NOT_FOUND,
            "message": f"SandboxPool {pool_name!r} not found in namespace {namespace!r}.",
        },
    )


@dataclass(frozen=True)
class PoolSnapshot:
    """Create-path view of one SandboxPool.

    ``is_resource_aware`` and ``resources`` are the OSEP-0026 pool-mode gate and
    default profile; ``action_handlers`` lists ``spec.actionHandlers[].name`` in
    declaration order.
    """

    name: str
    namespace: str
    is_resource_aware: bool
    resources: dict[str, str]
    action_handlers: tuple[str, ...]

    @classmethod
    def from_cr(cls, pool: dict) -> "PoolSnapshot":
        """Project a SandboxPool CR as fast-sandbox's ``ResourceMode()`` sees it."""
        metadata = pool.get("metadata") or {}
        spec = pool.get("spec") or {}
        profile = spec.get("sandboxResources") or {}
        # cpu/memory are int-or-string; pids is omitted exactly as GetPool omits 0.
        resources = {
            key: str(profile[key])
            for key in ("cpu", "memory")
            if profile.get(key) not in _ABSENT
        }
        if profile.get("pids"):
            resources["pids"] = str(profile["pids"])
        return cls(
            name=metadata.get("name") or "",
            namespace=metadata.get("namespace") or "",
            is_resource_aware=(
                profile.get("mode") == _RESOURCE_AWARE_MODE
                and all(
                    profile.get(key) not in _ABSENT
                    for key in ("cpu", "memory", "overcommitRatio")
                )
            ),
            resources=resources,
            action_handlers=tuple(
                handler["name"] for handler in spec.get("actionHandlers") or []
            ),
        )


class PoolCRReader:
    """Serves create-path SandboxPool reads through the service's shared K8sClient.

    ``K8sClient.get_custom_object`` answers from the namespace LIST/WATCH
    informer once it has synced, so steady-state creates make no API call and
    a pool edit reaches the next create with the watch; a cache miss falls
    back to a direct GET.
    """

    def __init__(
        self,
        kubernetes: Callable[[], K8sClient],
        *,
        kubernetes_if_ready: Optional[Callable[[], Optional[K8sClient]]] = None,
    ):
        self._kubernetes = kubernetes
        self._kubernetes_if_ready = kubernetes_if_ready or kubernetes

    def get_cached(self, namespace: str, pool_name: str) -> Optional[PoolSnapshot]:
        """Return a PoolSnapshot from an already-synced informer cache, or None.

        Does not lazily initialize a K8sClient, start an informer, or hit the
        API server, so callers can invoke it directly on an asyncio event loop.
        """
        client = self._kubernetes_if_ready()
        if client is None:
            return None
        cached = client.get_cached_custom_object(
            GROUP, VERSION, namespace, POOL_PLURAL, pool_name
        )
        if cached is None:
            return None
        return PoolSnapshot.from_cr(cached)

    def get(self, namespace: str, pool_name: str) -> PoolSnapshot:
        """Read one pool; 404 when absent, 503 on other read failures.

        A 403 ``ApiException`` is the one failure that escapes: the server
        ClusterRole has not been granted ``sandboxpools`` yet, and the caller
        owns the FastPath fallback for that rolling-upgrade window.
        """
        try:
            pool = self._kubernetes().get_custom_object(
                GROUP, VERSION, namespace, POOL_PLURAL, pool_name
            )
        except ApiException as exc:
            if exc.status == 403:
                raise
            raise self._read_error(exc) from exc
        except Exception as exc:
            raise self._read_error(exc) from exc
        if pool is None:
            raise pool_not_found(namespace, pool_name)
        return PoolSnapshot.from_cr(pool)

    @staticmethod
    def _read_error(exc: Exception) -> HTTPException:
        logger.warning(f"Fsb SandboxPool CR read failed: {exc}")
        return HTTPException(
            503,
            detail={
                "code": SandboxErrorCodes.FSB_API_ERROR,
                "message": "Fsb SandboxPool CRs are unavailable.",
            },
        )


class SandboxCRReader:
    def __init__(self, config: KubernetesRuntimeConfig, client: K8sClient | None = None):
        self._config = config
        self._client = client
        self._lock = Lock()

    def _kubernetes(self) -> K8sClient:
        with self._lock:
            if self._client is None:
                self._client = K8sClient(self._config)
            return self._client

    def get(self, namespace: str, sandbox_id: str) -> dict:
        try:
            result = self._kubernetes().get_custom_object(
                GROUP, VERSION, namespace, PLURAL, sandbox_id
            )
        except Exception as exc:
            raise self._read_error(exc) from exc
        if result is None:
            raise HTTPException(
                404,
                detail={
                    "code": SandboxErrorCodes.FSB_SANDBOX_NOT_FOUND,
                    "message": "Sandbox not found.",
                },
            )
        return result

    def list(self, namespace: str) -> list[dict]:
        try:
            client = self._kubernetes()
            objs = client.list_custom_objects(
                GROUP, VERSION, namespace, PLURAL, ignore_not_found=False
            )
        except ApiException as exc:
            # An uninstalled CRD is an empty list, not a read failure; every
            # other error surfaces as 503.
            if exc.status == 404:
                return []
            raise self._read_error(exc) from exc
        except Exception as exc:
            raise self._read_error(exc) from exc
        return [
            obj
            for obj in objs
            if obj.get("metadata", {}).get("name", "").startswith("fsb-")
        ]

    @staticmethod
    def _read_error(exc: Exception) -> HTTPException:
        logger.warning(f"Fsb Sandbox CR read failed: {exc}")
        return HTTPException(
            503,
            detail={
                "code": SandboxErrorCodes.FSB_API_ERROR,
                "message": "Fsb Sandbox CRs are unavailable.",
            },
        )

    def invalidate(self, namespace: str) -> None:
        with self._lock:
            if self._client is not None:
                self._client.invalidate_custom_objects(GROUP, VERSION, PLURAL, namespace)

    def close(self) -> None:
        with self._lock:
            if self._client is not None:
                self._client.stop_informers()
