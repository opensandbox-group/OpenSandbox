# pyright: reportAttributeAccessIssue=false
# protobuf-generated modules expose dynamic attributes.

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

"""FleetSandboxService: fast-sandbox (fleets) runtime backend (OSEP-0007).

FastPath owns live runtime state. Create echoes request-owned public fields;
reads that require fields absent from FastPath return FLEETS::API_NOT_SUPPORTED.
"""

from __future__ import annotations

import asyncio
import json
from datetime import datetime, timezone
from typing import Optional

from fastapi import HTTPException
from starlette import status

from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    CreateSandboxResponse,
    Endpoint,
    ListSandboxesRequest,
    ListSandboxesResponse,
    PatchSandboxMetadataRequest,
    RenewSandboxExpirationRequest,
    RenewSandboxExpirationResponse,
    Sandbox,
    SandboxStatus,
)
from opensandbox_server.config import AppConfig, FleetsRuntimeConfig
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.diagnostics import (
    DiagnosticResult,
    unsupported_scope_error,
)
from opensandbox_server.services.extension_service import ExtensionService
from opensandbox_server.services.fleets.create_mapping import (
    UnsupportedFieldError,
    map_create_request,
)
from opensandbox_server.services.fleets.fastpath_client import (
    FastPathClient,
    FastPathConflict,
    FastPathError,
    FastPathInvalidArgument,
    FastPathNotFound,
    FastPathUnavailable,
)
from opensandbox_server.services.fleets.generated import fastpath_pb2 as pb2
from opensandbox_server.services.fleets.status_mapping import map_reason, map_state
from opensandbox_server.services.sandbox_service import SandboxService
from opensandbox_server.services.validators import (
    ensure_future_expiration,
    ensure_timeout_within_limit,
)

_SUPPORTED_EVENT_SCOPES = ("runtime", "all")


