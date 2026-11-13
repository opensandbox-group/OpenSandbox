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

"""Bounded subject installation owner with optional dispatch-only TLS registries.

One operation lock owns routing, receiver calls, lifecycle publication and the
terminal latch. No socket I/O happens here. InstallationReceiver never performs
admission; the optional registries consume only the dispatch view. In B3b-1 an
unload receipt still proves installation cleanup only, not transport teardown.
"""

from __future__ import annotations

import hashlib
import ipaddress
import json
import sys
import threading
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any, Callable
from types import MappingProxyType

from revision_publication import InstallationReceiver
from revision_receiver import Revision, RevisionError, Snapshot
from tls_registry import ConnectionAccounting
if TYPE_CHECKING:
    from subject_admission import SubjectRegistry

LIFECYCLE_BODY_LIMIT = 8192
RESPONSE_LIMIT = 4096
MAX_SEQUENCE = 2**63 - 1
DEFAULT_DRAIN_TIMEOUT_SECONDS = 30
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
    if operation not in ("register", "unload", "suspend") or type(raw_body) is not bytes:
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
    source_addresses: tuple[str, ...] = ()
    snapshot: Snapshot | None = None
    prepared: Revision | None = None
    registry: SubjectRegistry | None = None


@dataclass(frozen=True, slots=True)
class SubjectDispatch:
    target: Target
    snapshot: Snapshot | None
    registry: SubjectRegistry | None = None


