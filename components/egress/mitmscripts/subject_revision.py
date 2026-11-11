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

"""Bounded, installation-only subject owner for the internal v2 IPC harness.

One operation lock owns routing, receiver calls, lifecycle publication and the
terminal latch. No socket I/O happens here. Admission is permanently disabled by
InstallationReceiver: successful unload proves installation cleanup with zero
transport obligations, not that live connections were drained.
"""

from __future__ import annotations

import hashlib
import json
import sys
import threading
from dataclasses import dataclass, field
from typing import Any, Callable

from revision_publication import InstallationReceiver
from revision_receiver import Revision, RevisionError, Snapshot

LIFECYCLE_BODY_LIMIT = 8192
RESPONSE_LIMIT = 4096
MAX_SEQUENCE = 2**63 - 1
_DIGEST_PREFIX = b"opensandbox.subject-lifecycle.v2\x00"


class SubjectError(Exception):
    """Fixed public outcome, without identities, payloads or raw commands."""

    def __init__(self, http_status: int, outcome: str) -> None:
        self.http_status = http_status
        self.outcome = outcome
        super().__init__(outcome)


def valid_identity(value: Any, limit: int) -> bool:
    return (
        type(value) is str and 0 < len(value) <= limit
        and not any(0xD800 <= ord(char) <= 0xDFFF for char in value)
    )


