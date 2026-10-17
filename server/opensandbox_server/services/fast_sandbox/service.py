# pyright: reportAttributeAccessIssue=false
# protobuf-generated modules expose dynamic attributes.

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

"""FastSandboxService: fast-sandbox (fsb) runtime backend.

FastPath owns mutations and live runtime operations. Kubernetes LIST/WATCH
provides the persisted Sandbox fields and eventually convergent observations.
"""

from __future__ import annotations

import asyncio
import json
import logging
from datetime import datetime, timezone
from threading import Lock
from typing import Optional

from fastapi import HTTPException
from kubernetes.client import ApiException
from starlette import status

from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    CreateSandboxResponse,
    Endpoint,
    ListSandboxesRequest,
    ListSandboxesResponse,
    NetworkPolicy,
    NetworkRule,
    PatchSandboxMetadataRequest,
    RenewSandboxExpirationRequest,
    RenewSandboxExpirationResponse,
    Sandbox,
    SandboxStatus,
)
from opensandbox_server.config import AppConfig, KubernetesRuntimeConfig
from opensandbox_server.extensions.keys import IDLE_POLICY_ANNOTATION_KEY
from opensandbox_server.integrations.idle_pause.constants import DEFAULT_PROBE_TIMEOUT_SECONDS
from opensandbox_server.integrations.idle_pause.policy import (
    ActivityProbe as IdlePauseActivityProbe,
)
from opensandbox_server.integrations.idle_pause.policy import (
    idle_policy_to_annotation,
    validate_idle_policy,
)
from opensandbox_server.middleware.request_id import get_request_id
from opensandbox_server.services.activity_recorder import ActivityRecorder
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.diagnostics import (
    DiagnosticResult,
    unsupported_scope_error,
)
from opensandbox_server.services.extension_service import ExtensionService
from opensandbox_server.services.fast_sandbox.create_mapping import (
    UnsupportedFieldError,
    map_create_request,
    map_template_create_request,
)
from opensandbox_server.services.fast_sandbox.fastpath_client import (
    FastPathClient,
    FastPathConflict,
    FastPathError,
    FastPathFailedPrecondition,
    FastPathInvalidArgument,
    FastPathNotFound,
    FastPathResourceExhausted,
    FastPathUnavailable,
)
from opensandbox_server.services.fast_sandbox.endpoint import build_endpoint
from opensandbox_server.services.fast_sandbox.cr_reader import (
    GROUP,
    PLURAL,
    VERSION,
    SandboxCRReader,
)
from opensandbox_server.services.fast_sandbox.cr_mapping import sandbox_from_cr
from opensandbox_server.services.fast_sandbox.network_policy import (
    delete_policy_rules,
    merge_policy_rules,
    normalized_policy,
    policy_status,
)
from opensandbox_server.services.fast_sandbox import vault as vault_relay
from opensandbox_server.services.templates.template_service import FastSandboxTemplateService
from opensandbox_server.services.fast_sandbox.generated import fastpath_pb2 as pb2
from opensandbox_server.services.fast_sandbox.status_mapping import map_reason, map_state
from opensandbox_server.services.sandbox_service import SandboxService
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.services.k8s.list_helpers import _build_list_sandboxes_response
from opensandbox_server.services.validators import (
    ensure_future_expiration,
    ensure_timeout_within_limit,
)

_SUPPORTED_EVENT_SCOPES = ("runtime", "all")

logger = logging.getLogger(__name__)


