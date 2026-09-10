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

import logging
from threading import Lock

from fastapi import HTTPException

from opensandbox_server.config import KubernetesRuntimeConfig
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.services.constants import SandboxErrorCodes

GROUP = "sandbox.fast.io"
VERSION = "v1alpha2"
PLURAL = "sandboxes"

logger = logging.getLogger(__name__)


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
            if not client.custom_resource_exists(GROUP, VERSION, PLURAL):
                return []
            return [
                obj
                for obj in client.list_custom_objects(
                    GROUP, VERSION, namespace, PLURAL, ignore_not_found=False
                )
                if obj.get("metadata", {}).get("name", "").startswith("flt-")
            ]
        except Exception as exc:
            raise self._read_error(exc) from exc

    @staticmethod
    def _read_error(exc: Exception) -> HTTPException:
        logger.warning("Fsb Sandbox CR read failed: %s", exc)
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
