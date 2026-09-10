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

from fastapi import HTTPException

from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    CreateSandboxResponse,
    Endpoint,
    ListSandboxesRequest,
    ListSandboxesResponse,
    NetworkPolicy,
    PatchSandboxMetadataRequest,
    RenewSandboxExpirationRequest,
    RenewSandboxExpirationResponse,
    Sandbox,
)
from opensandbox_server.services.diagnostics import DiagnosticResult
from opensandbox_server.services.extension_service import ExtensionService
from opensandbox_server.services.fsb.service import FsbSandboxService
from opensandbox_server.services.k8s.kubernetes_service import KubernetesSandboxService
from opensandbox_server.services.k8s.list_helpers import _build_list_sandboxes_response
from opensandbox_server.services.sandbox_service import SandboxService

logger = logging.getLogger(__name__)


class CompositeSandboxService(SandboxService, ExtensionService):
    def __init__(self, kubernetes: KubernetesSandboxService, fsb: FsbSandboxService):
        self._kubernetes = kubernetes
        self._fsb = fsb

    def _backend(self, sandbox_id: str) -> KubernetesSandboxService | FsbSandboxService:
        return self._fsb if sandbox_id.startswith("flt-") else self._kubernetes

    def set_tenant_provider(self, provider: object) -> None:
        self._kubernetes.set_tenant_provider(provider)
        self._fsb.set_tenant_provider(provider)

    def close(self) -> None:
        self._fsb.close()
        self._kubernetes.close()

    async def create_sandbox(self, request: CreateSandboxRequest) -> CreateSandboxResponse:
        # templateId is the unambiguous fsb selector: fsb sandboxes are
        # the microVM catalog and coexist with the container-sandbox
        # workload provider; every other create stays with it.
        if (request.template_id or "").strip():
            return await self._fsb.create_sandbox(request)
        return await self._kubernetes.create_sandbox(request)

    def list_sandboxes(self, request: ListSandboxesRequest) -> ListSandboxesResponse:
        try:
            objects = self._kubernetes.list_sandbox_objects() + self._fsb.list_sandbox_objects()
            objects.sort(key=lambda sandbox: sandbox.id)
            return _build_list_sandboxes_response(objects, request)
        except HTTPException:
            raise
        except Exception as exc:
            logger.warning("Cannot read complete sandbox list: %s", exc)
            raise HTTPException(
                503,
                detail={
                    "code": "SANDBOX_LIST_UNAVAILABLE",
                    "message": "Cannot read the complete sandbox list.",
                },
            ) from exc

    def get_sandbox(self, sandbox_id: str) -> Sandbox:
        return self._backend(sandbox_id).get_sandbox(sandbox_id)

    def patch_sandbox_metadata(
        self, sandbox_id: str, patch: PatchSandboxMetadataRequest
    ) -> Sandbox:
        return self._backend(sandbox_id).patch_sandbox_metadata(sandbox_id, patch)

    def delete_sandbox(self, sandbox_id: str) -> None:
        self._backend(sandbox_id).delete_sandbox(sandbox_id)

    def pause_sandbox(self, sandbox_id: str) -> None:
        self._backend(sandbox_id).pause_sandbox(sandbox_id)

    def resume_sandbox(self, sandbox_id: str) -> None:
        self._backend(sandbox_id).resume_sandbox(sandbox_id)

    def renew_expiration(
        self, sandbox_id: str, request: RenewSandboxExpirationRequest
    ) -> RenewSandboxExpirationResponse:
        return self._backend(sandbox_id).renew_expiration(sandbox_id, request)

    def get_endpoint(
        self,
        sandbox_id: str,
        port: int,
        resolve_internal: bool = False,
        expires: int | None = None,
        use_proxy_host: bool = False,
    ) -> Endpoint:
        return self._backend(sandbox_id).get_endpoint(
            sandbox_id, port, resolve_internal, expires, use_proxy_host
        )

    def get_access_renew_extend_seconds(self, sandbox_id: str) -> int | None:
        return self._backend(sandbox_id).get_access_renew_extend_seconds(sandbox_id)

    def get_network_policy(self, sandbox_id: str) -> dict:
        return self._fsb.get_network_policy(sandbox_id)

    def replace_network_policy(self, sandbox_id: str, policy: NetworkPolicy) -> dict:
        return self._fsb.replace_network_policy(sandbox_id, policy)

    def get_sandbox_log_diagnostics(self, sandbox_id: str, scope: str) -> DiagnosticResult:
        return self._backend(sandbox_id).get_sandbox_log_diagnostics(sandbox_id, scope)

    def get_sandbox_event_diagnostics(self, sandbox_id: str, scope: str) -> DiagnosticResult:
        return self._backend(sandbox_id).get_sandbox_event_diagnostics(sandbox_id, scope)

    def get_sandbox_logs(
        self,
        sandbox_id: str,
        tail: int = 100,
        since: str | None = None,
        container: str | None = None,
    ) -> str:
        return self._backend(sandbox_id).get_sandbox_logs(sandbox_id, tail, since, container)

    def get_sandbox_inspect(self, sandbox_id: str) -> str:
        return self._backend(sandbox_id).get_sandbox_inspect(sandbox_id)

    def get_sandbox_events(self, sandbox_id: str, limit: int = 50) -> str:
        return self._backend(sandbox_id).get_sandbox_events(sandbox_id, limit)