class FleetSandboxService(SandboxService, ExtensionService):
    """sandbox fleets runtime backed by the fast-sandbox FastPath v2 API."""

    def __init__(
        self,
        config: AppConfig,
        fastpath_client: Optional[FastPathClient] = None,
    ):
        self._app_config = config
        fleets_config = config.fleets or FleetsRuntimeConfig()
        self._fleets = fleets_config
        self._fastpath = fastpath_client or FastPathClient(
            endpoint=fleets_config.fastpath_endpoint,
            timeout_seconds=fleets_config.fastpath_timeout_seconds,
        )
        self._tenant_provider = None  # type: ignore[assignment]

    @staticmethod
    def generate_sandbox_id() -> str:
        return f"flt-{SandboxService.generate_sandbox_id()}"

    def set_tenant_provider(self, provider: object) -> None:
        """Inject the tenant provider (tenant -> fast-sandbox namespace mapping)."""
        self._tenant_provider = provider  # type: ignore[assignment]

    # -- namespace resolution ----------------------------------------------

    def _resolve_namespace(self) -> str:
        """Resolve the fast-sandbox namespace for the current tenant context."""
        from opensandbox_server.tenants.context import get_current_tenant

        tenant = get_current_tenant()
        if tenant is not None:
            return tenant.namespace
        return self._fleets.namespace

    def _resolve_namespace_for_lookup(self, sandbox_id: str) -> str:
        """Resolve namespace with a cross-namespace fallback for background work.

        Background renew workers have no request tenant in the ContextVar;
        scan the tenant provider's namespaces to find the sandbox before
        falling back to the global namespace.
        """
        from opensandbox_server.tenants.context import get_current_tenant

        tenant = get_current_tenant()
        if tenant is not None:
            return tenant.namespace

        namespaces = [self._fleets.namespace]
        if self._tenant_provider is not None:
            namespaces.extend(
                entry.namespace
                for entry in self._tenant_provider.list_tenants()
                if entry.namespace != self._fleets.namespace
            )
        for namespace in namespaces:
            try:
                self._fastpath.get_sandbox(namespace, sandbox_id)
            except FastPathNotFound:
                continue
            except FastPathError as exc:
                raise self._fastpath_http_error(exc) from exc
            return namespace

        return self._fleets.namespace

    def _pool_resources(self, namespace: str, pool_ref: str) -> dict:
        """Return the resource profile declared by the selected SandboxPool."""
        try:
            pool = self._fastpath.get_pool(namespace, pool_ref)
        except FastPathNotFound:
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail={
                    "code": SandboxErrorCodes.FLEETS_POOL_NOT_FOUND,
                    "message": f"SandboxPool {pool_ref!r} not found in namespace {namespace!r}.",
                },
            )
        except FastPathError as exc:
            raise self._fastpath_http_error(exc) from exc
        resources: dict = {}
        if pool.sandbox_cpu:
            resources["cpu"] = pool.sandbox_cpu
        if pool.sandbox_memory:
            resources["memory"] = pool.sandbox_memory
        if pool.sandbox_pids:
            resources["pids"] = str(pool.sandbox_pids)
        return resources

    # -- lifecycle ---------------------------------------------------------

    async def create_sandbox(self, request: CreateSandboxRequest) -> CreateSandboxResponse:
        """Create a sandbox through FastPath v2; returns accepted Pending when
        durable intent exists but the data plane is not ready yet.

        The FastPath calls are synchronous gRPC, so the whole workflow runs
        in a worker thread to keep the event loop responsive.
        """
        ensure_timeout_within_limit(
            request.timeout,
            self._app_config.server.max_sandbox_timeout_seconds,
        )
        return await asyncio.to_thread(self._create_sandbox_sync, request)

    def _create_sandbox_sync(self, request: CreateSandboxRequest) -> CreateSandboxResponse:
        created_at = datetime.now(timezone.utc)
        try:
            create_request = map_create_request(
                request,
                sandbox_id=self.generate_sandbox_id(),
                namespace=self._resolve_namespace(),
                default_pool_ref=self._fleets.default_pool_ref,
            )
        except UnsupportedFieldError as exc:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": str(exc),
                },
            ) from exc

        namespace = create_request.namespace
        sandbox_id = create_request.request_id
        pool_resources = self._pool_resources(namespace, create_request.pool_ref)
        try:
            create_request = map_create_request(
                request,
                sandbox_id=sandbox_id,
                namespace=namespace,
                default_pool_ref=self._fleets.default_pool_ref,
                expires_at_unix_seconds=create_request.expires_at_unix_seconds,
                pool_resources=pool_resources,
            )
        except UnsupportedFieldError as exc:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": str(exc),
                },
            ) from exc

        try:
            response = self._fastpath.create_sandbox(
                create_request,
                wait_timeout_millis=self._fleets.wait_ready_timeout_millis,
            )
        except (FastPathInvalidArgument, FastPathConflict, FastPathNotFound) as exc:
            raise self._fastpath_http_error(exc) from exc
        except FastPathError as exc:
            # Post-persistence failure recovery: if durable intent exists for
            # the same namespaced id, the create was accepted; return it as
            # Pending and let reconciliation continue.
            try:
                existing = self._fastpath.get_sandbox(namespace, sandbox_id)
            except FastPathError:
                raise self._fastpath_http_error(exc) from exc
            info = existing.sandbox
        else:
            info = response.sandbox

        return self._build_create_response(
            request,
            info,
            sandbox_id=sandbox_id,
            created_at=created_at,
            expires_at=datetime.fromtimestamp(
                create_request.expires_at_unix_seconds, tz=timezone.utc
            ),
        )

    def _build_create_response(
        self,
        request: CreateSandboxRequest,
        info: pb2.SandboxInfo,
        *,
        sandbox_id: str,
        created_at: datetime,
        expires_at: datetime,
    ) -> CreateSandboxResponse:
        return CreateSandboxResponse(
            id=sandbox_id,
            status=SandboxStatus(
                state=map_state(info),
                reason=map_reason(info),
                message=None,
                lastTransitionAt=None,
            ),
            metadata=request.metadata,
            extensions=request.extensions,
            platform=None,
            expiresAt=expires_at,
            createdAt=created_at,
            entrypoint=request.entrypoint,
        )

    def get_sandbox(self, sandbox_id: str) -> Sandbox:
        raise self._unsupported(
            "get sandbox until FastPath exposes the fields required by the Sandbox response"
        )

    def list_sandboxes(self, request: ListSandboxesRequest) -> ListSandboxesResponse:
        raise self._unsupported(
            "list sandboxes until FastPath exposes the fields required by Sandbox responses"
        )

    def delete_sandbox(self, sandbox_id: str) -> None:
        """Preflight Get to preserve the public 404 contract, then submit an
        async (finalizer-driven) deletion."""
        namespace = self._resolve_namespace_for_lookup(sandbox_id)
        try:
            current = self._fastpath.get_sandbox(namespace, sandbox_id)
        except FastPathError as exc:
            raise self._fastpath_http_error(exc) from exc
        try:
            self._fastpath.delete_sandbox(
                namespace,
                sandbox_id,
                expected_uid=current.sandbox.identity.uid,
            )
        except FastPathError as exc:
            raise self._fastpath_http_error(exc) from exc

    def pause_sandbox(self, sandbox_id: str) -> None:
        raise self._unsupported("pause")

    def resume_sandbox(self, sandbox_id: str) -> None:
        raise self._unsupported("resume")

    def renew_expiration(
        self,
        sandbox_id: str,
        request: RenewSandboxExpirationRequest,
    ) -> RenewSandboxExpirationResponse:
        """Persist the absolute expiry on the Sandbox CRD."""
        namespace = self._resolve_namespace_for_lookup(sandbox_id)
        normalized = ensure_future_expiration(request.expires_at)
        try:
            current = self._fastpath.get_sandbox(namespace, sandbox_id)
            self._fastpath.update_expiration(
                namespace,
                sandbox_id,
                int(normalized.timestamp()),
                expected_uid=current.sandbox.identity.uid,
                expected_generation=current.generation,
            )
        except FastPathError as exc:
            raise self._fastpath_http_error(exc) from exc
        return RenewSandboxExpirationResponse(expiresAt=normalized)

    def patch_sandbox_metadata(
        self,
        sandbox_id: str,
        patch: PatchSandboxMetadataRequest,
    ) -> Sandbox:
        raise self._unsupported(
            "metadata patch until FastPath exposes the fields required by the Sandbox response"
        )

    # -- diagnostics -------------------------------------------------------

    def get_sandbox_log_diagnostics(self, sandbox_id: str, scope: str) -> DiagnosticResult:
        raise self._unsupported("sandbox logs")

    def get_sandbox_event_diagnostics(self, sandbox_id: str, scope: str) -> DiagnosticResult:
        normalized_scope = scope.strip().lower()
        if normalized_scope not in _SUPPORTED_EVENT_SCOPES:
            raise unsupported_scope_error("events", scope, _SUPPORTED_EVENT_SCOPES)
        result = self._lifecycle_diagnostics(sandbox_id, "events", normalized_scope)
        if normalized_scope == "all":
            return DiagnosticResult(
                sandbox_id=result.sandbox_id,
                kind=result.kind,
                scope=result.scope,
                content=result.content,
                truncated=result.truncated,
                warnings=("The current backend only contributes runtime events to the all scope.",),
            )
        return result

    def get_sandbox_logs(
        self,
        sandbox_id: str,
        tail: int = 100,
        since: Optional[str] = None,
        container: Optional[str] = None,
    ) -> str:
        # FastPath diagnostics carry lifecycle events only; process output
        # flows through execd, which has no sandbox-log endpoint.
        raise self._unsupported("sandbox logs")

    def get_sandbox_inspect(self, sandbox_id: str) -> str:
        return self._lifecycle_diagnostics(sandbox_id, "events", "inspect").content

    def get_sandbox_events(self, sandbox_id: str, limit: int = 50) -> str:
        return self._lifecycle_diagnostics(sandbox_id, "events", "events", limit).content

    def _lifecycle_diagnostics(
        self, sandbox_id: str, kind: str, scope: str, limit: int = 50
    ) -> DiagnosticResult:
        namespace = self._resolve_namespace()
        try:
            response = self._fastpath.get_sandbox_diagnostics(namespace, sandbox_id, limit)
        except FastPathError as exc:
            raise self._fastpath_http_error(exc) from exc
        lines = [
            f"[{event.timestamp_unix_nano}] {event.level} {event.source}/{event.phase}: {event.message}"
            for event in response.events
        ]
        content = json.dumps(
            {
                "sandbox_id": sandbox_id,
                "runtime_state": _enum_name(pb2.RuntimeState, response.sandbox.runtime.state),
                "data_plane_state": _enum_name(
                    pb2.DataPlaneState, response.sandbox.data_plane.state
                ),
                "assignment_state": response.assignment_state,
                "events": lines,
            },
            indent=2,
        )
        return DiagnosticResult(
            sandbox_id=sandbox_id,
            kind=kind,  # type: ignore[arg-type]
            scope=scope,
            content=content,
        )

    # -- endpoints ---------------------------------------------------------

    def get_endpoint(
        self,
        sandbox_id: str,
        port: int,
        resolve_internal: bool = False,
        expires: Optional[int] = None,
        use_proxy_host: bool = False,
    ) -> Endpoint:
        # The stable tenant-scoped gateway route (T6, OSEP-0007 Phase 1a)
        # is not implemented yet; endpoint discovery is a separate work item.
        raise self._unsupported("get_endpoint on fleets (phase 1a)")

    # -- ExtensionService --------------------------------------------------

    def get_access_renew_extend_seconds(self, sandbox_id: str) -> Optional[int]:
        return None

    # -- helpers -----------------------------------------------------------

    def _unsupported(self, feature: str) -> HTTPException:
        return HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.FLEETS_UNSUPPORTED,
                "message": f"{feature} is not supported on fleets (OSEP-0007 Phase 1a).",
            },
        )

    def _fastpath_http_error(self, exc: FastPathError) -> HTTPException:
        """Map a typed FastPath error to the public HTTP contract."""
        if isinstance(exc, FastPathNotFound):
            return HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail={
                    "code": SandboxErrorCodes.FLEETS_SANDBOX_NOT_FOUND,
                    "message": "Sandbox not found.",
                },
            )
        if isinstance(exc, FastPathInvalidArgument):
            return HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": exc.message,
                },
            )
        if isinstance(exc, FastPathConflict):
            return HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "code": SandboxErrorCodes.FLEETS_API_ERROR,
                    "message": exc.message,
                },
            )
        if isinstance(exc, FastPathUnavailable):
            return HTTPException(
                status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
                detail={
                    "code": SandboxErrorCodes.FLEETS_API_ERROR,
                    "message": "FastPath backend unavailable.",
                },
            )
        return HTTPException(
            status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
            detail={
                "code": SandboxErrorCodes.FLEETS_API_ERROR,
                "message": exc.message,
            },
        )


def _enum_name(enum_type, value: int) -> str:
    """Return a stable diagnostic value across FastPath enum version skew."""
    try:
        return enum_type.Name(value)
    except ValueError:
        return str(value)


__all__ = ["FleetSandboxService"]
