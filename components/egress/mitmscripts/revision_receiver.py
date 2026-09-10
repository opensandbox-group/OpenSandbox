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

"""Generation-fenced in-memory snapshot receiver for OSEP-0023.

This foundation has no IPC endpoint or live TLS hooks. The adapter must supply
a pure, bounded validator for the complete payload (including agreement with
the revision metadata). Validation must return None on success and raise on
failure; any other return value is rejected. Transport authentication and
request drain are separate integration responsibilities. Digests cover exact serialized bytes, not a
reserialized JSON object.

No installed revision is unknown state, not authoritative empty Vault state.
An explicit empty snapshot must pass the same prepare/commit path.

Only one active and one prepared snapshot are retained. Returned request handles
are immutable, but existing holders retain their bytes across commit/close.
This module does not promise zeroization or revoke those in-flight handles.
"""

from __future__ import annotations

import hashlib
import threading
from collections.abc import Callable
from dataclasses import dataclass, field


class RevisionError(Exception):
    """A rejected transition; messages never include snapshot contents."""


@dataclass(frozen=True, slots=True)
class Revision:
    control_generation: str
    subject_generation: str
    decision_epoch: int
    vault_revision: int
    policy_epoch: int
    digest: str

    def __post_init__(self) -> None:
        for generation in (self.control_generation, self.subject_generation):
            if type(generation) is not str or not generation or len(generation) > 128:
                raise ValueError("invalid revision generation")
        for value, minimum in (
            (self.decision_epoch, 1),
            (self.vault_revision, 0),
            (self.policy_epoch, 0),
        ):
            if type(value) is not int or not minimum <= value < 2**63:
                raise ValueError("invalid revision counter")
        if (
            type(self.digest) is not str
            or len(self.digest) != 64
            or any(c not in "0123456789abcdef" for c in self.digest)
        ):
            raise ValueError("invalid revision digest")


@dataclass(frozen=True, slots=True)
class Snapshot:
    revision: Revision
    payload: bytes = field(repr=False)


