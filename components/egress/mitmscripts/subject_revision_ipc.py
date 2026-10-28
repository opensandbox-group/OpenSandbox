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

"""Strict, authenticated, bounded v2 Unix IPC for installation-only subjects.

This endpoint is consumed by the cross-language harness, not production launch
configuration. The v1 handler and its exact-key wire contract are unchanged.
"""

from __future__ import annotations

import base64
import binascii
import hmac
import json
import math
import os
import socket
import socketserver
import stat
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler
from typing import Any

from revision_ipc import ServerError, _BadRequest, _from_wire, _to_wire, _valid_token
from subject_revision import (
    LIFECYCLE_BODY_LIMIT, RESPONSE_LIMIT, SubjectError, SubjectRevisionOwner,
    Target, parse_object, request_body_limit, valid_identity,
)


class _UnixServer(socketserver.UnixStreamServer):
    """At most C workers and one accepted socket waiting in the accept loop."""

    def __init__(self, path: str, handler: type, *, request_timeout: float,
                 max_concurrent_requests: int) -> None:
        self.request_timeout = request_timeout
        self._permits = threading.BoundedSemaphore(max_concurrent_requests)
        self._stopping = threading.Event()
        self._workers_lock = threading.Lock()
        self._workers: set[threading.Thread] = set()
        self._connections: set[socket.socket] = set()
        super().__init__(path, handler)

    def process_request(self, request: socket.socket, client_address: Any) -> None:
        # The sole accept-loop wait is bounded by the request deadline but
        # interruptible on shutdown, even with a very large request timeout.
        deadline = time.monotonic() + self.request_timeout
        acquired = False
        while not self._stopping.is_set():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            acquired = self._permits.acquire(timeout=min(remaining, 0.05))
            if acquired:
                break
        if not acquired:
            self.shutdown_request(request)
            return
        if self._stopping.is_set():
            self._permits.release()
            self.shutdown_request(request)
            return
        thread = None
        try:
            thread = threading.Thread(target=self._worker, args=(request, client_address),
                                      name="subject-revision-request", daemon=True)
            with self._workers_lock:
                self._workers.add(thread)
                self._connections.add(request)
            thread.start()
        except Exception:
            with self._workers_lock:
                self._workers.discard(thread)
                self._connections.discard(request)
            self._permits.release()
            self.shutdown_request(request)
            raise

    def _worker(self, request: socket.socket, address: Any) -> None:
        try:
            self.finish_request(request, address)
        except Exception:  # noqa: BLE001 - no payload, token or identity logging
            pass
        finally:
            try:
                self.shutdown_request(request)
            finally:
                with self._workers_lock:
                    self._connections.discard(request)
                    self._workers.discard(threading.current_thread())
                self._permits.release()

    def handle_error(self, _request: object, _address: object) -> None:
        pass

    def stop_workers(self) -> None:
        with self._workers_lock:
            connections = tuple(self._connections)
            workers = tuple(self._workers)
        for connection in connections:
            try:
                connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        for thread in workers:
            thread.join()


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "OpenSandboxSubjectRevisionIPC/2"
    sys_version = ""

    def setup(self) -> None:
        super().setup()
        self.connection.settimeout(self.server.request_timeout)

    def log_message(self, _format: str, *args: object) -> None:
        pass

    def _reply(self, status: int, value: dict[str, Any]) -> None:
        body = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        if len(body) > RESPONSE_LIMIT:
            status, body = 500, b'{"error":"internal_error"}'
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True
        self.wfile.write(body)

    def _authenticate(self) -> bool:
        values = self.headers.get_all("Authorization", [])
        supplied = values[0] if len(values) == 1 else ""
        if hmac.compare_digest(supplied.encode("utf-8"), ("Bearer " + self.server.session_token).encode("ascii")):
            return True
        self._reply(401, {"error": "unauthorized"})
        return False

    def _body(self, limit: int) -> bytes:
        types = self.headers.get_all("Content-Type", [])
        lengths = self.headers.get_all("Content-Length", [])
        if (self.headers.get_all("Transfer-Encoding", [])
                or types != ["application/json"] or len(lengths) != 1
                or not lengths[0].isascii() or not lengths[0].isdigit()):
            raise SubjectError(400, "malformed_request")
        # Avoid converting an arbitrarily long integer header.
        digits = lengths[0].lstrip("0") or "0"
        if len(digits) > len(str(limit)) or int(digits) > limit:
            raise SubjectError(413, "request_too_large")
        length = int(digits)
        raw = self.rfile.read(length)
        if len(raw) != length:
            raise SubjectError(400, "malformed_request")
        return raw

    def _dispatch(self, operation: str, raw: bytes) -> tuple[int, dict[str, Any]]:
        owner = self.server.owner
        if operation in ("register", "unload"):
            receipt = owner.lifecycle(operation, raw)
            return receipt["result"]["httpStatus"], receipt
        value = parse_object(raw)
        if operation == "lifecycle-status":
            if set(value) != {"controlGeneration"}:
                raise SubjectError(400, "malformed_request")
            return 200, owner.lifecycle_status(value["controlGeneration"])
        expected = {"target", "controlGeneration"} if operation == "active" else {"target", "revision"}
        if operation == "prepare":
            expected.add("payload")
        if set(value) != expected:
            raise SubjectError(400, "malformed_request")
        try:
            target = Target.from_wire(value["target"])
        except ValueError:
            raise SubjectError(400, "malformed_request") from None
        if operation == "active":
            # Check the fixed session before routing, even for unknown targets.
            owner._check_control(value["controlGeneration"])
            return 200, {"revision": _to_wire(owner.readback(target))}
        try:
            revision = _from_wire(value["revision"])
        except _BadRequest:
            raise SubjectError(400, "malformed_request") from None
        if (not valid_identity(revision.control_generation, 128)
                or not valid_identity(revision.subject_generation, 128)):
            raise SubjectError(400, "malformed_request")
        if operation == "prepare":
            encoded = value["payload"]
            if type(encoded) is not str:
                raise SubjectError(400, "malformed_request")
            try:
                payload = base64.b64decode(encoded, validate=True)
            except (binascii.Error, ValueError):
                raise SubjectError(400, "malformed_request") from None
            if base64.b64encode(payload).decode("ascii") != encoded:
                raise SubjectError(400, "malformed_request")
            if len(payload) > self.server.max_snapshot_bytes:
                raise SubjectError(413, "request_too_large")
            acknowledged = owner.prepare(target, revision, payload)
        else:
            acknowledged = getattr(owner, operation)(target, revision)
        return 200, {"revision": _to_wire(acknowledged)}

    def do_POST(self) -> None:
        if not self._authenticate():
            return
        operation = {
            "/v2/subjects/register": "register",
            "/v2/subjects/unload": "unload",
            "/v2/subjects/lifecycle-status": "lifecycle-status",
            "/v2/revisions/prepare": "prepare",
            "/v2/revisions/commit": "commit",
            "/v2/revisions/abort": "abort",
            "/v2/revisions/active": "active",
        }.get(self.path)
        if operation is None:
            self._reply(404, {"error": "not_found"})
            return
        try:
            limit = self.server.max_request_bytes if operation == "prepare" else LIFECYCLE_BODY_LIMIT
            status, value = self._dispatch(operation, self._body(limit))
        except SubjectError as exc:
            status, value = exc.http_status, {"error": exc.outcome}
        except Exception:  # noqa: BLE001 - transport/parser failures never fence the owner
            status, value = 500, {"error": "internal_error"}
        # Keep network failures outside state error classification: a published
        # result remains recoverable even if sending it fails.
        self._reply(status, value)

    def _unsupported(self) -> None:
        if self._authenticate():
            if self.path.startswith("/v1/"):
                self._reply(404, {"error": "not_found"})
            else:
                self._reply(405, {"error": "method_not_allowed"})

    do_GET = do_CONNECT = do_DELETE = do_HEAD = do_OPTIONS = do_PATCH = do_PUT = do_TRACE = _unsupported


