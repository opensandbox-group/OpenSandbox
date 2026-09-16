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

"""Fast-sandbox snapshot runtime (Path B: submit intent, converge via watch).

``create_snapshot`` submits ``CreateSandboxSnapshot`` over FastPath and returns
a non-terminal status immediately; terminal state is observed by the status
watch on the fast-sandbox ``SandboxSnapshot`` CRs and by callers re-reading
``inspect_snapshot`` (which resolves authoritative status over FastPath).
"""

from __future__ import annotations

# pyright: reportAttributeAccessIssue=false
# protobuf-generated modules expose dynamic attributes.

import logging
from threading import Lock
from typing import Callable, Iterable, Optional
from uuid import UUID

from opensandbox_server.services.fast_sandbox.fastpath_client import (
    FastPathClient,
    FastPathError,
    FastPathNotFound,
    namespaced_reference,
)
from opensandbox_server.services.fast_sandbox.generated import fastpath_pb2 as pb2
from opensandbox_server.services.k8s.snapshot_runtime import (
    PUBLIC_SNAPSHOT_NAME_PREFIX,
    build_public_snapshot_name,
)
from opensandbox_server.services.snapshot_models import SnapshotState
from opensandbox_server.services.snapshot_runtime import (
    SnapshotRuntimePreflightError,
    SnapshotRuntimeStatus,
)

GROUP = "sandbox.fast.io"
VERSION = "v1alpha2"
PLURAL = "sandboxesnapshots"

# FastPath validates snapshot metadata keys as DNS-1123 labels and projects
# them onto CR labels as "metadata.sandbox.fast.io/<key>" (same convention as
# sandbox metadata).
SNAPSHOT_ID_METADATA_KEY = "opensandbox-snapshot-id"
SNAPSHOT_ID_LABEL_KEY = f"metadata.sandbox.fast.io/{SNAPSHOT_ID_METADATA_KEY}"
_SNAPSHOT_ID_LABEL = SNAPSHOT_ID_LABEL_KEY

logger = logging.getLogger(__name__)

# FastPath phase values (SnapshotPhase): PENDING, CREATING, PUBLISHING,
# SUCCEEDED, FAILED. Only the terminal ones carry completion semantics.
_TERMINAL_READY_PHASES = frozenset({"SNAPSHOT_PHASE_SUCCEEDED", "SUCCEEDED"})
_TERMINAL_FAILED_PHASES = frozenset({"SNAPSHOT_PHASE_FAILED", "FAILED"})
_BACKEND_FSB = "fsb"


def snapshot_id_from_crd(obj: dict) -> Optional[str]:
    """Reverse-map a SandboxSnapshot CR to the server snapshot id.

    The id is carried in the ``metadata.sandbox.fast.io/opensandbox-snapshot-id``
    label set at create time; the deterministic CR name
    (``osb-snap-<uuid hex>``) is the fallback.
    """
    metadata = obj.get("metadata") if isinstance(obj, dict) else None
    if not isinstance(metadata, dict):
        return None
    labels = metadata.get("labels") or {}
    snapshot_id = labels.get(_SNAPSHOT_ID_LABEL)
    if snapshot_id:
        return str(snapshot_id)
    name = metadata.get("name") or ""
    if name.startswith(PUBLIC_SNAPSHOT_NAME_PREFIX):
        try:
            return str(UUID(hex=name[len(PUBLIC_SNAPSHOT_NAME_PREFIX) :]))
        except ValueError:
            return None
    return None


