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
"""Durable snapshot/restore orchestration with stable resource identities."""

import asyncio
from datetime import datetime, timezone
import hashlib
import json
import logging
from threading import Event, Thread
import time
from typing import Any
from uuid import uuid4

from fastapi import HTTPException
from pydantic import ValidationError

from opensandbox_server.api.fork_schema import ForkOperation, ForkSandboxRequest
from opensandbox_server.api.schema import CreateSandboxRequest, CreateSnapshotRequest
from opensandbox_server.services.fork_config import build_fork_config, reject
from opensandbox_server.services.snapshot_runtime import SnapshotRuntimePreflightError, SnapshotRuntimeUnsupportedError
from opensandbox_server.tenants.context import get_current_tenant, set_current_tenant
from opensandbox_server.tenants.models import TenantEntry

logger = logging.getLogger(__name__)
OPERATION_TIMEOUT_SECONDS = 1800


def timestamp() -> str:
    return datetime.now(timezone.utc).isoformat()


def owner_scope() -> str:
    tenant = get_current_tenant()
    return json.dumps([tenant.name, tenant.namespace] if tenant else [None, None])


class ForkService:
    def __init__(self, repository, sandbox_service, snapshot_service) -> None:
        self.repository = repository
        self.sandboxes = sandbox_service
        self.snapshots = snapshot_service
        self._stop = Event()
        self._threads = [Thread(target=self._run, name=f"sandbox-fork-{index}", daemon=True) for index in range(4)]

    def start(self) -> None:
        for worker in self._threads:
            worker.start()

    def close(self) -> None:
        self._stop.set()
        for worker in self._threads:
            if worker.is_alive():
                worker.join()
        self.repository.close()

    async def fork(self, source_id: str, request: ForkSandboxRequest, key: str | None = None) -> ForkOperation:
        if key is not None and (not key.strip() or len(key) > 256):
            reject("Idempotency-Key must contain 1 to 256 characters.", "INVALID_REQUEST", 400)
        digest = hashlib.sha256(json.dumps(
            [source_id, request.model_dump(mode="json", by_alias=True, exclude_unset=True)],
            sort_keys=True, separators=(",", ":"),
        ).encode()).hexdigest()
        scope = owner_scope()
        if key:
            existing = await asyncio.to_thread(self.repository.by_key, scope, key)
            if existing:
                return self._replay(existing, digest)
        try:
            config = await build_fork_config(self.sandboxes, source_id, request)
        except ValidationError:
            reject("The inherited configuration or supplied overrides are invalid.", "INVALID_REQUEST", 400)
        tenant = get_current_tenant()
        backend: Any = getattr(self.sandboxes, "_kubernetes", self.sandboxes)
        namespace = tenant.namespace if tenant else None
        if hasattr(backend, "_resolve_namespace_for_lookup"):
            namespace = backend._resolve_namespace_for_lookup(source_id)
        runtime = self.snapshots._snapshot_runtime
        if not runtime.supports_create_snapshot():
            reject("Snapshot capture is unavailable.", "UNSUPPORTED_RUNTIME", 501)
        try:
            await asyncio.to_thread(runtime.preflight_create_snapshot, source_id, namespace=namespace)
        except (SnapshotRuntimePreflightError, SnapshotRuntimeUnsupportedError):
            reject("Source runtime cannot capture a restorable rootfs snapshot.", "RUNTIME_PREFLIGHT_FAILED")
        now = timestamp()
        record = {
            "operation": {"id": str(uuid4()), "sourceSandboxId": source_id,
                          "status": {"state": "Pending"}, "createdAt": now,
                          "updatedAt": now, "cleanupPending": False},
            "digest": digest, "config": config, "snapshot_id": str(uuid4()),
            "target_id": str(uuid4()), "deadline": time.time() + OPERATION_TIMEOUT_SECONDS,
            "tenant": {"name": tenant.name, "namespace": tenant.namespace} if tenant else None,
            "namespace": namespace,
        }
        stored = await asyncio.to_thread(self.repository.create, record, scope, key)
        return self._replay(stored, digest)

    @staticmethod
    def _replay(record: dict, digest: str) -> ForkOperation:
        if record["digest"] != digest:
            reject("Idempotency-Key was used with a different request.", "IDEMPOTENCY_CONFLICT")
        return ForkOperation.model_validate(record["operation"])

    def get(self, fork_id: str) -> ForkOperation:
        record = self.repository.get(fork_id, owner_scope())
        if record is None:
            reject("Fork operation not found.", "NOT_FOUND", 404)
        return ForkOperation.model_validate(record["operation"])

    def _save(self, record: dict, owner: str, *, release: bool = False) -> None:
        record["operation"]["updatedAt"] = timestamp()
        if not self.repository.save(record, owner, release=release):
            raise RuntimeError("Fork lease was lost; stop advancing this operation.")

    def _run(self) -> None:
        while not self._stop.is_set():
            owner = str(uuid4())
            try:
                record = self.repository.claim(owner)
                if record is None:
                    self._stop.wait(2)
                    continue
                self.advance(record, owner)
            except Exception:
                logger.exception("Fork coordinator iteration failed")
                self._stop.wait(2)

    def advance(self, record: dict, owner: str) -> None:
        """One leased reconciliation step; also used by recovery tests."""
        finished = Event()
        lost = Event()
        def heartbeat() -> None:
            while not finished.wait(10):
                try:
                    if not self.repository.renew(record["operation"]["id"], owner):
                        lost.set()
                        return
                except Exception:
                    lost.set()
                    return
        keeper = Thread(target=heartbeat, daemon=True)
        keeper.start()
        set_current_tenant(TenantEntry(**record["tenant"]) if record["tenant"] else None)
        try:
            asyncio.run(self._advance(record, owner, lost))
        finally:
            set_current_tenant(None)
            finished.set()
            keeper.join()

    async def _advance(self, record: dict, owner: str, lost: Event) -> None:
        operation = record["operation"]
        try:
            if operation["status"]["state"] == "Failed":
                await self._cleanup(record)
                self._save(record, owner, release=True)
                return
            if time.time() >= record["deadline"]:
                reject("Fork exceeded its 30 minute deadline.", "TIMEOUT")
            if operation["status"]["state"] == "Pending":
                operation["status"] = {"state": "Snapshotting"}
                self._save(record, owner)
            snapshot = await asyncio.to_thread(
                self.snapshots.create_snapshot, operation["sourceSandboxId"],
                CreateSnapshotRequest(name=None), snapshot_id=record["snapshot_id"], rootfs_only=True,
                capture_namespace=record["namespace"],
            )
            operation["snapshotId"] = snapshot.id
            snapshot = await asyncio.to_thread(self.snapshots.get_snapshot, snapshot.id)
            if snapshot.status.state == "Failed":
                if (getattr(snapshot.status, "reason", None) == "snapshot_runtime_timeout"
                        and not record.get("snapshot_capture_resolved")):
                    record["snapshot_uncertain"] = True
                reject("Source snapshot capture failed.", "SNAPSHOT_FAILED")
            if snapshot.status.state != "Ready":
                self._save(record, owner, release=True)
                return
            operation["status"] = {"state": "Provisioning"}
            self._save(record, owner)
            try:
                target = await asyncio.to_thread(self.sandboxes.get_sandbox, record["target_id"])
                if target.snapshot_id != snapshot.id:
                    reject("Target ID belongs to a different resource.", "RESOURCE_CONFLICT")
                operation["sandboxId"] = target.id
                record["target_observed"] = True
                if target.status.state == "Running":
                    operation["status"] = {"state": "Succeeded"}
                elif target.status.state in {"Failed", "Terminated", "Stopping"}:
                    reject("Target provisioning failed.", "PROVISIONING_FAILED")
                self._save(record, owner, release=True)
                return
            except HTTPException as exc:
                if exc.status_code != 404:
                    raise
            if lost.is_set():
                return
            if record.get("target_submitted"):
                # A previous worker may have submitted the create immediately
                # before losing its lease. Absence is not proof that it failed:
                # never race a still-running request by submitting another.
                self._save(record, owner, release=True)
                return
            request = CreateSandboxRequest(snapshotId=snapshot.id, **record["config"])
            request._operation_sandbox_id = record["target_id"]
            record["target_submitted"] = True
            self._save(record, owner)
            target = await self.sandboxes.create_sandbox(request)
            operation["sandboxId"] = target.id
            record["target_observed"] = True
            if time.time() >= record["deadline"]:
                reject("Fork exceeded its deadline while provisioning.", "TIMEOUT")
            if target.status.state == "Running":
                operation["status"] = {"state": "Succeeded"}
            elif target.status.state in {"Failed", "Terminated", "Stopping"}:
                reject("Target provisioning failed.", "PROVISIONING_FAILED")
            self._save(record, owner, release=True)
        except Exception as exc:
            if lost.is_set():
                return
            reason = "FORK::INTERNAL_ERROR"
            message = "Fork failed; inspect server logs for details."
            if isinstance(exc, HTTPException) and isinstance(exc.detail, dict):
                reason = exc.detail.get("code", reason)
                # Runtime errors may embed credentials; keep the public failure concise.
                message = "Fork failed during snapshot capture or target provisioning."
            logger.exception("Fork %s failed", operation["id"])
            if (record.get("target_submitted") and isinstance(exc, HTTPException)
                    and exc.status_code in {400, 403, 422, 429}):
                record["target_rejected"] = True
            operation["status"] = {"state": "Failed", "reason": reason, "message": message}
            operation["cleanupPending"] = True
            self._save(record, owner)
            await self._cleanup(record)
            self._save(record, owner, release=True)

    async def _cleanup(self, record: dict) -> None:
        operation = record["operation"]
        try:
            try:
                target = await asyncio.to_thread(self.sandboxes.get_sandbox, record["target_id"])
            except HTTPException as exc:
                if exc.status_code != 404:
                    raise
                if record.get("target_submitted") and not record.get("target_observed") and not record.get("target_rejected"):
                    # A disappeared worker or a timed-out runtime request may
                    # still publish its reserved target later. Keep observing.
                    operation["cleanupPending"] = True
                    return
            else:
                if target.snapshot_id != record["snapshot_id"]:
                    reject("Refusing to delete an unrelated target.", "RESOURCE_CONFLICT")
                await asyncio.to_thread(self.sandboxes.delete_sandbox, record["target_id"])
                record["target_observed"] = True
            try:
                snapshot = await asyncio.to_thread(self.snapshots.get_snapshot, record["snapshot_id"])
            except HTTPException as exc:
                if exc.status_code != 404:
                    raise
            else:
                if (getattr(snapshot.status, "reason", None) == "snapshot_runtime_timeout"
                        and not record.get("snapshot_capture_resolved")):
                    record["snapshot_uncertain"] = True
            if record.get("snapshot_uncertain"):
                runtime_status = await asyncio.to_thread(
                    self.snapshots._snapshot_runtime.inspect_snapshot,
                    record["snapshot_id"], source_sandbox_id=operation["sourceSandboxId"],
                    namespace=record["namespace"],
                )
                if runtime_status.state.value != "Ready":
                    operation["cleanupPending"] = True
                    return
                await asyncio.to_thread(
                    self.snapshots._snapshot_runtime.delete_snapshot,
                    record["snapshot_id"], runtime_status.image,
                    source_sandbox_id=operation["sourceSandboxId"],
                    namespace=record["namespace"],
                )
                record["snapshot_uncertain"] = False
                record["snapshot_capture_resolved"] = True
            try:
                await asyncio.to_thread(self.snapshots.delete_snapshot, record["snapshot_id"])
            except HTTPException as exc:
                if exc.status_code != 404:
                    raise
            operation["cleanupPending"] = False
        except Exception:
            operation["cleanupPending"] = True
            logger.warning("Fork cleanup will retry for %s", operation["id"], exc_info=True)
