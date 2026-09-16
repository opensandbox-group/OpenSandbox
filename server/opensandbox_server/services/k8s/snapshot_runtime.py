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

"""
Kubernetes-backed public snapshot runtime.
"""

from __future__ import annotations

from hashlib import sha256
import json
import logging
from threading import Lock
from typing import Callable, Iterable, Optional
from uuid import UUID

from kubernetes.client import ApiException

from opensandbox_server.services.snapshot_models import SnapshotState
from opensandbox_server.services.snapshot_runtime import (
    SnapshotRuntimePreflightError,
    SnapshotRuntimeStatus,
    SnapshotRuntimeUnsupportedError,
)

logger = logging.getLogger(__name__)

_GROUP = "sandbox.opensandbox.io"
_VERSION = "v1alpha1"
_PLURAL = "sandboxsnapshots"
_BATCHSANDBOX_PLURAL = "batchsandboxes"
_POOL_ALLOCATION_ANNOTATION = "sandbox.opensandbox.io/alloc-status"
_BATCHSANDBOX_NAME_LABEL = "batch-sandbox.sandbox.opensandbox.io/name"

PUBLIC_SNAPSHOT_NAME_PREFIX = "osb-snap-"
PUBLIC_SNAPSHOT_TAG_PREFIX = "snap-"
PUBLIC_SNAPSHOT_SCOPE_LABEL = "opensandbox.io/snapshot-scope"
PUBLIC_SNAPSHOT_ID_LABEL = "opensandbox.io/snapshot-id"
PUBLIC_SNAPSHOT_SOURCE_SANDBOX_ID_LABEL = "opensandbox.io/source-sandbox-id"
PUBLIC_SNAPSHOT_SCOPE_VALUE = "public"
MAIN_CONTAINER_NAME = "sandbox"


def _stable_hex(value: str) -> str:
    try:
        return UUID(value).hex
    except ValueError:
        return sha256(value.encode("utf-8")).hexdigest()[:32]


def build_public_snapshot_name(snapshot_id: str) -> str:
    return f"{PUBLIC_SNAPSHOT_NAME_PREFIX}{_stable_hex(snapshot_id)}"


def build_public_snapshot_tag(snapshot_id: str) -> str:
    return f"{PUBLIC_SNAPSHOT_TAG_PREFIX}{_stable_hex(snapshot_id)}"