class FastSandboxSnapshotRuntime:
    """Non-blocking fsb snapshot runtime backed by FastPath v2."""

    def __init__(
        self,
        fastpath_client: FastPathClient,
        k8s_client,
        *,
        namespace: str,
    ) -> None:
        self._fastpath = fastpath_client
        self._k8s_client = k8s_client
        self._namespace = namespace
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
            self._fastpath.get_sandbox(ns, sandbox_id)
        except FastPathNotFound as exc:
            raise SnapshotRuntimePreflightError(
                f"Cannot snapshot sandbox {sandbox_id}: the fsb sandbox was not found."
            ) from exc
        except FastPathError as exc:
            raise SnapshotRuntimePreflightError(
                f"Cannot verify snapshot runtime for sandbox {sandbox_id}."
            ) from exc

    def create_snapshot(
        self,
        snapshot_id: str,
        sandbox_id: str,
        *,
        namespace: str | None = None,
    ) -> SnapshotRuntimeStatus:
        snapshot_name = build_public_snapshot_name(snapshot_id)
        ns = namespace if namespace is not None else self._namespace
        self._ensure_namespace_watch(ns)

        request = pb2.CreateSandboxSnapshotRequest(
            request_id=snapshot_name,
            sandbox=namespaced_reference(ns, sandbox_id),
            template_name=snapshot_name,
        )
        request.metadata[SNAPSHOT_ID_METADATA_KEY] = snapshot_id
        try:
            self._fastpath.create_sandbox_snapshot(request)
        except FastPathNotFound as exc:
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_runtime_create_failed",
                message=f"Failed to create fsb snapshot {snapshot_name}: source sandbox not found: {exc}",
            )
        except FastPathError as exc:
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_runtime_create_failed",
                message=f"Failed to create fsb snapshot {snapshot_name}: {exc}",
            )
        return SnapshotRuntimeStatus(
            state=SnapshotState.CREATING,
            reason="snapshot_runtime_submitted",
            message=(
                f"fsb snapshot {snapshot_name} accepted in namespace {ns}; "
                "completion is observed via watch."
            ),
            backend=_BACKEND_FSB,
        )

    def get_snapshot_status(self, snapshot_id: str) -> SnapshotRuntimeStatus:
        return self.inspect_snapshot(snapshot_id)

    def delete_snapshot(
        self,
        snapshot_id: str,
        image: Optional[str] = None,
        *,
        namespace: str | None = None,
        source_sandbox_id: str | None = None,
    ) -> None:
        snapshot_name = build_public_snapshot_name(snapshot_id)
        ns = namespace if namespace is not None else self._namespace
        try:
            self._fastpath.delete_sandbox_snapshot(ns, snapshot_name)
        except FastPathNotFound:
            logger.info(f"fsb snapshot {snapshot_name} already absent")
            return
        except FastPathError as exc:
            raise RuntimeError(f"Failed to delete fsb snapshot {snapshot_name}: {exc}") from exc

    def inspect_snapshot(
        self,
        snapshot_id: str,
        image: Optional[str] = None,
        *,
        namespace: str | None = None,
        source_sandbox_id: str | None = None,
    ) -> SnapshotRuntimeStatus:
        snapshot_name = build_public_snapshot_name(snapshot_id)
        ns = namespace if namespace is not None else self._namespace
        try:
            response = self._fastpath.get_sandbox_snapshot(ns, snapshot_name)
        except FastPathNotFound:
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_recovery_missing_snapshot",
                message=f"fsb snapshot {snapshot_name} was not found in namespace {ns}.",
            )
        except FastPathError as exc:
            logger.warning(f"Failed to inspect fsb snapshot {snapshot_name}: {exc}")
            return SnapshotRuntimeStatus(
                state=SnapshotState.CREATING,
                reason="snapshot_runtime_inspect_failed",
                message=f"Failed to inspect fsb snapshot {snapshot_name}: {exc}",
            )

        return self._status_from_info(response.snapshot)

    # -- status watch ------------------------------------------------------

    def start_status_watch(
        self,
        on_change: Callable[[str, str], None],
        namespaces: Iterable[str] = (),
    ) -> None:
        """React to SandboxSnapshot CR changes with ``on_change(snapshot_id, namespace)``.

        One LIST/WATCH informer per namespace that owns snapshot work; every
        watch event and reconciling LIST item invokes the callback. Callbacks
        run on informer threads and must be cheap and thread-safe.
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
                GROUP, VERSION, namespace, PLURAL, self._on_crd_event
            )
        except Exception as exc:  # noqa: BLE001 - the watch must never break creating
            logger.warning(
                f"fsb snapshot status watch for {namespace}/{PLURAL} failed to start: {exc}"
            )
            return
        if informer is None:
            logger.debug(
                f"Informers disabled; fsb snapshot {namespace}/{PLURAL} converges via reads only"
            )
            return
        with self._watch_lock:
            self._watched_namespaces.add(namespace)

    def _on_crd_event(self, event_type: str, obj: dict) -> None:
        snapshot_id = snapshot_id_from_crd(obj)
        if not snapshot_id:
            return
        with self._watch_lock:
            callback = self._on_change
        if callback is None:
            return
        metadata = (obj or {}).get("metadata") or {}
        namespace = metadata.get("namespace") or self._namespace
        try:
            callback(snapshot_id, namespace)
        except Exception as exc:  # noqa: BLE001 - never propagate into the watch
            logger.warning(
                f"fsb snapshot status callback failed for {namespace}/{snapshot_id}: {exc}"
            )

    def close(self) -> None:
        """Stop informer threads owned by the runtime's dedicated client."""
        stop_informers = getattr(self._k8s_client, "stop_informers", None)
        if stop_informers is not None:
            stop_informers()

    # -- status mapping ------------------------------------------------------

    @staticmethod
    def _status_from_info(info) -> SnapshotRuntimeStatus:
        phase = getattr(info, "phase", None)
        phase_name = pb2_phase_name(phase)
        message = (getattr(info, "message", "") or "").strip() or None
        template_name = (getattr(info, "template_name", "") or "").strip() or None

        if phase_name in _TERMINAL_READY_PHASES:
            # The restore image is the snapshot's template name: the artifact
            # set is published under index/<sha256(templateName)>.json, the
            # same content-addressed layout as template golden images. The
            # manifest_ref points at the raw manifest object and is not
            # resolvable as a CreateSandbox image.
            if not template_name:
                return SnapshotRuntimeStatus(
                    state=SnapshotState.FAILED,
                    reason="snapshot_runtime_missing_image",
                    message="fsb snapshot succeeded without a template name.",
                    backend=_BACKEND_FSB,
                )
            return SnapshotRuntimeStatus(
                state=SnapshotState.READY,
                image=template_name,
                reason="snapshot_runtime_ready",
                message="fsb snapshot artifacts published successfully.",
                backend=_BACKEND_FSB,
            )

        if phase_name in _TERMINAL_FAILED_PHASES:
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_runtime_failed",
                message=message or "fsb snapshot creation failed.",
                backend=_BACKEND_FSB,
            )

        return SnapshotRuntimeStatus(
            state=SnapshotState.CREATING,
            reason="snapshot_runtime_in_progress",
            message=message or f"fsb snapshot phase is {phase_name or 'Pending'}.",
            backend=_BACKEND_FSB,
        )


def pb2_phase_name(phase) -> str:
    """Best-effort enum name for a SnapshotPhase value (proto enum or int)."""
    try:
        return phase.name  # proto enum instance
    except AttributeError:
        pass
    try:
        return pb2.SnapshotPhase.Name(int(phase))
    except (TypeError, ValueError):
        return ""


__all__ = [
    "FastSandboxSnapshotRuntime",
    "snapshot_id_from_crd",
]
