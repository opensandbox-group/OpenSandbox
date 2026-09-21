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

"""Authenticated Unix IPC endpoint for the OSEP-0023 revision receiver.

The live addon imports this module only when its launcher hands off a complete
internal session; current egress profiles do not supply one. The future session
owner must provide a fresh token per proxy process and fence readiness and
remote teardown.
"""

from __future__ import annotations

import base64
import binascii
import hmac
import json
import math
import os
import socketserver
import stat
import threading
from http.server import BaseHTTPRequestHandler
from typing import Any

from revision_receiver import Receiver, Revision, RevisionError


class ServerError(Exception):
    """A sanitized IPC configuration or lifecycle error."""


class _BadRequest(Exception):
    pass


class _TooLarge(Exception):
    pass


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result = {}
    for key, value in pairs:
        if key in result:
            raise _BadRequest
        result[key] = value
    return result


def _from_wire(value: Any) -> Revision:
    fields = {
        "controlGeneration",
        "subjectGeneration",
        "decisionEpoch",
        "vaultRevision",
        "policyEpoch",
        "digest",
    }
    if type(value) is not dict or set(value) != fields:
        raise _BadRequest
    try:
        return Revision(
            value["controlGeneration"],
            value["subjectGeneration"],
            value["decisionEpoch"],
            value["vaultRevision"],
            value["policyEpoch"],
            value["digest"],
        )
    except (TypeError, ValueError):
        raise _BadRequest from None


def _to_wire(revision: Revision | None) -> dict[str, Any] | None:
    if revision is None:
        return None
    return {
        "controlGeneration": revision.control_generation,
        "subjectGeneration": revision.subject_generation,
        "decisionEpoch": revision.decision_epoch,
        "vaultRevision": revision.vault_revision,
        "policyEpoch": revision.policy_epoch,
        "digest": revision.digest,
    }


class _UnixServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True

    def handle_error(self, _request: object, _address: object) -> None:
        pass


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "OpenSandboxRevisionIPC/1"
    sys_version = ""

    def setup(self) -> None:
        super().setup()
        self.connection.settimeout(self.server.request_timeout)

    def log_message(self, _format: str, *args: object) -> None:
        pass

    def _reply(self, status: int, value: dict[str, Any]) -> None:
        body = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)
        self.close_connection = True

    def _authenticate(self) -> bool:
        values = self.headers.get_all("Authorization", [])
        supplied = values[0] if len(values) == 1 else ""
        if hmac.compare_digest(supplied, "Bearer " + self.server.session_token):
            return True
        self._reply(401, {"error": "unauthorized"})
        return False

    def _json(self) -> dict[str, Any]:
        if self.headers.get_all("Transfer-Encoding", []):
            raise _BadRequest
        types = self.headers.get_all("Content-Type", [])
        lengths = self.headers.get_all("Content-Length", [])
        if (
            types != ["application/json"]
            or len(lengths) != 1
            or not lengths[0].isdigit()
        ):
            raise _BadRequest
        length = int(lengths[0])
        if length > self.server.max_request_bytes:
            raise _TooLarge
        raw = self.rfile.read(length)
        if len(raw) != length:
            raise _BadRequest
        try:
            value = json.loads(
                raw,
                object_pairs_hook=_unique_object,
                parse_constant=lambda _value: (_ for _ in ()).throw(_BadRequest()),
            )
        except (json.JSONDecodeError, UnicodeDecodeError, RecursionError, _BadRequest):
            raise _BadRequest from None
        if type(value) is not dict:
            raise _BadRequest
        return value

    def _command(self, operation: str) -> None:
        value = self._json()
        expected = {"revision", "payload"} if operation == "prepare" else {"revision"}
        if set(value) != expected:
            raise _BadRequest
        revision = _from_wire(value["revision"])
        if operation == "prepare":
            if type(value["payload"]) is not str:
                raise _BadRequest
            try:
                payload = base64.b64decode(value["payload"], validate=True)
            except (binascii.Error, ValueError):
                raise _BadRequest from None
            if base64.b64encode(payload).decode() != value["payload"]:
                raise _BadRequest
            if len(payload) > self.server.max_snapshot_bytes:
                raise _TooLarge
            acknowledged = self.server.receiver.prepare(revision, payload)
        elif operation == "commit":
            acknowledged = self.server.receiver.commit(revision)
        else:
            acknowledged = self.server.receiver.abort(revision)
        self._reply(200, {"revision": _to_wire(acknowledged)})

    def do_POST(self) -> None:
        if not self._authenticate():
            return
        operation = {
            "/v1/revisions/prepare": "prepare",
            "/v1/revisions/commit": "commit",
            "/v1/revisions/abort": "abort",
        }.get(self.path)
        if operation is None:
            self._reply(404, {"error": "not_found"})
            return
        try:
            self._command(operation)
        except _TooLarge:
            self._reply(413, {"error": "request_too_large"})
        except _BadRequest:
            self._reply(400, {"error": "malformed_request"})
        except RevisionError:
            self._reply(409, {"error": "revision_rejected"})
        except Exception:  # noqa: BLE001 - never expose payload-bearing failures
            self._reply(500, {"error": "internal_error"})

    def do_GET(self) -> None:
        if not self._authenticate():
            return
        if self.path != "/v1/revisions/active":
            self._reply(404, {"error": "not_found"})
            return
        lengths = self.headers.get_all("Content-Length", [])
        if (
            self.headers.get_all("Transfer-Encoding", [])
            or len(lengths) > 1
            or lengths
            and lengths[0] != "0"
        ):
            self._reply(400, {"error": "malformed_request"})
            return
        try:
            self._reply(200, {"revision": _to_wire(self.server.receiver.readback())})
        except RevisionError:
            self._reply(409, {"error": "revision_rejected"})
        except Exception:  # noqa: BLE001 - keep failure details private
            self._reply(500, {"error": "internal_error"})

    def _unsupported(self) -> None:
        if self._authenticate():
            self._reply(405, {"error": "method_not_allowed"})

    do_CONNECT = do_DELETE = do_HEAD = do_OPTIONS = do_PATCH = do_PUT = do_TRACE = (
        _unsupported
    )