class Server:
    """Private Unix socket with an explicit worker budget and one subject owner."""

    def __init__(self, owner: SubjectRevisionOwner, socket_path: str, session_token: str,
                 *, request_timeout: float, max_concurrent_requests: int) -> None:
        valid_timeout = (type(request_timeout) in (int, float)
                         and 0 < request_timeout <= threading.TIMEOUT_MAX
                         and math.isfinite(request_timeout))
        if (type(owner) is not SubjectRevisionOwner or type(socket_path) is not str
                or not os.path.isabs(socket_path) or not _valid_token(session_token)
                or not valid_timeout or type(max_concurrent_requests) is not int
                or not 0 < max_concurrent_requests <= sys.maxsize):
            raise ServerError("invalid subject IPC configuration")
        parent = os.path.dirname(socket_path)
        try:
            parent_stat = os.lstat(parent)
        except OSError:
            raise ServerError("subject IPC parent unavailable") from None
        if (not stat.S_ISDIR(parent_stat.st_mode) or parent_stat.st_uid != os.geteuid()
                or parent_stat.st_mode & 0o022 or os.path.lexists(socket_path)):
            raise ServerError("subject IPC socket path unavailable")
        try:
            server = _UnixServer(socket_path, _Handler, request_timeout=float(request_timeout),
                                 max_concurrent_requests=max_concurrent_requests)
        except OSError:
            raise ServerError("subject IPC socket unavailable") from None
        try:
            socket_stat = os.lstat(socket_path)
            os.chmod(socket_path, 0o600)
        except OSError:
            server.server_close()
            try:
                current = os.lstat(socket_path)
                if "socket_stat" in locals() and (current.st_dev, current.st_ino) == (socket_stat.st_dev, socket_stat.st_ino):
                    os.unlink(socket_path)
            except OSError:
                pass
            raise ServerError("subject IPC socket unavailable") from None
        server.owner = owner
        server.session_token = session_token
        server.max_snapshot_bytes = owner.max_snapshot_bytes
        server.max_request_bytes = request_body_limit(owner.max_snapshot_bytes)
        self._server = server
        self._owner = owner
        self._path = socket_path
        self._socket_identity = (socket_stat.st_dev, socket_stat.st_ino)
        self._thread: threading.Thread | None = None
        self._lock = threading.Lock()
        self._closed = False
        self._path_cleaned = False

    def start(self) -> None:
        with self._lock:
            if self._closed or self._thread is not None:
                raise ServerError("subject IPC server cannot start")
            thread = threading.Thread(target=self._server.serve_forever,
                                      kwargs={"poll_interval": 0.05}, name="subject-revision-ipc", daemon=True)
            try:
                thread.start()
            except RuntimeError:
                raise ServerError("subject IPC server cannot start") from None
            self._thread = thread

    def close(self) -> None:
        # Serialize complete shutdown and cleanup retries. _closed fences start;
        # it does not imply that receiver or filesystem cleanup succeeded.
        with self._lock:
            if not self._closed:
                self._closed = True
                self._server._stopping.set()
                thread = self._thread
                if thread is not None and thread.is_alive():
                    self._server.shutdown()
                self._server.server_close()
                self._server.stop_workers()
            failed = False
            try:
                self._owner.close()
            except SubjectError:
                failed = True
            if not self._path_cleaned:
                try:
                    current = os.lstat(self._path)
                    if (current.st_dev, current.st_ino) == self._socket_identity:
                        os.unlink(self._path)
                    self._path_cleaned = True
                except FileNotFoundError:
                    self._path_cleaned = True
                except OSError:
                    failed = True
            if failed:
                raise ServerError("subject IPC cleanup failed")