class KubernetesSnapshotRuntime:
    """Non-blocking snapshot runtime: submit intent, converge via watch.

    ``create_snapshot`` persists the SandboxSnapshot CR and returns a
    non-terminal status immediately; terminal state is observed by the status
    watch (``start_status_watch``) and by callers re-reading ``inspect_snapshot``.
    """

    def __init__(
        self,
        k8s_client,
        *,
        namespace: str,
        postgresql_ha_enabled: bool = False,
    ) -> None:
        self._k8s_client = k8s_client
        self._namespace = namespace
        self._postgresql_ha_enabled = postgresql_ha_enabled
        self._snapshot_namespaces: dict[str, str] = {}
        self._watched_namespaces: set[str] = set()
        self._watch_lock = Lock()
        self._on_change: Optional[Callable[[str, str], None]] = None

    def supports_create_snapshot(self) -> bool:
        return True

    def create_snapshot_unsupported_message(self) -> str:
        return ""

    def preflight_create_snapshot(
        self,
        sandbox_id: str,
        *,
        namespace: str | None = None,
    ) -> None:
        ns = namespace if namespace is not None else self._namespace
        try:
            workload = self._k8s_client.get_custom_object(
                group=_GROUP,
                version=_VERSION,
                namespace=ns,
                plural=_BATCHSANDBOX_PLURAL,
                name=sandbox_id,
            )
            if workload is None:
                raise SnapshotRuntimePreflightError(
                    f"Cannot verify snapshot runtime for sandbox {sandbox_id}: "
                    "BatchSandbox was not found."
                )

            source_pod = self._source_pod(
                workload,
                sandbox_id=sandbox_id,
                namespace=ns,
            )
            pod_spec = self._field(source_pod, "spec")
            runtime_class_name = self._field(
                pod_spec,
                "runtimeClassName",
                "runtime_class_name",
            )
            if runtime_class_name is None:
                return
            if not isinstance(runtime_class_name, str) or not runtime_class_name:
                raise SnapshotRuntimePreflightError(
                    f"Cannot verify snapshot runtime for sandbox {sandbox_id}: "
                    "source Pod has an invalid RuntimeClass name."
                )

            runtime_class = self._k8s_client.read_runtime_class(runtime_class_name)
            handler = self._runtime_class_handler(runtime_class)
            if not handler:
                raise SnapshotRuntimePreflightError(
                    f"Cannot verify snapshot runtime for sandbox {sandbox_id}: "
                    f"RuntimeClass {runtime_class_name!r} has no handler."
                )
        except SnapshotRuntimePreflightError:
            raise
        except Exception as exc:  # noqa: BLE001
            raise SnapshotRuntimePreflightError(
                f"Cannot verify snapshot runtime for sandbox {sandbox_id}."
            ) from exc

        if self._is_gvisor_handler(handler):
            raise SnapshotRuntimeUnsupportedError(
                f"gVisor RuntimeClass {runtime_class_name!r} (handler {handler!r}) "
                "does not support the built-in rootfs snapshot committer."
            )

    def create_snapshot(
        self,
        snapshot_id: str,
        sandbox_id: str,
        *,
        namespace: str | None = None,
    ) -> Optional[SnapshotRuntimeStatus]:
        snapshot_name = build_public_snapshot_name(snapshot_id)
        ns = namespace if namespace is not None else self._namespace
        self._snapshot_namespaces[snapshot_id] = ns
        # A fresh namespace may never have been watched; register before any
        # status can change so the reactor observes this snapshot's events.
        self._ensure_namespace_watch(ns)
        body = self._build_snapshot_body(snapshot_id, sandbox_id, snapshot_name, namespace=ns)
        should_validate_existing_source = False

        if self._postgresql_ha_enabled:
            observed = self._observe_before_create(snapshot_name, namespace=ns)
            if isinstance(observed, SnapshotRuntimeStatus):
                return observed
            if observed is not None:
                conflict = self._validate_existing_source(
                    observed,
                    sandbox_id,
                    require_source=True,
                )
                if conflict is not None:
                    return conflict
                # The existing CR may already be terminal (peer recovery);
                # one read lets the caller converge now.
                return self.inspect_snapshot(snapshot_id, namespace=ns)

        try:
            self._k8s_client.create_custom_object(
                group=_GROUP,
                version=_VERSION,
                namespace=ns,
                plural=_PLURAL,
                body=body,
            )
        except ApiException as exc:
            if exc.status != 409:
                if self._postgresql_ha_enabled and self._is_retryable_api_error(exc):
                    return SnapshotRuntimeStatus(
                        state=SnapshotState.CREATING,
                        reason="snapshot_runtime_create_retryable",
                        message=(
                            f"Kubernetes SandboxSnapshot {snapshot_name} create will be retried: {exc}"
                        ),
                    )
                return SnapshotRuntimeStatus(
                    state=SnapshotState.FAILED,
                    reason="snapshot_runtime_create_failed",
                    message=f"Failed to create Kubernetes SandboxSnapshot {snapshot_name}: {exc}",
                )
            logger.info(f"Kubernetes SandboxSnapshot {snapshot_name} already exists; continuing")
            should_validate_existing_source = True
        except Exception as exc:  # noqa: BLE001
            logger.exception(f"Failed to create Kubernetes SandboxSnapshot {snapshot_name}: {exc}")
            if self._postgresql_ha_enabled:
                return SnapshotRuntimeStatus(
                    state=SnapshotState.CREATING,
                    reason="snapshot_runtime_create_retryable",
                    message=(
                        f"Kubernetes SandboxSnapshot {snapshot_name} create will be retried: {exc}"
                    ),
                )
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_runtime_create_failed",
                message=f"Failed to create Kubernetes SandboxSnapshot {snapshot_name}: {exc}",
            )

        if should_validate_existing_source:
            try:
                current = self._get_snapshot_cr(snapshot_name, namespace=ns)
            except Exception as exc:  # noqa: BLE001
                logger.warning(
                    f"Failed to inspect existing Kubernetes SandboxSnapshot "
                    f"{snapshot_name} after create conflict: {exc}"
                )
                if self._postgresql_ha_enabled:
                    return SnapshotRuntimeStatus(
                        state=SnapshotState.CREATING,
                        reason="snapshot_runtime_inspect_failed",
                        message=(
                            f"Failed to inspect Kubernetes SandboxSnapshot {snapshot_name} "
                            f"after create conflict: {exc}"
                        ),
                    )
            else:
                if current is None and self._postgresql_ha_enabled:
                    return SnapshotRuntimeStatus(
                        state=SnapshotState.CREATING,
                        reason="snapshot_runtime_conflict_not_observed",
                        message=(
                            f"Kubernetes SandboxSnapshot {snapshot_name} create conflicted "
                            "but the existing object is not observable yet."
                        ),
                    )
                conflict = self._validate_existing_source(
                    current,
                    sandbox_id,
                    require_source=self._postgresql_ha_enabled,
                )
                if conflict is not None:
                    return conflict
                # The conflicting CR may already be terminal (a peer create
                # that finished); one read lets the caller converge now.
                return self.inspect_snapshot(snapshot_id, namespace=ns)

        return self._submitted_status(snapshot_name, ns)

    @staticmethod
    def _submitted_status(snapshot_name: str, namespace: str) -> SnapshotRuntimeStatus:
        """Non-terminal status returned right after the create intent is durable."""
        return SnapshotRuntimeStatus(
            state=SnapshotState.CREATING,
            reason="snapshot_runtime_submitted",
            message=(
                f"Kubernetes SandboxSnapshot {snapshot_name} accepted in "
                f"namespace {namespace}; completion is observed via watch."
            ),
        )

    # -- status watch ------------------------------------------------------

    def start_status_watch(
        self,
        on_change: Callable[[str, str], None],
        namespaces: Iterable[str] = (),
    ) -> None:
        """React to SandboxSnapshot CR changes with ``on_change(snapshot_id, namespace)``.

        One LIST/WATCH informer per namespace that owns snapshot work (the
        given namespaces plus the configured default); every watch event and
        reconciling LIST item invokes the callback. Callbacks run on informer
        threads and must be cheap and thread-safe.
        """
        with self._watch_lock:
            self._on_change = on_change
        for namespace in sorted({*(ns for ns in namespaces if ns), self._namespace}):
            self._ensure_namespace_watch(namespace)

    def _ensure_namespace_watch(self, namespace: str) -> None:
        with self._watch_lock:
            if namespace in self._watched_namespaces or self._on_change is None:
                return
        watch_custom_objects = getattr(self._k8s_client, "watch_custom_objects", None)
        if watch_custom_objects is None:
            return
        try:
            informer = watch_custom_objects(
                _GROUP, _VERSION, namespace, _PLURAL, self._on_crd_event
            )
        except Exception as exc:  # noqa: BLE001 - the watch must never break creating
            logger.warning(
                f"Snapshot status watch for {namespace}/{_PLURAL} failed to start: {exc}"
            )
            return
        if informer is None:
            logger.debug(
                f"Informers disabled; snapshot {namespace}/{_PLURAL} converges via reads only"
            )
            return
        with self._watch_lock:
            self._watched_namespaces.add(namespace)

    def _on_crd_event(self, event_type: str, obj: dict) -> None:
        metadata = obj.get("metadata") if isinstance(obj, dict) else None
        if not isinstance(metadata, dict):
            return
        snapshot_id = (metadata.get("labels") or {}).get(PUBLIC_SNAPSHOT_ID_LABEL)
        if not snapshot_id:
            return
        with self._watch_lock:
            callback = self._on_change
        if callback is None:
            return
        namespace = metadata.get("namespace") or self._namespace
        try:
            callback(snapshot_id, namespace)
        except Exception as exc:  # noqa: BLE001 - never propagate into the watch
            logger.warning(
                f"Snapshot status callback failed for {namespace}/{snapshot_id}: {exc}"
            )

    def close(self) -> None:
        """Stop informer threads owned by the runtime's dedicated client."""
        stop_informers = getattr(self._k8s_client, "stop_informers", None)
        if stop_informers is not None:
            stop_informers()

    def get_snapshot_status(self, snapshot_id: str) -> Optional[SnapshotRuntimeStatus]:
        ns = self._snapshot_namespaces.get(snapshot_id)
        return self.inspect_snapshot(snapshot_id, namespace=ns)

    def delete_snapshot(
        self,
        snapshot_id: str,
        image: Optional[str] = None,
        *,
        namespace: str | None = None,
        source_sandbox_id: str | None = None,
    ) -> None:
        snapshot_name = build_public_snapshot_name(snapshot_id)
        fallback = namespace if namespace is not None else self._namespace
        ns = self._snapshot_namespaces.pop(snapshot_id, fallback)
        try:
            self._k8s_client.delete_custom_object(
                group=_GROUP,
                version=_VERSION,
                namespace=ns,
                plural=_PLURAL,
                name=snapshot_name,
            )
        except ApiException as exc:
            if exc.status == 404:
                logger.info(f"Kubernetes SandboxSnapshot {snapshot_name} already absent")
                return
            raise RuntimeError(f"Failed to delete Kubernetes SandboxSnapshot {snapshot_name}: {exc}") from exc

    def inspect_snapshot(
        self,
        snapshot_id: str,
        image: Optional[str] = None,
        *,
        namespace: str | None = None,
        source_sandbox_id: str | None = None,
    ) -> SnapshotRuntimeStatus:
        snapshot_name = build_public_snapshot_name(snapshot_id)
        ns = namespace or self._snapshot_namespaces.get(snapshot_id)
        try:
            snapshot = self._get_snapshot_cr(snapshot_name, namespace=ns)
        except Exception as exc:  # noqa: BLE001
            logger.warning(f"Failed to inspect Kubernetes SandboxSnapshot {snapshot_name}: {exc}")
            return SnapshotRuntimeStatus(
                state=SnapshotState.CREATING,
                reason="snapshot_runtime_inspect_failed",
                message=f"Failed to inspect Kubernetes SandboxSnapshot {snapshot_name}: {exc}",
            )

        if snapshot is None:
            return SnapshotRuntimeStatus(
                state=(
                    SnapshotState.CREATING
                    if self._postgresql_ha_enabled
                    else SnapshotState.FAILED
                ),
                reason="snapshot_recovery_missing_snapshot",
                message=(
                    f"Kubernetes SandboxSnapshot {snapshot_name} was not found"
                    + (
                        " and will be created during PostgreSQL recovery."
                        if self._postgresql_ha_enabled
                        else "."
                    )
                ),
            )

        return self._snapshot_status_from_cr(snapshot)

    def _build_snapshot_body(self, snapshot_id: str, sandbox_id: str, snapshot_name: str, *, namespace: str = "default") -> dict:
        return {
            "apiVersion": f"{_GROUP}/{_VERSION}",
            "kind": "SandboxSnapshot",
            "metadata": {
                "name": snapshot_name,
                "namespace": namespace,
                "labels": {
                    PUBLIC_SNAPSHOT_ID_LABEL: snapshot_id,
                    PUBLIC_SNAPSHOT_SOURCE_SANDBOX_ID_LABEL: sandbox_id,
                    PUBLIC_SNAPSHOT_SCOPE_LABEL: PUBLIC_SNAPSHOT_SCOPE_VALUE,
                },
            },
            "spec": {
                "sandboxName": sandbox_id,
            },
        }

    def _source_pod(
        self,
        workload: dict,
        *,
        sandbox_id: str,
        namespace: str,
    ):
        for pod_name in self._allocated_pod_names(workload):
            pod = self._k8s_client.read_pod(namespace, pod_name)
            if self._pod_is_running(pod):
                return pod

        pods = self._k8s_client.list_pods(
            namespace=namespace,
            label_selector=f"{_BATCHSANDBOX_NAME_LABEL}={sandbox_id}",
        )
        for pod in pods:
            if self._pod_is_running(pod):
                return pod

        fallback = self._k8s_client.read_pod(namespace, f"{sandbox_id}-0")
        if self._pod_is_running(fallback):
            return fallback

        raise SnapshotRuntimePreflightError(
            f"Cannot verify snapshot runtime for sandbox {sandbox_id}: "
            "no running source Pod was found."
        )

    @staticmethod
    def _allocated_pod_names(workload: dict) -> list[str]:
        annotations = (workload.get("metadata") or {}).get("annotations") or {}
        raw_allocation = annotations.get(_POOL_ALLOCATION_ANNOTATION)
        if not isinstance(raw_allocation, str):
            return []
        try:
            allocation = json.loads(raw_allocation)
        except (TypeError, ValueError):
            return []
        if not isinstance(allocation, dict):
            return []
        pod_names = allocation.get("pods")
        if not isinstance(pod_names, list):
            return []
        return [name for name in pod_names if isinstance(name, str) and name]

    @classmethod
    def _pod_is_running(cls, pod) -> bool:
        status = cls._field(pod, "status")
        return cls._field(status, "phase") == "Running"

    @staticmethod
    def _field(value, *names: str):
        if isinstance(value, dict):
            for name in names:
                if name in value:
                    return value[name]
            return None
        for name in names:
            field = getattr(value, name, None)
            if field is not None:
                return field
        return None

    @staticmethod
    def _runtime_class_handler(runtime_class) -> str | None:
        if isinstance(runtime_class, dict):
            handler = runtime_class.get("handler")
        else:
            handler = getattr(runtime_class, "handler", None)
        return handler if isinstance(handler, str) and handler else None

    @staticmethod
    def _is_gvisor_handler(handler: str) -> bool:
        normalized = handler.lower()
        return normalized == "runsc" or normalized.startswith("runsc-")

    def _get_snapshot_cr(self, snapshot_name: str, *, namespace: str | None = None) -> Optional[dict]:
        return self._k8s_client.get_custom_object(
            group=_GROUP,
            version=_VERSION,
            namespace=namespace if namespace is not None else self._namespace,
            plural=_PLURAL,
            name=snapshot_name,
        )

    def _validate_existing_source(
        self,
        snapshot: Optional[dict],
        sandbox_id: str,
        *,
        require_source: bool = False,
    ) -> Optional[SnapshotRuntimeStatus]:
        if snapshot is None:
            return None

        existing_sandbox = (
            snapshot.get("spec", {}).get("sandboxName")
            if isinstance(snapshot, dict)
            else None
        )
        if existing_sandbox == sandbox_id or (
            existing_sandbox is None and not require_source
        ):
            return None

        return SnapshotRuntimeStatus(
            state=SnapshotState.FAILED,
            reason="snapshot_runtime_conflict",
            message=(
                "Kubernetes SandboxSnapshot already exists for a different "
                f"source sandbox: {existing_sandbox}"
            ),
        )

    def _snapshot_status_from_cr(self, snapshot: dict) -> SnapshotRuntimeStatus:
        status = snapshot.get("status", {})
        phase = status.get("phase")

        if phase == "Succeed":
            if status.get("format") == "qemu-v1":
                return SnapshotRuntimeStatus(
                    state=SnapshotState.FAILED,
                    reason="snapshot_restore_qemu_not_supported",
                    message=(
                        "QEMU VMState restore is currently supported only by "
                        "BatchSandbox pause/resume; the public snapshot API does "
                        "not yet persist the complete Pod template restore plan."
                    ),
                )
            image_status = self._select_restore_image(status.get("containers") or [])
            if image_status.state == SnapshotState.FAILED:
                return image_status
            return SnapshotRuntimeStatus(
                state=SnapshotState.READY,
                image=image_status.image,
                reason="snapshot_runtime_ready",
                message="Kubernetes snapshot image created successfully.",
            )

        if phase == "Failed":
            reason, message = self._failure_reason_and_message(status)
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason=reason,
                message=message,
            )

        return SnapshotRuntimeStatus(
            state=SnapshotState.CREATING,
            reason="snapshot_runtime_in_progress",
            message=f"Kubernetes SandboxSnapshot phase is {phase or 'Pending'}.",
        )

    def _select_restore_image(self, containers: list[dict]) -> SnapshotRuntimeStatus:
        if not containers:
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_runtime_missing_image",
                message="Kubernetes SandboxSnapshot succeeded without container image status.",
            )

        sandbox_containers = [
            container for container in containers
            if container.get("containerName") == MAIN_CONTAINER_NAME
        ]
        if sandbox_containers:
            image = sandbox_containers[0].get("imageUri")
            if image:
                return SnapshotRuntimeStatus(state=SnapshotState.READY, image=image)

        if len(containers) == 1 and containers[0].get("imageUri"):
            return SnapshotRuntimeStatus(
                state=SnapshotState.READY,
                image=containers[0]["imageUri"],
            )

        return SnapshotRuntimeStatus(
            state=SnapshotState.FAILED,
            reason="snapshot_restore_image_ambiguous",
            message="Kubernetes SandboxSnapshot did not identify a single restorable sandbox container image.",
        )

    @staticmethod
    def _failure_reason_and_message(status: dict) -> tuple[str, str]:
        for condition in status.get("conditions") or []:
            if condition.get("type") == "Failed" and condition.get("status") == "True":
                return (
                    condition.get("reason") or "snapshot_runtime_failed",
                    condition.get("message") or "Kubernetes snapshot creation failed.",
                )
        return (
            "snapshot_runtime_failed",
            "Kubernetes snapshot creation failed.",
        )

    def _observe_before_create(
        self,
        snapshot_name: str,
        *,
        namespace: str,
    ) -> Optional[dict] | SnapshotRuntimeStatus:
        try:
            return self._get_snapshot_cr(snapshot_name, namespace=namespace)
        except ApiException as exc:
            if not self._is_retryable_api_error(exc):
                return SnapshotRuntimeStatus(
                    state=SnapshotState.FAILED,
                    reason="snapshot_runtime_inspect_failed",
                    message=(
                        f"Failed to observe Kubernetes SandboxSnapshot {snapshot_name} "
                        f"before create: {exc}"
                    ),
                )
            logger.warning(
                f"Failed to observe Kubernetes SandboxSnapshot {snapshot_name} "
                f"before create: {exc}"
            )
            return SnapshotRuntimeStatus(
                state=SnapshotState.CREATING,
                reason="snapshot_runtime_inspect_failed",
                message=(
                    f"Failed to observe Kubernetes SandboxSnapshot {snapshot_name} before create: {exc}"
                ),
            )
        except Exception as exc:  # noqa: BLE001
            logger.warning(
                f"Failed to observe Kubernetes SandboxSnapshot {snapshot_name} "
                f"before create: {exc}"
            )
            return SnapshotRuntimeStatus(
                state=SnapshotState.CREATING,
                reason="snapshot_runtime_inspect_failed",
                message=(
                    f"Failed to observe Kubernetes SandboxSnapshot {snapshot_name} before create: {exc}"
                ),
            )

    @staticmethod
    def _is_retryable_api_error(exc: ApiException) -> bool:
        return exc.status in (408, 429) or (
            isinstance(exc.status, int) and exc.status >= 500
        )


__all__ = [
    "KubernetesSnapshotRuntime",
    "build_public_snapshot_name",
    "build_public_snapshot_tag",
]
