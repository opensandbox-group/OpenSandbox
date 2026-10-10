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

"""Sidecar-only joint revision publication for OSEP-0023.

InstallationReceiver adapts this owner to IPC with admissions disabled for its
entire lifetime. LiveReceiver adapts it with live connection and request
admission for the experimental sidecar runtime. Neither owner supplies live
hooks or a public mutation ACK by itself.
"""

from __future__ import annotations

from decision_snapshot import validate
from revision_receiver import Receiver, Revision, RevisionError, Snapshot
from tls_decision import TLSSelectorView, compile_view
from tls_registry import AdmissionToken, BoundConnectionRegistry, RegistryError


class RevisionPublisher:
    """Own one fresh Receiver/Registry pair for its entire generation lifetime.

    Prepare validates bytes and compiles selectors outside both locks. Staging,
    commit, abort and close take Registry -> Receiver, also the request admission
    order. No callbacks run during publication. All allocation and retirement
    planning completes before either active view changes. The public registry
    remains available for admission and lifecycle bookkeeping, but independent
    activate/deactivate and Receiver mutation are disabled for this owned pair.
    """

    def __init__(
        self, control_generation: str, subject_generation: str, *,
        max_snapshot_bytes: int, capacity: int,
        max_abort_records: int = 1024, request_capacity: int | None = None,
        drain_timeout_seconds: int = 30,
    ) -> None:
        self._receiver = Receiver(
            control_generation, subject_generation, validate,
            max_snapshot_bytes=max_snapshot_bytes, max_abort_records=max_abort_records,
        )
        self._registry = BoundConnectionRegistry(
            capacity=capacity, receiver=self._receiver, request_capacity=request_capacity,
            drain_timeout_seconds=drain_timeout_seconds,
        )
        self._receiver._publication_owned = True
        self._registry._publication_owned = True
        self._pending_view: TLSSelectorView | None = None

    @property
    def registry(self) -> BoundConnectionRegistry:
        """Admission and cleanup APIs; publication belongs exclusively to us."""
        return self._registry

    def prepare(self, revision: Revision, payload: bytes) -> Revision:
        """Stage an exact snapshot and its compiled view without activating it."""
        receiver = self._receiver
        with self._registry._lock, receiver._lock:
            receiver._check(revision)
        snapshot = receiver._snapshot(revision, payload)
        with self._registry._lock, receiver._lock:
            if receiver._prepare_check(snapshot):
                return revision
            serial = receiver._serial

        valid = True
        try:
            view = compile_view(snapshot)
        except Exception:  # noqa: BLE001 - compilation errors may contain secrets
            valid = False
        if not valid:
            raise RevisionError("snapshot validation failed")

        with self._registry._lock, receiver._lock:
            if receiver._stage_check(snapshot, serial):
                return revision
            receiver._stage(snapshot)
            self._pending_view = view
        return revision

    def commit(self, revision: Revision) -> tuple[AdmissionToken, ...]:
        """Publish both views and return newly uncovered transports to drain.

        An exact active retry is a no-op, returning no new transports, preserving
        any newer candidate and never extending a retirement deadline. Readback
        identifies the committed revision; this result is not a public ACK.
        """
        receiver, registry = self._receiver, self._registry
        with registry._lock, receiver._lock:
            receiver._check(revision)
            if receiver._active is not None and receiver._active.revision == revision:
                return ()
            if receiver._pending is None or receiver._pending.revision != revision:
                raise RevisionError("revision is not prepared")
            view = self._pending_view
            if view is None or view.revision != revision:
                raise RevisionError("revision is not prepared")
            valid = True
            try:
                activation = registry._plan_activation(view)
                serial = receiver._serial + 1
            except Exception:  # noqa: BLE001 - planning errors may contain secrets
                valid = False
            if not valid:
                raise RegistryError("joint revision publication failed")

            # No validation, allocation or external callbacks after this point.
            # Both locks remain held until both views and all fences are visible.
            registry._publish_activation(activation)
            receiver._active = receiver._pending
            receiver._pending = None
            receiver._serial = serial
            self._pending_view = None
            return activation.uncovered

    def abort(self, revision: Revision) -> Revision:
        """Retire a candidate without discarding unrelated prepared work."""
        with self._registry._lock, self._receiver._lock:
            self._receiver._abort(revision)
            if self._receiver._pending is None:
                self._pending_view = None
            return revision

    def readback(self) -> Revision | None:
        """Return committed metadata only, under the Receiver publication lock."""
        return self._receiver.readback()

    def acquire(self) -> Snapshot | None:
        """Pin immutable bytes; later close never revokes external references."""
        return self._receiver.acquire()

    def close(self) -> tuple[AdmissionToken, ...]:
        """Permanently fence both views; retain memberships for terminal cleanup.

        Existing retirement deadlines are preserved. The caller must actually
        close returned transports; this method neither drains nor cancels work.
        """
        receiver, registry = self._receiver, self._registry
        with registry._lock, receiver._lock:
            tokens = tuple(registry._entries.values())
            receiver._close()
            registry._closed = True
            registry._view = None
            self._pending_view = None
            return tokens