def normalize_source(value: str) -> str:
    if type(value) is not str or "%" in value:
        raise ValueError("invalid source address")
    address = ipaddress.ip_address(value)
    # netip.Addr.String uses dotted IPv4 for IPv4-mapped IPv6; ipaddress
    # otherwise renders that suffix in hex. Preserve the registry key's family.
    if isinstance(address, ipaddress.IPv6Address) and address.ipv4_mapped is not None:
        return "::ffff:" + str(address.ipv4_mapped)
    return str(address)


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
                 max_snapshot_bytes: int, retained_payload_budget: int,
                 tls_capacity: int | None = None, process_tls_capacity: int | None = None,
                 static_passthrough: tuple | None = None,
                 drain_timeout_seconds: int | None = None) -> None:
        if (not valid_identity(control_generation, 128)
                or not all(_positive_integer(value) for value in
                           (max_subjects, max_snapshot_bytes, retained_payload_budget))
                or max_subjects > sys.maxsize // 2 // max_snapshot_bytes
                or 2 * max_subjects * max_snapshot_bytes > retained_payload_budget):
            raise ValueError("invalid subject resource envelope")
        supplied = (tls_capacity is not None, process_tls_capacity is not None,
                    static_passthrough is not None)
        if any(supplied) and not all(supplied):
            raise ValueError("incomplete subject TLS admission configuration")
        if drain_timeout_seconds is None:
            # Internal constructors keep the registry default; the addon
            # always passes the launcher handoff value.
            drain_timeout_seconds = DEFAULT_DRAIN_TIMEOUT_SECONDS
        elif not supplied[0]:
            raise ValueError("incomplete subject TLS admission configuration")
        if type(drain_timeout_seconds) is not int or not 1 <= drain_timeout_seconds <= 300:
            raise ValueError("invalid subject TLS drain timeout")
        request_body_limit(max_snapshot_bytes)
        self._control = control_generation
        self._max_subjects = max_subjects
        self._max_snapshot_bytes = max_snapshot_bytes
        self._entries: dict[str, _Entry] = {}
        self._source_index = MappingProxyType({})
        self._quarantined: list[InstallationReceiver] = []
        self._last: _Record | None = None
        self._operation_lock = threading.Lock()
        self._terminal = False
        self._closed = False
        self._tls_capacity = tls_capacity
        self._accounting = None if tls_capacity is None else ConnectionAccounting(process_tls_capacity)
        self._static_passthrough = () if static_passthrough is None else static_passthrough
        self._drain_timeout = drain_timeout_seconds

    @property
    def control_generation(self) -> str:
        return self._control

    def admit_tls(self, address, sni, *, ech_hidden=False, destination_port=None):
        from subject_admission import admit_source

        return admit_source(self, address, sni, ech_hidden=ech_hidden,
                            destination_port=destination_port)[0]

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
                for entry in self._entries.values():
                    if entry.registry is not None:
                        entry.registry.deactivate()
                self._source_index = MappingProxyType({})
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
            entry.snapshot = None
            self._publish_index()
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
            entry = self._lookup(target)
            result = self._call(entry, operation, revision, *args)
            if operation == "prepare":
                # An exact active retry is a receiver no-op: it must neither
                # replace a pending candidate nor restore suspended authority.
                if self._call(entry, "readback") != revision:
                    entry.prepared = revision
            elif operation == "commit" and entry.prepared == revision:
                entry.snapshot = self._call(entry, "acquire")
                entry.prepared = None
                # Synchronous: the registry request fence and retirement
                # deadlines are published before this commit receipt returns.
                self._publish_index()
            elif operation == "abort" and entry.prepared == revision:
                entry.prepared = None
            return result
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
        return self._run(lambda: self._lookup(target).snapshot)

    def lookup_source(self, address: str) -> SubjectDispatch | None:
        """Dispatch an address to a fenced identity and its published snapshot.

        Read one immutable publication without the potentially slow operation
        lock or receiver locks. Previously returned views remain immutable.
        """
        try:
            address = normalize_source(address)
        except ValueError:
            return None
        return self._source_index.get(address)

    def _publish_index(self) -> None:
        for entry in self._entries.values():
            if entry.registry is not None:
                entry.registry.publish_dispatch(entry.snapshot)
        self._source_index = MappingProxyType({
            address: SubjectDispatch(entry.target, entry.snapshot, entry.registry)
            for entry in self._entries.values() for address in entry.source_addresses
        })

    def _register(self, target: Target, addresses: tuple[str, ...]) -> _Result:
        entry = self._entries.get(target.subject_key)
        if entry is not None:
            if entry.target != target or entry.source_addresses != addresses:
                return _Result(409, "subject_conflict")
            return _Result(503, "subject_failed") if entry.failed else _Result(200, "already_registered")
        if any(address in self._source_index for address in addresses):
            return _Result(409, "subject_conflict")
        if len(self._entries) + len(self._quarantined) >= self._max_subjects:
            return _Result(503, "subject_capacity_exhausted")
        try:
            receiver = InstallationReceiver(self._control, target.subject_generation,
                                            max_snapshot_bytes=self._max_snapshot_bytes,
                                            static_passthrough=self._static_passthrough)
        except Exception:  # noqa: BLE001 - no entry has been inserted yet
            return _Result(503, "subject_failed")
        self._entries[target.subject_key] = _Entry(target, receiver, source_addresses=addresses)
        if self._accounting is not None:
            from subject_admission import SubjectRegistry

            self._entries[target.subject_key].registry = SubjectRegistry(
                capacity=self._tls_capacity, accounting=self._accounting,
                static_passthrough=self._static_passthrough,
                drain_timeout_seconds=self._drain_timeout,
            )
        self._publish_index()
        return _Result(200, "registered")

    def _unload(self, target: Target) -> _Result:
        entry = self._entries.get(target.subject_key)
        if entry is None:
            return _Result(200, "already_absent", "terminal")
        if entry.target != target:
            return _Result(409, "subject_generation_mismatch")
        entry.failed = True
        if entry.registry is not None:
            entry.registry.deactivate()
        del self._entries[target.subject_key]
        self._publish_index()  # revoke routing before close, including a slow/failed close
        try:
            entry.receiver.close()
        except Exception:  # noqa: BLE001 - reserve capacity without retaining key/IP
            self._quarantined.append(entry.receiver)
            return _Result(503, "subject_failed", "fenced")
        return _Result(200, "unloaded", "terminal")

    def _suspend(self, target: Target) -> _Result:
        try:
            entry = self._lookup(target)
        except SubjectError as exc:
            return _Result(exc.http_status, exc.outcome)
        entry.snapshot = None
        # A pre-suspend prepare/active retry can still finish its installation
        # transaction/readback, but cannot restore authority. Only a subsequent
        # prepare and successful commit publish a dispatch snapshot again.
        entry.prepared = None
        self._publish_index()
        return _Result(200, "suspended")

    def lifecycle(self, operation: str, raw_body: bytes) -> dict[str, Any]:
        if operation not in ("register", "unload", "suspend") or type(raw_body) is not bytes:
            raise SubjectError(400, "malformed_request")
        if len(raw_body) > LIFECYCLE_BODY_LIMIT:
            raise SubjectError(413, "request_too_large")
        value = parse_object(raw_body)
        keys = {"controlGeneration", "lifecycleSequence", "target"}
        # Empty legacy installation-harness registrations remain valid. They
        # have no source dispatch route and cannot establish a source identity.
        if operation == "register" and "sourceAddresses" in value:
            keys.add("sourceAddresses")
        if set(value) != keys:
            raise SubjectError(400, "malformed_request")
        addresses = ()
        if operation == "register":
            raw_addresses = value.get("sourceAddresses", [])
            try:
                if type(raw_addresses) is not list:
                    raise ValueError
                addresses = tuple(sorted({normalize_source(ip) for ip in raw_addresses}))
            except ValueError:
                raise SubjectError(400, "malformed_request") from None
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
                if operation == "register":
                    result = self._register(target, addresses)
                elif operation == "suspend":
                    result = self._suspend(target)
                else:
                    result = self._unload(target)
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
            self._source_index = MappingProxyType({})
            for entry in self._entries.values():
                if entry.registry is not None:
                    entry.registry.deactivate()
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
                # Keep quarantined receivers charged until this process dies,
                # even if a later best-effort close happens to succeed.
                for receiver in self._quarantined:
                    try:
                        receiver.close()
                    except Exception:  # noqa: BLE001
                        failed = True
            except Exception:  # noqa: BLE001
                self._terminal = True
                failed = True
        if failed:
            raise SubjectError(500, "internal_error")