def parse_object(raw: bytes) -> dict[str, Any]:
    """Accept UTF-8 JSON only, without duplicate keys at any nesting level."""
    def unique(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError
            result[key] = value
        return result

    def invalid_constant(_value: str) -> None:
        raise ValueError

    try:
        value = json.loads(raw.decode("utf-8"), object_pairs_hook=unique,
                           parse_constant=invalid_constant)
        if type(value) is dict:
            return value
    except (ValueError, UnicodeError, RecursionError):
        pass
    raise SubjectError(400, "malformed_request")


def command_digest(operation: str, raw_body: bytes) -> str:
    if operation not in ("register", "unload") or type(raw_body) is not bytes:
        raise ValueError("invalid lifecycle command")
    return hashlib.sha256(_DIGEST_PREFIX + operation.encode("ascii") + b"\x00" + raw_body).hexdigest()


def _positive_integer(value: Any) -> bool:
    return type(value) is int and 0 < value <= sys.maxsize


def request_body_limit(max_snapshot_bytes: int) -> int:
    if not _positive_integer(max_snapshot_bytes):
        raise ValueError("invalid subject resource envelope")
    encoded = ((max_snapshot_bytes + 2) // 3) * 4
    if encoded > sys.maxsize - LIFECYCLE_BODY_LIMIT:
        raise ValueError("invalid subject resource envelope")
    return encoded + LIFECYCLE_BODY_LIMIT


@dataclass(frozen=True, slots=True, repr=False)
class Target:
    subject_key: str
    subject_generation: str

    def __post_init__(self) -> None:
        if not valid_identity(self.subject_key, 256) or not valid_identity(self.subject_generation, 128):
            raise ValueError("invalid subject target")

    @classmethod
    def from_wire(cls, value: Any) -> Target:
        if type(value) is not dict or set(value) != {"subjectKey", "subjectGeneration"}:
            raise ValueError("invalid subject target")
        return cls(value["subjectKey"], value["subjectGeneration"])


@dataclass(slots=True, repr=False)
class _Entry:
    target: Target
    receiver: InstallationReceiver
    failed: bool = False


@dataclass(frozen=True, slots=True)
class _Result:
    http_status: int
    outcome: str
    completion: str | None = None

    def wire(self) -> dict[str, Any]:
        unload = None if self.completion is None else {
            "routingRevoked": True, "transportObligations": "none", "completion": self.completion,
        }
        return {"httpStatus": self.http_status, "outcome": self.outcome, "unload": unload}


@dataclass(frozen=True, slots=True)
class _Record:
    sequence: int
    operation: str
    raw_body: bytes = field(repr=False)
    digest: str
    result: _Result

    def receipt(self) -> dict[str, Any]:
        return {"lifecycleSequence": self.sequence, "operation": self.operation,
                "commandDigest": self.digest, "result": self.result.wire()}


class SubjectRevisionOwner:
    """Finite live map and one raw lifecycle record; no historical tombstones.

    A receiver exception fences only its entry. An unexpected bookkeeping or
    result-publication exception latches the entire owner terminal before unlock.
    Different subjects have separate revision state, but validation is serialized.
    """

    def __init__(self, control_generation: str, *, max_subjects: int,
                 max_snapshot_bytes: int, retained_payload_budget: int) -> None:
        if (not valid_identity(control_generation, 128)
                or not all(_positive_integer(value) for value in
                           (max_subjects, max_snapshot_bytes, retained_payload_budget))
                or max_subjects > sys.maxsize // 2 // max_snapshot_bytes
                or 2 * max_subjects * max_snapshot_bytes > retained_payload_budget):
            raise ValueError("invalid subject resource envelope")
        request_body_limit(max_snapshot_bytes)
        self._control = control_generation
        self._max_subjects = max_subjects
        self._max_snapshot_bytes = max_snapshot_bytes
        self._entries: dict[str, _Entry] = {}
        self._last: _Record | None = None
        self._operation_lock = threading.Lock()
        self._terminal = False
        self._closed = False

    @property
    def max_snapshot_bytes(self) -> int:
        return self._max_snapshot_bytes

    def _check_owner(self) -> None:
        if self._terminal or self._closed:
            raise SubjectError(500, "internal_error")

    def _run(self, operation: Callable[[], Any]) -> Any:
        with self._operation_lock:
            self._check_owner()
            try:
                return operation()
            except SubjectError:
                raise
            except Exception:  # noqa: BLE001 - uncertain owner publication is terminal
                self._terminal = True
        # Do not retain a potentially payload-bearing exception as context.
        raise SubjectError(500, "internal_error")

    def _check_control(self, value: Any) -> None:
        if not valid_identity(value, 128):
            raise SubjectError(400, "malformed_request")
        if value != self._control:
            raise SubjectError(409, "control_generation_mismatch")

    def _lookup(self, target: Target, *, allow_failed: bool = False) -> _Entry:
        if type(target) is not Target:
            raise SubjectError(400, "malformed_request")
        entry = self._entries.get(target.subject_key)
        if entry is None:
            raise SubjectError(404, "subject_unknown")
        if entry.target != target:
            raise SubjectError(409, "subject_generation_mismatch")
        if entry.failed and not allow_failed:
            raise SubjectError(503, "subject_failed")
        return entry

    def _call(self, entry: _Entry, operation: str, *args: Any) -> Any:
        rejected = failed = False
        try:
            return getattr(entry.receiver, operation)(*args)
        except RevisionError:
            rejected = True
        except Exception:  # noqa: BLE001 - only a located receiver may fail independently
            failed = True
        if rejected:
            raise SubjectError(409, "revision_rejected")
        if failed:
            entry.failed = True
            raise SubjectError(503, "subject_failed")

    def _revision_call(self, operation: str, target: Target, revision: Revision,
                       *args: Any) -> Revision:
        def perform() -> Revision:
            if (type(revision) is not Revision
                    or not valid_identity(revision.control_generation, 128)
                    or not valid_identity(revision.subject_generation, 128)
                    or type(target) is not Target):
                raise SubjectError(400, "malformed_request")
            self._check_control(revision.control_generation)
            if target.subject_generation != revision.subject_generation:
                raise SubjectError(409, "subject_generation_mismatch")
            return self._call(self._lookup(target), operation, revision, *args)
        return self._run(perform)

    def prepare(self, target: Target, revision: Revision, payload: bytes) -> Revision:
        return self._revision_call("prepare", target, revision, payload)

    def commit(self, target: Target, revision: Revision) -> Revision:
        return self._revision_call("commit", target, revision)

    def abort(self, target: Target, revision: Revision) -> Revision:
        return self._revision_call("abort", target, revision)

    def readback(self, target: Target) -> Revision | None:
        return self._run(lambda: self._call(self._lookup(target), "readback"))

    def acquire(self, target: Target) -> Snapshot | None:
        """In-process test inspection only; never exposes a registry or admission."""
        return self._run(lambda: self._call(self._lookup(target), "acquire"))

    def _register(self, target: Target) -> _Result:
        entry = self._entries.get(target.subject_key)
        if entry is not None:
            if entry.target != target:
                return _Result(409, "subject_conflict")
            return _Result(503, "subject_failed") if entry.failed else _Result(200, "already_registered")
        if len(self._entries) >= self._max_subjects:
            return _Result(503, "subject_capacity_exhausted")
        try:
            receiver = InstallationReceiver(self._control, target.subject_generation,
                                            max_snapshot_bytes=self._max_snapshot_bytes)
        except Exception:  # noqa: BLE001 - no entry has been inserted yet
            return _Result(503, "subject_failed")
        self._entries[target.subject_key] = _Entry(target, receiver)
        return _Result(200, "registered")

    def _unload(self, target: Target) -> _Result:
        entry = self._entries.get(target.subject_key)
        if entry is None:
            return _Result(200, "already_absent", "terminal")
        if entry.target != target:
            return _Result(409, "subject_generation_mismatch")
        entry.failed = True
        try:
            entry.receiver.close()
        except Exception:  # noqa: BLE001 - keep the failed receiver and its capacity
            return _Result(503, "subject_failed", "fenced")
        del self._entries[target.subject_key]
        return _Result(200, "unloaded", "terminal")

    def lifecycle(self, operation: str, raw_body: bytes) -> dict[str, Any]:
        if operation not in ("register", "unload") or type(raw_body) is not bytes:
            raise SubjectError(400, "malformed_request")
        if len(raw_body) > LIFECYCLE_BODY_LIMIT:
            raise SubjectError(413, "request_too_large")
        value = parse_object(raw_body)
        if set(value) != {"controlGeneration", "lifecycleSequence", "target"}:
            raise SubjectError(400, "malformed_request")
        self._check_control(value["controlGeneration"])
        sequence = value["lifecycleSequence"]
        if type(sequence) is not int or not 1 <= sequence <= MAX_SEQUENCE:
            raise SubjectError(400, "malformed_request")

        def perform() -> dict[str, Any]:
            last = self._last
            last_sequence = 0 if last is None else last.sequence
            if sequence == last_sequence:
                if last.operation == operation and last.raw_body == raw_body:
                    return last.receipt()
                raise SubjectError(409, "lifecycle_sequence_conflict")
            if sequence < last_sequence:
                raise SubjectError(409, "stale_lifecycle_sequence")
            if sequence != last_sequence + 1:
                raise SubjectError(409, "sequence_gap")
            try:
                target = Target.from_wire(value["target"])
            except ValueError:
                result = _Result(400, "invalid_target")
            else:
                result = self._register(target) if operation == "register" else self._unload(target)
            record = _Record(sequence, operation, raw_body, command_digest(operation, raw_body), result)
            receipt = record.receipt()
            # Sequence and frozen result publish together under the operation lock.
            self._last = record
            return receipt
        return self._run(perform)

    def lifecycle_status(self, control_generation: str) -> dict[str, Any]:
        self._check_control(control_generation)
        def status() -> dict[str, Any]:
            if self._last is None:
                return {"lastSequence": 0, "operation": None, "commandDigest": None, "result": None}
            receipt = self._last.receipt()
            return {"lastSequence": receipt.pop("lifecycleSequence"), **receipt}
        return self._run(status)

    def close(self) -> None:
        """Fence the process and try every receiver, retaining any failed closes."""
        with self._operation_lock:
            self._closed = True
            failed = False
            try:
                for key, entry in tuple(self._entries.items()):
                    entry.failed = True
                    try:
                        entry.receiver.close()
                    except Exception:  # noqa: BLE001 - try all entries before reporting
                        failed = True
                    else:
                        del self._entries[key]
                self._last = None
            except Exception:  # noqa: BLE001
                self._terminal = True
                failed = True
        if failed:
            raise SubjectError(500, "internal_error")