class Receiver:
    """Own one control-plane/subject generation pair.

    The validator runs outside the state lock so teardown never waits for it.
    A concurrent state change invalidates a validation still in progress.
    The future adapter must serialize policy/vault writes before calling here.
    Retries recognize active, pending, and all successfully aborted identities.
    Abort metadata (never payloads) is retained until close, without eviction.
    The lifetime abort budget bounds memory: exhaustion rejects new candidates,
    preserving active state and exact retries. The adapter must provision this
    budget and arrange fenced recovery rather than silently clearing history.
    Recovery after proxy-process loss must reinstall the coordinator's selected
    state before readiness; fencing old transport sessions belongs to the adapter.
    """

    def __init__(
        self,
        control_generation: str,
        subject_generation: str,
        validate: Callable[[Snapshot], None],
        *,
        max_snapshot_bytes: int,
        max_abort_records: int = 1024,
    ) -> None:
        # Reuse the wire-field checks without inventing another identity grammar.
        Revision(control_generation, subject_generation, 1, 0, 0, "0" * 64)
        if not callable(validate):
            raise TypeError("snapshot validator required")
        if type(max_snapshot_bytes) is not int or max_snapshot_bytes <= 0:
            raise ValueError("positive snapshot byte budget required")
        if type(max_abort_records) is not int or max_abort_records <= 0:
            raise ValueError("positive abort history budget required")
        self._control = control_generation
        self._subject = subject_generation
        self._validate = validate
        self._limit = max_snapshot_bytes
        self._lock = threading.Lock()
        self._active: Snapshot | None = None
        self._pending: Snapshot | None = None
        self._aborted: dict[int, Revision] = {}
        self._abort_limit = max_abort_records
        self._highest_epoch = 0
        self._serial = 0
        self._closed = False

    def _check_open(self) -> None:
        if self._closed:
            raise RevisionError("receiver closed")

    def _check(self, revision: Revision) -> None:
        self._check_open()
        if (
            type(revision) is not Revision
            or revision.control_generation != self._control
            or revision.subject_generation != self._subject
        ):
            raise RevisionError("revision generation mismatch")

    def _prepare_check(self, snapshot: Snapshot) -> bool:
        self._check(snapshot.revision)
        if snapshot == self._active or snapshot == self._pending:
            return True
        if self._pending is not None:
            raise RevisionError("another revision is prepared")
        if snapshot.revision.decision_epoch <= self._highest_epoch:
            raise RevisionError("stale or conflicting revision")
        if len(self._aborted) >= self._abort_limit:
            raise RevisionError("abort history capacity exhausted")
        return False

    def prepare(self, revision: Revision, payload: bytes) -> Revision:
        """Validate an immutable candidate without changing active request state."""
        with self._lock:
            self._check(revision)
        if type(payload) is not bytes or len(payload) > self._limit:
            raise RevisionError("invalid snapshot byte budget or type")
        if hashlib.sha256(payload).hexdigest() != revision.digest:
            raise RevisionError("snapshot digest mismatch")
        snapshot = Snapshot(revision, payload)
        with self._lock:
            if self._prepare_check(snapshot):
                return revision
            serial = self._serial

        valid = True
        try:
            valid = self._validate(snapshot) is None
        except Exception:  # noqa: BLE001 - validator errors may contain secrets
            valid = False
        # Raise outside the handler so secret-bearing validator exceptions are
        # not retained as __context__ or included in formatted tracebacks.
        if not valid:
            raise RevisionError("snapshot validation failed")

        with self._lock:
            if self._prepare_check(snapshot):
                return revision
            if self._serial != serial:
                raise RevisionError("receiver changed during validation")
            self._pending = snapshot
            self._highest_epoch = revision.decision_epoch
            self._serial += 1
        return revision

    def commit(self, revision: Revision) -> Revision:
        """Atomically activate a prepared snapshot; exact retries acknowledge it."""
        with self._lock:
            self._check(revision)
            if self._active is not None and self._active.revision == revision:
                return revision
            if self._pending is None or self._pending.revision != revision:
                raise RevisionError("revision is not prepared")
            self._active, self._pending = self._pending, None
            self._serial += 1
            return revision

    def abort(self, revision: Revision) -> Revision:
        """Fence a candidate, even before staging; never undo committed state.

        Canceling an unseen future epoch does not discard a different already
        prepared tuple: that tuple may still commit. The watermark reserves
        retired/staged identities, not an instruction to roll back older work.
        """
        with self._lock:
            self._check(revision)
            if self._active is not None and self._active.revision == revision:
                raise RevisionError("revision already committed")
            retired = self._aborted.get(revision.decision_epoch)
            if retired == revision:
                return revision
            pending_matches = (
                self._pending is not None and self._pending.revision == revision
            )
            if retired is not None or (
                not pending_matches and revision.decision_epoch <= self._highest_epoch
            ):
                raise RevisionError("stale or conflicting revision")
            # A prepared candidate must always retain room for its own abort.
            reserved = int(self._pending is not None and not pending_matches)
            if len(self._aborted) + 1 + reserved > self._abort_limit:
                raise RevisionError("abort history capacity exhausted")
            if pending_matches:
                self._pending = None
            # Abort may overtake prepare or arrive while validation is running.
            # Retire that epoch so delayed work cannot stage it afterwards.
            self._highest_epoch = max(self._highest_epoch, revision.decision_epoch)
            self._aborted[revision.decision_epoch] = revision
            self._serial += 1
            return revision

    def readback(self) -> Revision | None:
        """Return metadata only for reconciliation, never snapshot payload bytes."""
        with self._lock:
            self._check_open()
            return None if self._active is None else self._active.revision

    def acquire(self) -> Snapshot | None:
        """Pin the active immutable snapshot for an in-process request."""
        with self._lock:
            self._check_open()
            return self._active

    def close(self) -> None:
        """Permanently fence this receiver, including validation still in progress."""
        with self._lock:
            self._closed = True
            self._active = self._pending = None
            self._aborted.clear()
            self._serial += 1