class FastSandboxService(SandboxService, ExtensionService):
    """sandbox fsb runtime backed by the fast-sandbox FastPath v2 API."""

    def __init__(
        self,
        config: AppConfig,
        fastpath_client: Optional[FastPathClient] = None,
        k8s_client: Optional[K8sClient] = None,
        template_service: Optional[FastSandboxTemplateService] = None,
    ):
        self._app_config = config
        # The fsb backend shares the [kubernetes] block: CR reads and the
        # fast-sandbox (FastPath) settings live side by side there.
        self._k8s = config.kubernetes or KubernetesRuntimeConfig()
        self._fastpath = fastpath_client or FastPathClient(
            endpoint=self._k8s.fastpath_endpoint,
            timeout_seconds=self._k8s.fastpath_timeout_seconds,
        )
        self._tenant_provider = None  # type: ignore[assignment]
        self._k8s_client = k8s_client
        self._k8s_client_lock = Lock()
        self._cr_reader = SandboxCRReader(self._k8s, k8s_client)
        self._template_service = template_service
        self._activity_recorder: Optional[ActivityRecorder] = None
        # Per-sandbox locks serialize the server-side vault PATCH
        # (relay GET → merge → relay PUT) within this process; the revision
        # precondition inside the merged PUT is the cross-replica backstop.
        self._vault_locks: dict[str, Lock] = {}
        self._vault_locks_guard = Lock()

    def vault_lock(self, sandbox_id: str) -> Lock:
        """Return the process-local PATCH lock for one sandbox."""
        with self._vault_locks_guard:
            return self._vault_locks.setdefault(sandbox_id, Lock())

    @property
    def cr_reader(self) -> SandboxCRReader:
        """Informer-cached Sandbox CR reader, for background integrations."""
        return self._cr_reader

    @property
    def fastpath_client(self) -> FastPathClient:
        """Shared FastPath gRPC client, for background integrations."""
        return self._fastpath

    def set_activity_recorder(self, recorder: Optional[ActivityRecorder]) -> None:
        """Inject the OSEP-0024 activity recorder (None disables the writes)."""
        self._activity_recorder = recorder

    def _record_activity(self, sandbox_id: str) -> None:
        """Restart the idle clock after a lifecycle transition (fire-and-forget)."""
        if self._activity_recorder is not None:
            self._activity_recorder.record(sandbox_id)

    def _validated_idle_policy_annotation(
        self, request: CreateSandboxRequest
    ) -> Optional[str]:
        """Validate the declared policy (deployment bounds included) and
        return the annotation value, or None. Raises 400 before any create
        side effect; the TTL bound needs config, so it lives here."""
        lifecycle = request.lifecycle
        if lifecycle is None or lifecycle.idle_policy is None:
            return None
        policy = lifecycle.idle_policy
        probe = policy.active_probe
        try:
            validated = validate_idle_policy(
                idle_timeout_seconds=policy.idle_timeout_seconds,
                on_idle=policy.on_idle,
                wake_on_access=policy.wake_on_access,
                active_probe=(
                    None
                    if probe is None
                    else IdlePauseActivityProbe(
                        port=probe.port,
                        path=probe.path,
                        timeout_seconds=(
                            probe.timeout_seconds
                            if probe.timeout_seconds is not None
                            else DEFAULT_PROBE_TIMEOUT_SECONDS
                        ),
                    )
                ),
                activity_ttl_seconds=self._app_config.automation.idle_pause.activity_ttl_seconds,
            )
        except ValueError as exc:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": str(exc),
                },
            ) from exc
        return idle_policy_to_annotation(validated)

    def _persist_idle_policy_annotation(
        self, namespace: str, sandbox_id: str, annotation: str
    ) -> None:
        """Best-effort CR annotation write: a failed write leaves the sandbox
        unpolicied (never auto-paused or auto-woken), so the create itself
        must not fail after its side effects exist."""
        try:
            self._kubernetes_client().patch_custom_object(
                GROUP,
                VERSION,
                namespace,
                PLURAL,
                sandbox_id,
                body={"metadata": {"annotations": {IDLE_POLICY_ANNOTATION_KEY: annotation}}},
            )
        except ApiException as exc:
            logger.warning(
                f"idle_pause policy annotation write failed sandbox_id={sandbox_id} "
                f"namespace={namespace} status={exc.status}; sandbox stays unpolicied"
            )
        except Exception as exc:
            logger.warning(
                f"idle_pause policy annotation write failed sandbox_id={sandbox_id} "
                f"namespace={namespace} error={exc}; sandbox stays unpolicied"
            )
        else:
            self._cr_reader.invalidate(namespace)

    def _kubernetes_client(self) -> K8sClient:
        # create_sandbox runs in worker threads; lazy init must not race
        # (K8sClient._load_config mutates process-global config).
        with self._k8s_client_lock:
            if self._k8s_client is None:
                self._k8s_client = K8sClient(self._k8s)
            return self._k8s_client

    def close(self) -> None:
        self._cr_reader.close()
        if self._template_service is not None:
            self._template_service.close()
        self._fastpath.close()

    def resolve_template_service(self) -> FastSandboxTemplateService:
        if self._template_service is None:
            self._template_service = FastSandboxTemplateService(self._app_config)
            # Keep template rows converged even without /templates traffic
            # (template-mode create resolves against the synced row).
            self._template_service.start_background_sync()
        return self._template_service

    @staticmethod
    def generate_sandbox_id() -> str:
        return f"fsb-{SandboxService.generate_sandbox_id()}"

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
        return self._k8s.namespace or "default"

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

        default_namespace = self._k8s.namespace or "default"
        namespaces = [default_namespace]
        if self._tenant_provider is not None:
            namespaces.extend(
                entry.namespace
                for entry in self._tenant_provider.list_tenants()
                if entry.namespace != default_namespace
            )
        for namespace in namespaces:
            try:
                self._fastpath.get_sandbox(namespace, sandbox_id)
            except FastPathNotFound:
                continue
            except FastPathError as exc:
                raise self._fastpath_http_error(exc) from exc
            return namespace

        return self._k8s.namespace or "default"

    def _pool_resources(self, namespace: str, pool_ref: str) -> dict:
        """Return the resource profile declared by the selected SandboxPool."""
        try:
            pool = self._fastpath.get_pool(namespace, pool_ref)
        except FastPathNotFound:
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail={
                    "code": SandboxErrorCodes.FSB_POOL_NOT_FOUND,
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
        if request.network_policy is not None:
            self._reject_network_policy_with_upstream_proxy()
        # Validate the opt-in before any side effect.
        idle_policy_annotation = self._validated_idle_policy_annotation(request)
        created_at = datetime.now(timezone.utc)
        # Template mode: the resolved artifact reference becomes
        # the FastPath image; workload shape comes from the golden image.
        template_entrypoint: Optional[list[str]] = None
        if (request.template_id or "").strip():
            image_ref, template_entrypoint = (
                self.resolve_template_service().resolve_template_artifact(
                    request.template_id.strip()
                )
            )
            try:
                create_request = map_template_create_request(
                    request,
                    sandbox_id=self.generate_sandbox_id(),
                    namespace=self._resolve_namespace(),
                    image_ref=image_ref,
                    entrypoint=template_entrypoint,
                    fastpath_resource_pool=self._k8s.fastpath_resource_pool,
                )
            except UnsupportedFieldError as exc:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_PARAMETER,
                        "message": str(exc),
                    },
                ) from exc
        else:
            try:
                create_request = map_create_request(
                    request,
                    sandbox_id=self.generate_sandbox_id(),
                    namespace=self._resolve_namespace(),
                    fastpath_resource_pool=self._k8s.fastpath_resource_pool,
                )
            except UnsupportedFieldError as exc:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_PARAMETER,
                        "message": str(exc),
                    },
                ) from exc
            pool_resources = self._pool_resources(
                create_request.namespace, create_request.pool_ref
            )
            try:
                create_request = map_create_request(
                    request,
                    sandbox_id=create_request.request_id,
                    namespace=create_request.namespace,
                    fastpath_resource_pool=self._k8s.fastpath_resource_pool,
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

        namespace = create_request.namespace
        sandbox_id = create_request.request_id

        try:
            response = self._fastpath.create_sandbox(
                create_request,
                wait_timeout_millis=int(self._k8s.fastpath_wait_ready_seconds * 1000),
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
        finally:
            self._cr_reader.invalidate(namespace)

        # Durable intent exists (accepted or recovery-confirmed): restart the
        # idle clock from this transition (OSEP-0024).
        self._record_activity(sandbox_id)
        if idle_policy_annotation is not None:
            self._persist_idle_policy_annotation(namespace, sandbox_id, idle_policy_annotation)

        return self._build_create_response(
            request,
            info,
            sandbox_id=sandbox_id,
            created_at=created_at,
            expires_at=datetime.fromtimestamp(
                create_request.expires_at_unix_seconds, tz=timezone.utc
            ),
            entrypoint=template_entrypoint,
        )

    def _build_create_response(
        self,
        request: CreateSandboxRequest,
        info: pb2.SandboxInfo,
        *,
        sandbox_id: str,
        created_at: datetime,
        expires_at: datetime,
        entrypoint: Optional[list[str]] = None,
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
            entrypoint=entrypoint if entrypoint is not None else request.entrypoint,
        )

    def get_sandbox(self, sandbox_id: str) -> Sandbox:
        return sandbox_from_cr(self._get_cr(sandbox_id))

    def _get_cr(self, sandbox_id: str) -> dict:
        from opensandbox_server.tenants.context import get_current_tenant

        namespace = self._resolve_namespace()
        if get_current_tenant() is not None or self._tenant_provider is None:
            return self._cr_reader.get(namespace, sandbox_id)
        namespaces = dict.fromkeys(
            [namespace] + [entry.namespace for entry in self._tenant_provider.list_tenants()]
        )
        for candidate in namespaces:
            try:
                return self._cr_reader.get(candidate, sandbox_id)
            except HTTPException as exc:
                if exc.status_code != 404:
                    raise
        raise self._fastpath_http_error(FastPathNotFound("NOT_FOUND", "Sandbox not found."))

    def list_sandbox_objects(self) -> list[Sandbox]:
        return [sandbox_from_cr(cr) for cr in self._cr_reader.list(self._resolve_namespace())]

    def list_sandboxes(self, request: ListSandboxesRequest) -> ListSandboxesResponse:
        return _build_list_sandboxes_response(self.list_sandbox_objects(), request)

    def delete_sandbox(self, sandbox_id: str) -> None:
        # Runtime observations can disappear before the CR during finalization.
        # Deletion needs durable identity, not a live Fastlet Get observation.
        metadata = self._get_cr(sandbox_id)["metadata"]
        namespace = metadata["namespace"]
        try:
            self._fastpath.delete_sandbox(
                namespace,
                sandbox_id,
                expected_uid=metadata["uid"],
            )
        except FastPathError as exc:
            raise self._fastpath_http_error(exc) from exc
        finally:
            self._cr_reader.invalidate(namespace)

    def pause_sandbox(self, sandbox_id: str) -> None:
        """Persist the pause intent; PAUSING -> PAUSED completes asynchronously."""
        metadata = self._get_cr(sandbox_id)["metadata"]
        namespace = metadata["namespace"]
        try:
            self._fastpath.pause_sandbox(
                namespace,
                sandbox_id,
                expected_uid=metadata["uid"],
                request_id=get_request_id() or "",
            )
        except FastPathError as exc:
            raise self._fastpath_http_error(exc) from exc
        finally:
            self._cr_reader.invalidate(namespace)

    def resume_sandbox(self, sandbox_id: str) -> None:
        """Persist the resume intent; fast-sandbox restores the checkpoint
        (possibly on another Fastlet) and routes must be re-resolved."""
        metadata = self._get_cr(sandbox_id)["metadata"]
        namespace = metadata["namespace"]
        try:
            self._fastpath.resume_sandbox(
                namespace,
                sandbox_id,
                expected_uid=metadata["uid"],
                request_id=get_request_id() or "",
            )
        except FastPathError as exc:
            raise self._fastpath_http_error(exc) from exc
        else:
            # The idle clock restarts from the resume transition (OSEP-0024):
            # a stale key or aged tombstone must not cause an immediate pause.
            self._record_activity(sandbox_id)
        finally:
            self._cr_reader.invalidate(namespace)

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
        finally:
            self._cr_reader.invalidate(namespace)
        return RenewSandboxExpirationResponse(expiresAt=normalized)

    def patch_sandbox_metadata(
        self,
        sandbox_id: str,
        patch: PatchSandboxMetadataRequest,
    ) -> Sandbox:
        current = self._get_cr(sandbox_id)
        if not patch:
            return sandbox_from_cr(current)
        self._apply_metadata_patch({}, patch)
        metadata = current["metadata"]
        namespace = metadata["namespace"]
        try:
            self._fastpath.update_metadata(
                namespace,
                sandbox_id,
                upsert={key: value for key, value in patch.items() if value is not None},
                delete_keys=[key for key, value in patch.items() if value is None],
                expected_uid=metadata["uid"],
                expected_generation=metadata["generation"],
            )
        except FastPathError as exc:
            raise self._fastpath_http_error(exc) from exc
        finally:
            self._cr_reader.invalidate(namespace)
        return sandbox_from_cr(self._cr_reader.get(namespace, sandbox_id))

    # -- diagnostics -------------------------------------------------------

    def get_sandbox_log_diagnostics(self, sandbox_id: str, scope: str) -> DiagnosticResult:
        raise self._unsupported("sandbox logs", status.HTTP_501_NOT_IMPLEMENTED)

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
        raise self._unsupported("sandbox logs", status.HTTP_501_NOT_IMPLEMENTED)

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

    # -- credential vault (relay; OSEP-0022) -------------------------------

    def create_credential_vault(self, sandbox_id: str, request) -> dict:
        """Relay POST create-only; the relay response is the sync ack."""
        return vault_relay.relay_create(self, sandbox_id, request)

    def get_credential_vault(self, sandbox_id: str) -> dict:
        """Sanitized vault state — values never leave the server."""
        return vault_relay.relay_get(self, sandbox_id)

    def patch_credential_vault(self, sandbox_id: str, request) -> dict:
        """Server-side merge commit: relay GET → merge → relay PUT."""
        return vault_relay.relay_patch(self, sandbox_id, request)

    def delete_credential_vault(self, sandbox_id: str) -> None:
        """Relay DELETE; the policy is kept."""
        vault_relay.relay_delete(self, sandbox_id)

    def list_credential_vault_credentials(self, sandbox_id: str) -> dict:
        return vault_relay.relay_list_credentials(self, sandbox_id)

    def get_credential_vault_credential(self, sandbox_id: str, name: str) -> dict:
        return vault_relay.relay_get_credential(self, sandbox_id, name)

    def list_credential_vault_bindings(self, sandbox_id: str) -> dict:
        return vault_relay.relay_list_bindings(self, sandbox_id)

    def get_credential_vault_binding(self, sandbox_id: str, name: str) -> dict:
        return vault_relay.relay_get_binding(self, sandbox_id, name)

    # -- endpoints ---------------------------------------------------------

    def get_network_policy(self, sandbox_id: str) -> dict:
        current = self._cr_reader.get(self._resolve_namespace(), sandbox_id)
        binding = next(
            (b for b in current["spec"].get("actionBindings", []) if b["handler"] == "egress"), None
        )
        try:
            raw = binding.get("input") if binding else None
            if isinstance(raw, str) and not raw.strip():
                raw = None
            policy = json.loads(raw) if raw is not None else None
            if policy is not None:
                policy = normalized_policy(NetworkPolicy.model_validate(policy))
        except (ValueError, TypeError, HTTPException) as exc:
            raise HTTPException(503, detail="Invalid persisted egress binding.") from exc
        return policy_status(policy)

    def replace_network_policy(self, sandbox_id: str, policy: NetworkPolicy) -> dict:
        return self._commit_network_policy(sandbox_id, normalized_policy(policy))

    def patch_network_policy(self, sandbox_id: str, rules: list[NetworkRule]) -> dict:
        """Merge rules into the persisted egress binding (sidecar PATCH semantics)."""
        current = self.get_network_policy(sandbox_id)
        merged = merge_policy_rules(current["policy"], rules)
        return self._commit_network_policy(sandbox_id, normalized_policy(NetworkPolicy.model_validate(merged)))

    def delete_network_policy_rules(self, sandbox_id: str, targets: list[str]) -> dict:
        """Remove rules by target from the persisted egress binding (idempotent)."""
        current = self.get_network_policy(sandbox_id)
        kept = delete_policy_rules(current["policy"], targets)
        return self._commit_network_policy(
            sandbox_id,
            normalized_policy(NetworkPolicy.model_validate(kept)),
            guard_upstream_proxy=False,
        )

    def _commit_network_policy(
        self, sandbox_id: str, normalized: dict, *, guard_upstream_proxy: bool = True
    ) -> dict:
        if guard_upstream_proxy:
            self._reject_network_policy_with_upstream_proxy()
        current = self._cr_reader.get(self._resolve_namespace(), sandbox_id)
        metadata = current["metadata"]
        bindings = [dict(b) for b in current["spec"].get("actionBindings", [])]
        replacement = {"handler": "egress", "input": json.dumps(normalized)}
        for index, binding in enumerate(bindings):
            if binding["handler"] == "egress":
                bindings[index] = replacement
                break
        else:
            bindings.append(replacement)
        try:
            self._fastpath.replace_action_bindings(
                metadata["namespace"],
                sandbox_id,
                bindings,
                expected_uid=metadata["uid"],
                expected_generation=metadata["generation"],
            )
        except FastPathError as exc:
            raise self._fastpath_http_error(exc) from exc
        finally:
            self._cr_reader.invalidate(metadata["namespace"])
        return policy_status(normalized)

    def get_endpoint(
        self,
        sandbox_id: str,
        port: int,
        resolve_internal: bool = False,
        expires: Optional[int] = None,
        use_proxy_host: bool = False,
    ) -> Endpoint:
        return build_endpoint(
            self._app_config.ingress, self._resolve_namespace(), sandbox_id, port, expires
        )

    # -- ExtensionService --------------------------------------------------

    def get_access_renew_extend_seconds(self, sandbox_id: str) -> Optional[int]:
        return None

    # -- helpers -----------------------------------------------------------

    def _reject_network_policy_with_upstream_proxy(self) -> None:
        """The shared-Fastlet egress cannot chain through an upstream proxy, so
        networkPolicy create/replace/patch is refused while it is configured.
        Deleting rules stays available so operators can still tear down a
        policy on existing sandboxes."""
        egress = self._app_config.egress
        if egress is None or egress.upstream_proxy is None:
            return
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    "networkPolicy is not supported for fast sandboxes while "
                    "egress.upstream_proxy is configured: the shared-Fastlet "
                    "egress cannot chain through the upstream proxy"
                ),
            },
        )

    def _unsupported(
        self, feature: str, status_code: int = status.HTTP_400_BAD_REQUEST
    ) -> HTTPException:
        return HTTPException(
            status_code=status_code,
            detail={
                "code": SandboxErrorCodes.FSB_UNSUPPORTED,
                "message": f"{feature} is not supported on fsb.",
            },
        )

    def _fastpath_http_error(self, exc: FastPathError) -> HTTPException:
        """Map a typed FastPath error to the public HTTP contract."""
        if isinstance(exc, FastPathResourceExhausted):
            # Upstream may classify runtime admission/create failures (e.g.
            # Firecracker snapshot/KVM errors) as RESOURCE_EXHAUSTED; keep the
            # underlying message visible so it is not mistaken for pool
            # exhaustion (see opensandbox-group/OpenSandbox#2000).
            logger.warning(
                "FastPath resource exhausted: code=%s message=%s", exc.code, exc.message
            )
            return HTTPException(
                status_code=status.HTTP_429_TOO_MANY_REQUESTS,
                headers={"Retry-After": "1"},
                detail={
                    "code": SandboxErrorCodes.FSB_API_ERROR,
                    "message": exc.message,
                    "cause": "FastPath pool capacity is temporarily unavailable.",
                },
            )
        if isinstance(exc, FastPathNotFound):
            return HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail={
                    "code": SandboxErrorCodes.FSB_SANDBOX_NOT_FOUND,
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
        if isinstance(exc, FastPathFailedPrecondition):
            return HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "code": SandboxErrorCodes.FSB_API_ERROR,
                    "message": exc.message,
                },
            )
        if isinstance(exc, FastPathConflict):
            return HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "code": SandboxErrorCodes.FSB_API_ERROR,
                    "message": exc.message,
                },
            )
        if isinstance(exc, FastPathUnavailable):
            logger.warning(
                "FastPath backend unavailable: code=%s message=%s", exc.code, exc.message
            )
            return HTTPException(
                status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
                detail={
                    "code": SandboxErrorCodes.FSB_API_ERROR,
                    "message": "FastPath backend unavailable.",
                },
            )
        return HTTPException(
            status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
            detail={
                "code": SandboxErrorCodes.FSB_API_ERROR,
                "message": exc.message,
            },
        )


def _enum_name(enum_type, value: int) -> str:
    """Return a stable diagnostic value across FastPath enum version skew."""
    try:
        return enum_type.Name(value)
    except ValueError:
        return str(value)


__all__ = ["FastSandboxService"]