class InstallationReceiver:
    """Receiver-shaped IPC backend with no connection or request admission.

    Exclusively owns a fresh Publisher and never exposes its Registry. The
    admission guard is permanent for this instance and installed before it can
    escape construction. Therefore commit and close have no transport obligations
    to return. Their completion proves coherent installation/fencing only, never
    transport drain or public Vault mutation completion. Live traffic integration
    must use a different owner that consumes Publisher transport obligations.
    """

    def __init__(
        self, control_generation: str, subject_generation: str, *,
        max_snapshot_bytes: int,
    ) -> None:
        publisher = RevisionPublisher(
            control_generation, subject_generation,
            max_snapshot_bytes=max_snapshot_bytes, capacity=1, request_capacity=1,
        )
        # These unused capacities are not a live-traffic sizing recommendation.
        # Guard before exposing the owner, including through this adapter.
        publisher.registry._admission_disabled = True
        self._publisher = publisher

    def prepare(self, revision: Revision, payload: bytes) -> Revision:
        return self._publisher.prepare(revision, payload)

    def commit(self, revision: Revision) -> Revision:
        self._publisher.commit(revision)
        # Do not read back: another commit may already have installed a successor.
        return revision

    def abort(self, revision: Revision) -> Revision:
        return self._publisher.abort(revision)

    def readback(self) -> Revision | None:
        return self._publisher.readback()

    def acquire(self) -> Snapshot | None:
        return self._publisher.acquire()

    def close(self) -> None:
        self._publisher.close()


class LiveReceiver:
    """Receiver-shaped IPC backend with live connection and request admission.

    Exclusively owns a fresh Publisher with real admission budgets, unlike
    InstallationReceiver which permanently disables admission. Commit publishes
    both views and the registry records fences and retirement deadlines for
    newly uncovered connections; the transport owner that consumes
    ``registry`` must drain those connections before a public mutation is
    acknowledged. The IPC wire acknowledgement alone proves installation only.
    """

    def __init__(
        self, control_generation: str, subject_generation: str, *,
        max_snapshot_bytes: int, capacity: int,
        request_capacity: int | None = None, drain_timeout_seconds: int = 30,
    ) -> None:
        self._publisher = RevisionPublisher(
            control_generation, subject_generation,
            max_snapshot_bytes=max_snapshot_bytes, capacity=capacity,
            request_capacity=request_capacity,
            drain_timeout_seconds=drain_timeout_seconds,
        )

    @property
    def registry(self) -> BoundConnectionRegistry:
        """Admission and cleanup APIs; publication stays IPC-owned."""
        return self._publisher.registry

    def prepare(self, revision: Revision, payload: bytes) -> Revision:
        return self._publisher.prepare(revision, payload)

    def commit(self, revision: Revision) -> Revision:
        # Newly uncovered transports stay in the registry with their retirement
        # deadlines; the transport owner closes them through the registry.
        self._publisher.commit(revision)
        # Do not read back: another commit may already have installed a successor.
        return revision

    def abort(self, revision: Revision) -> Revision:
        return self._publisher.abort(revision)

    def readback(self) -> Revision | None:
        return self._publisher.readback()

    def acquire(self) -> Snapshot | None:
        return self._publisher.acquire()

    def close(self) -> None:
        # Returned memberships remain in the registry for the transport owner's
        # terminal cleanup; the process is exiting either way.
        self._publisher.close()