def _valid_token(value: str) -> bool:
    return (
        type(value) is str
        and 32 <= len(value) <= 256
        and all(c.isascii() and (c.isalnum() or c in "-_") for c in value)
    )


class Server:
    """Own a private receiver socket and permanently close its receiver."""

    def __init__(
        self,
        receiver: Receiver,
        socket_path: str,
        session_token: str,
        *,
        max_snapshot_bytes: int,
        request_timeout: float,
    ) -> None:
        valid_timeout = (
            type(request_timeout) in (int, float)
            and math.isfinite(request_timeout)
            and request_timeout > 0
        )
        if (
            type(receiver) is not Receiver
            or not os.path.isabs(socket_path)
            or not _valid_token(session_token)
            or type(max_snapshot_bytes) is not int
            or max_snapshot_bytes <= 0
            or not valid_timeout
        ):
            raise ServerError("invalid revision IPC configuration")
        parent = os.path.dirname(socket_path)
        try:
            parent_stat = os.lstat(parent)
        except OSError:
            raise ServerError("revision IPC parent unavailable") from None
        if (
            not stat.S_ISDIR(parent_stat.st_mode)
            or parent_stat.st_uid != os.geteuid()
            or parent_stat.st_mode & 0o022
            or os.path.lexists(socket_path)
        ):
            raise ServerError("revision IPC socket path unavailable")
        try:
            server = _UnixServer(socket_path, _Handler)
        except OSError:
            raise ServerError("revision IPC socket unavailable") from None
        try:
            socket_stat = os.lstat(socket_path)
            os.chmod(socket_path, 0o600)
        except OSError:
            server.server_close()
            try:
                current = os.lstat(socket_path)
                if "socket_stat" in locals() and (
                    current.st_dev,
                    current.st_ino,
                ) == (socket_stat.st_dev, socket_stat.st_ino):
                    os.unlink(socket_path)
            except OSError:
                pass
            raise ServerError("revision IPC socket unavailable") from None
        server.receiver = receiver
        server.session_token = session_token
        server.max_snapshot_bytes = max_snapshot_bytes
        server.max_request_bytes = ((max_snapshot_bytes + 2) // 3) * 4 + 4096
        server.request_timeout = float(request_timeout)
        self._server = server
        self._receiver = receiver
        self._path = socket_path
        self._socket_identity = (socket_stat.st_dev, socket_stat.st_ino)
        self._thread = None
        self._lock = threading.Lock()
        self._closed = False

    def start(self) -> None:
        """Serve after the socket is bound; a second start is rejected."""
        with self._lock:
            if self._closed or self._thread is not None:
                raise ServerError("revision IPC server cannot start")
            thread = threading.Thread(
                target=self._server.serve_forever, name="revision-ipc", daemon=True
            )
            try:
                thread.start()
            except RuntimeError:
                raise ServerError("revision IPC server cannot start") from None
            self._thread = thread

    def close(self) -> None:
        """Idempotently stop IPC, fence the receiver, and remove our socket."""
        with self._lock:
            if self._closed:
                return
            self._closed = True
            thread = self._thread
        if thread is not None and thread.is_alive():
            self._server.shutdown()
        self._server.server_close()
        self._receiver.close()
        try:
            current = os.lstat(self._path)
            if (current.st_dev, current.st_ino) == self._socket_identity:
                os.unlink(self._path)
        except FileNotFoundError:
            pass
        except OSError:
            raise ServerError("revision IPC socket cleanup failed") from None
