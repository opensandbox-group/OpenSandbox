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
import logging
import time
from typing import Optional
from uuid import UUID

from kubernetes.client import ApiException

from opensandbox_server.services.snapshot_models import SnapshotState
from opensandbox_server.services.snapshot_runtime import SnapshotRuntimeStatus

logger = logging.getLogger(__name__)

_GROUP = "sandbox.opensandbox.io"
_VERSION = "v1alpha1"
_PLURAL = "sandboxsnapshots"

PUBLIC_SNAPSHOT_NAME_PREFIX = "osb-snap-"
PUBLIC_SNAPSHOT_TAG_PREFIX = "snap-"
PUBLIC_SNAPSHOT_SCOPE_LABEL = "opensandbox.io/snapshot-scope"
PUBLIC_SNAPSHOT_ID_LABEL = "opensandbox.io/snapshot-id"
PUBLIC_SNAPSHOT_SOURCE_SANDBOX_ID_LABEL = "opensandbox.io/source-sandbox-id"
PUBLIC_SNAPSHOT_SCOPE_VALUE = "public"
MAIN_CONTAINER_NAME = "sandbox"
DEFAULT_WAIT_TIMEOUT_SECONDS = 15 * 60
DEFAULT_POLL_INTERVAL_SECONDS = 2.0


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
    def __init__(
        self,
        k8s_client,
        *,
        namespace: str,
        wait_timeout_seconds: float = DEFAULT_WAIT_TIMEOUT_SECONDS,
        poll_interval_seconds: float = DEFAULT_POLL_INTERVAL_SECONDS,
    ) -> None:
        self._k8s_client = k8s_client
        self._namespace = namespace
        self._wait_timeout_seconds = wait_timeout_seconds
        self._poll_interval_seconds = poll_interval_seconds

    def supports_create_snapshot(self) -> bool:
        return True

    def create_snapshot_unsupported_message(self) -> str:
        return ""

    def create_snapshot(
        self,
        snapshot_id: str,
        sandbox_id: str,
    ) -> Optional[SnapshotRuntimeStatus]:
        snapshot_name = build_public_snapshot_name(snapshot_id)
        body = self._build_snapshot_body(snapshot_id, sandbox_id, snapshot_name)
        should_validate_existing_source = False

        try:
            self._k8s_client.create_custom_object(
                group=_GROUP,
                version=_VERSION,
                namespace=self._namespace,
                plural=_PLURAL,
                body=body,
            )
        except ApiException as exc:
            if exc.status != 409:
                return SnapshotRuntimeStatus(
                    state=SnapshotState.FAILED,
                    reason="snapshot_runtime_create_failed",
                    message=f"Failed to create Kubernetes SandboxSnapshot {snapshot_name}: {exc}",
                )
            logger.info("Kubernetes SandboxSnapshot %s already exists; continuing", snapshot_name)
            should_validate_existing_source = True
        except Exception as exc:  # noqa: BLE001
            logger.exception("Failed to create Kubernetes SandboxSnapshot %s: %s", snapshot_name, exc)
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_runtime_create_failed",
                message=f"Failed to create Kubernetes SandboxSnapshot {snapshot_name}: {exc}",
            )

        if should_validate_existing_source:
            try:
                current = self._get_snapshot_cr(snapshot_name)
            except Exception as exc:  # noqa: BLE001
                logger.warning(
                    "Failed to inspect existing Kubernetes SandboxSnapshot %s after create conflict: %s",
                    snapshot_name,
                    exc,
                )
            else:
                conflict = self._validate_existing_source(current, sandbox_id)
                if conflict is not None:
                    return conflict

        return self._wait_for_terminal_snapshot(snapshot_id)

    def get_snapshot_status(self, snapshot_id: str) -> Optional[SnapshotRuntimeStatus]:
        return self.inspect_snapshot(snapshot_id)

    def delete_snapshot(self, snapshot_id: str, image: Optional[str] = None) -> None:
        snapshot_name = build_public_snapshot_name(snapshot_id)
        try:
            self._k8s_client.delete_custom_object(
                group=_GROUP,
                version=_VERSION,
                namespace=self._namespace,
                plural=_PLURAL,
                name=snapshot_name,
            )
        except ApiException as exc:
            if exc.status == 404:
                logger.info("Kubernetes SandboxSnapshot %s already absent", snapshot_name)
                return
            raise RuntimeError(f"Failed to delete Kubernetes SandboxSnapshot {snapshot_name}: {exc}") from exc

    def inspect_snapshot(self, snapshot_id: str, image: Optional[str] = None) -> SnapshotRuntimeStatus:
        snapshot_name = build_public_snapshot_name(snapshot_id)
        try:
            snapshot = self._get_snapshot_cr(snapshot_name)
        except Exception as exc:  # noqa: BLE001
            logger.warning("Failed to inspect Kubernetes SandboxSnapshot %s: %s", snapshot_name, exc)
            return SnapshotRuntimeStatus(
                state=SnapshotState.CREATING,
                reason="snapshot_runtime_inspect_failed",
                message=f"Failed to inspect Kubernetes SandboxSnapshot {snapshot_name}: {exc}",
            )

        if snapshot is None:
            return SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_recovery_missing_snapshot",
                message=f"Kubernetes SandboxSnapshot {snapshot_name} was not found.",
            )

        return self._snapshot_status_from_cr(snapshot)

    def _build_snapshot_body(self, snapshot_id: str, sandbox_id: str, snapshot_name: str) -> dict:
        return {
            "apiVersion": f"{_GROUP}/{_VERSION}",
            "kind": "SandboxSnapshot",
            "metadata": {
                "name": snapshot_name,
                "namespace": self._namespace,
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

    def _get_snapshot_cr(self, snapshot_name: str) -> Optional[dict]:
        return self._k8s_client.get_custom_object(
            group=_GROUP,
            version=_VERSION,
            namespace=self._namespace,
            plural=_PLURAL,
            name=snapshot_name,
        )

    def _validate_existing_source(self, snapshot: Optional[dict], sandbox_id: str) -> Optional[SnapshotRuntimeStatus]:
        if snapshot is None:
            return None

        existing_sandbox = (
            snapshot.get("spec", {}).get("sandboxName")
            if isinstance(snapshot, dict)
            else None
        )
        if existing_sandbox in (None, sandbox_id):
            return None

        return SnapshotRuntimeStatus(
            state=SnapshotState.FAILED,
            reason="snapshot_runtime_conflict",
            message=(
                "Kubernetes SandboxSnapshot already exists for a different "
                f"source sandbox: {existing_sandbox}"
            ),
        )

    def _wait_for_terminal_snapshot(self, snapshot_id: str) -> SnapshotRuntimeStatus:
        deadline = time.monotonic() + self._wait_timeout_seconds
        while True:
            runtime_status = self.inspect_snapshot(snapshot_id)
            if runtime_status.state in (SnapshotState.READY, SnapshotState.FAILED):
                return runtime_status

            if time.monotonic() >= deadline:
                return SnapshotRuntimeStatus(
                    state=SnapshotState.FAILED,
                    reason="snapshot_runtime_timeout",
                    message=(
                        "Timed out waiting for Kubernetes SandboxSnapshot "
                        f"{build_public_snapshot_name(snapshot_id)} to complete."
                    ),
                )

            time.sleep(self._poll_interval_seconds)

    def _snapshot_status_from_cr(self, snapshot: dict) -> SnapshotRuntimeStatus:
        status = snapshot.get("status", {})
        phase = status.get("phase")

        if phase == "Succeed":
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


__all__ = [
    "KubernetesSnapshotRuntime",
    "build_public_snapshot_name",
    "build_public_snapshot_tag",
]
