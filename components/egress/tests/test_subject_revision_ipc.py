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

"""v2 strict framing, bounded workers and mandatory real Unix socket tests.

SubjectSocketTest intentionally fails rather than skips when Unix sockets are
unavailable. Pure handler/worker tests are independently runnable for diagnosis.
"""

import base64
import io
import http.client
import json
import os
import socket
import stat
import sys
import tempfile
import threading
import time
import unittest
from email.message import Message
from types import SimpleNamespace
from unittest import mock

import test_subject_revision as fixtures
from test_revision_ipc import UnixConnection

subject = fixtures.subject
ipc = fixtures.fixtures.load("subject_revision_ipc")
A, B, CONTROL = fixtures.A, fixtures.B, fixtures.CONTROL
TOKEN = "0123456789abcdef0123456789abcdef"


def prepare(target=A, epoch=1):
    item = fixtures.snapshot(target, epoch)
    return {"target": fixtures.wire(target), "revision": ipc._to_wire(item.revision),
            "payload": base64.b64encode(item.payload).decode()}, item


def wait_for(predicate):
    deadline = time.monotonic() + 3
    while not predicate():
        if time.monotonic() >= deadline:
            raise AssertionError("worker did not finish")
        time.sleep(0.001)


class SubjectHandlerTest(unittest.TestCase):
    def setUp(self):
        self.owner = fixtures.make_owner()
        self.server = SimpleNamespace(owner=self.owner, session_token=TOKEN,
                                      max_snapshot_bytes=2048,
                                      max_request_bytes=subject.request_body_limit(2048))

    def tearDown(self):
        self.owner.close()

    def handler(self, path, value=None, *, raw=None, headers=None):
        raw = json.dumps(value).encode() if raw is None else raw
        handler = object.__new__(ipc._Handler)
        handler.path, handler.server, handler.rfile = path, self.server, io.BytesIO(raw)
        handler.headers = Message()
        for key, value in (headers if headers is not None else [
                ("Authorization", "Bearer " + TOKEN), ("Content-Type", "application/json"),
                ("Content-Length", str(len(raw)))]):
            handler.headers[key] = value
        return handler

    def request(self, path, value=None, *, raw=None, headers=None, method="POST"):
        handler = self.handler(path, value, raw=raw, headers=headers)
        replies = []
        handler._reply = lambda status, body: replies.append((status, body))
        getattr(handler, "do_" + method)()
        self.assertEqual(len(replies), 1)
        return replies[0]

    def test_success_paths_and_status(self):
        self.assertEqual(self.request("/v2/subjects/lifecycle-status", {"controlGeneration": CONTROL}),
                         (200, {"lastSequence": 0, "operation": None, "commandDigest": None, "result": None}))
        first = self.request("/v2/subjects/register", raw=fixtures.command(1))
        self.assertEqual(first[0], 200)
        self.assertEqual(first[1]["result"]["outcome"], "registered")
        active = {"target": fixtures.wire(A), "controlGeneration": CONTROL}
        self.assertEqual(self.request("/v2/revisions/active", active), (200, {"revision": None}))
        value, item = prepare()
        self.assertEqual(self.request("/v2/revisions/prepare", value), (200, {"revision": value["revision"]}))
        self.assertEqual(self.request("/v2/revisions/commit", {key: value[key] for key in ("target", "revision")}),
                         (200, {"revision": value["revision"]}))
        self.assertEqual(self.request("/v2/revisions/active", active), (200, {"revision": value["revision"]}))
        self.assertEqual(self.owner.acquire(A), item)
        aborted, _ = prepare(epoch=2)
        del aborted["payload"]
        self.assertEqual(self.request("/v2/revisions/abort", aborted)[0], 200)
        status = self.request("/v2/subjects/lifecycle-status", {"controlGeneration": CONTROL})[1]
        self.assertEqual(status["lastSequence"], 1)
        self.assertEqual(status["commandDigest"], first[1]["commandDigest"])
        self.assertEqual(self.request("/v2/subjects/unload", raw=fixtures.command(2))[1]["result"]["outcome"], "unloaded")

    def test_authentication_exact_headers_precede_parsing(self):
        for headers in ([('Authorization', 'Bearer ' + 'x' * 32)],
                        [('Authorization', 'Bearer ' + TOKEN), ('Authorization', 'Bearer ' + TOKEN)],
                        [('Authorization', 'Bearer 雪')], []):
            self.assertEqual(self.request("/v2/subjects/register", raw=b'{', headers=headers),
                             (401, {"error": "unauthorized"}))
        self.assertEqual(self.owner.lifecycle_status(CONTROL)["lastSequence"], 0)

    def test_strict_framing(self):
        auth = [("Authorization", "Bearer " + TOKEN)]
        raw = fixtures.command(1)
        good = [("Content-Type", "application/json"), ("Content-Length", str(len(raw)))]
        invalid = [[], good + [("Content-Length", str(len(raw)))],
                   good + [("Transfer-Encoding", "chunked")], good + [("Content-Type", "application/json")],
                   [("Content-Type", "application/json; charset=utf-8"), ("Content-Length", str(len(raw)))],
                   [("Content-Type", "application/json"), ("Content-Length", "-1")],
                   [("Content-Type", "application/json"), ("Content-Length", "١")]]
        for headers in invalid:
            self.assertEqual(self.request("/v2/subjects/register", raw=raw, headers=auth + headers),
                             (400, {"error": "malformed_request"}))
        for length in ("8193", "9" * 10000):
            self.assertEqual(self.request("/v2/subjects/register", raw=raw, headers=auth + [
                ("Content-Type", "application/json"), ("Content-Length", length)]),
                (413, {"error": "request_too_large"}))
        self.assertEqual(self.request("/v2/subjects/register", raw=raw[:-1], headers=auth + good),
                         (400, {"error": "malformed_request"}))
        self.assertEqual(self.owner.lifecycle_status(CONTROL)["lastSequence"], 0)

    def test_malformed_json_outer_keys_and_non_utf8_do_not_consume(self):
        malformed = [b'{', b'[]', b'{"target":{},"target":{}}', fixtures.command(1, extra=1),
                     fixtures.command(1).decode().encode('utf-16'),
                     fixtures.command(1).replace(b'"s-a"', b'NaN'),
                     fixtures.command(1).replace(b'"subjectKey":"s-a"', b'"subjectKey":"s-a","subjectKey":"s-b"')]
        for raw in malformed:
            self.assertEqual(self.request("/v2/subjects/register", raw=raw), (400, {"error": "malformed_request"}))
        self.assertEqual(self.owner.lifecycle_status(CONTROL)["lastSequence"], 0)
        response = self.request("/v2/subjects/register", raw=fixtures.command(1, None))
        self.assertEqual(response[0], 400)
        self.assertEqual(response[1]["result"]["outcome"], "invalid_target")
        self.assertEqual(self.owner.lifecycle_status(CONTROL)["lastSequence"], 1)

    def test_revision_strict_keys_base64_unicode_and_size(self):
        self.owner.lifecycle("register", fixtures.command(1))
        valid, item = prepare()
        bad_values = [{**valid, "extra": 1}, {"target": valid["target"], "revision": valid["revision"]},
                      {**valid, "target": {**valid["target"], "extra": 1}}, {**valid, "target": None},
                      {**valid, "payload": None}, {**valid, "payload": "AB=="},
                      {**valid, "payload": "aA"}, {**valid, "payload": "aA==\n"},
                      {**valid, "revision": {**valid["revision"], "controlGeneration": "\ud800"}},
                      {**valid, "revision": {**valid["revision"], "subjectGeneration": "\udfff"}},
                      {**valid, "revision": {**valid["revision"], "decisionEpoch": True}},
                      {**valid, "revision": {**valid["revision"], "extra": 1}}]
        for value in bad_values:
            self.assertEqual(self.request("/v2/revisions/prepare", value), (400, {"error": "malformed_request"}))
        self.assertEqual(self.request("/v2/revisions/prepare", {**valid, "payload": base64.b64encode(b'x' * 2049).decode()}),
                         (413, {"error": "request_too_large"}))
        self.assertEqual(self.request("/v2/revisions/prepare", raw=b' ' * (self.server.max_request_bytes + 1)),
                         (413, {"error": "request_too_large"}))
        self.assertEqual(self.request("/v2/revisions/prepare", valid)[0], 200)
        self.owner.commit(A, item.revision)
        self.assertEqual(self.owner.acquire(A), item)

    def test_generation_errors_unknown_and_v1_separation(self):
        active = {"target": fixtures.wire(A), "controlGeneration": CONTROL}
        self.assertEqual(self.request("/v2/revisions/active", active), (404, {"error": "subject_unknown"}))
        self.owner.lifecycle("register", fixtures.command(1))
        self.assertEqual(self.request("/v2/revisions/active", {**active, "controlGeneration": "old"}),
                         (409, {"error": "control_generation_mismatch"}))
        self.assertEqual(self.request("/v2/revisions/active", {**active, "target": fixtures.wire(subject.Target("s-a", "wrong"))}),
                         (409, {"error": "subject_generation_mismatch"}))
        for method in ("POST", "GET"):
            self.assertEqual(self.request("/v1/revisions/active", {}, method=method), (404, {"error": "not_found"}))
        self.assertEqual(self.request("/v2/revisions/active", active, method="GET"), (405, {"error": "method_not_allowed"}))
        self.assertEqual(self.request("/v2/revisions/missing", {}), (404, {"error": "not_found"}))

    def test_reply_broken_pipe_and_timeout_do_not_fence_published_result(self):
        for index, error in enumerate((BrokenPipeError, TimeoutError), 1):
            raw = fixtures.command(index)
            handler = self.handler("/v2/subjects/register", raw=raw)
            handler._reply = mock.Mock(side_effect=error)
            with self.assertRaises(error):
                handler.do_POST()
            status = self.owner.lifecycle_status(CONTROL)
            self.assertEqual(status["lastSequence"], index)
            self.assertEqual(status["commandDigest"], subject.command_digest("register", raw))
            self.assertEqual(self.request("/v2/subjects/register", raw=raw)[0], 200)
        self.assertFalse(self.owner._terminal)
        self.assertIsNone(self.owner.readback(A))

    def test_terminal_result_cannot_serve_cached_success(self):
        raw = fixtures.command(1)
        self.request("/v2/subjects/register", raw=raw)
        with mock.patch.object(subject._Record, "receipt", side_effect=RuntimeError("secret")):
            self.assertEqual(self.request("/v2/subjects/register", raw=fixtures.command(2)), (500, {"error": "internal_error"}))
        for path, value in (("/v2/subjects/lifecycle-status", {"controlGeneration": CONTROL}),
                            ("/v2/revisions/active", {"controlGeneration": CONTROL, "target": fixtures.wire(A)})):
            self.assertEqual(self.request(path, value), (500, {"error": "internal_error"}))
        self.assertEqual(self.request("/v2/subjects/register", raw=raw), (500, {"error": "internal_error"}))


class SubjectWorkerTest(unittest.TestCase):
    def server(self, timeout=0.1):
        server = object.__new__(ipc._UnixServer)
        server.request_timeout = timeout
        server._permits = threading.BoundedSemaphore(1)
        server._stopping = threading.Event()
        server._workers_lock = threading.Lock()
        server._workers = set()
        server._connections = set()
        server.shutdown_request = mock.Mock()
        return server

    def test_constructor_and_start_failure_release_permit_and_socket(self):
        for point in ("constructor", "start"):
            server = self.server()
            request = object()
            replacement = mock.Mock(side_effect=RuntimeError) if point == "constructor" else mock.Mock()
            if point == "start":
                replacement.return_value.start.side_effect = RuntimeError
            with mock.patch.object(ipc.threading, "Thread", replacement), self.assertRaises(RuntimeError):
                server.process_request(request, None)
            server.shutdown_request.assert_called_once_with(request)
            self.assertTrue(server._permits.acquire(blocking=False))
            self.assertEqual(server._workers, set())
            self.assertEqual(server._connections, set())

    def test_worker_failure_releases_permit(self):
        for error in (RuntimeError, TimeoutError, BrokenPipeError):
            server = self.server()
            server.finish_request = mock.Mock(side_effect=error)
            server.process_request(object(), None)
            wait_for(lambda: not server._workers)
            self.assertTrue(server._permits.acquire(blocking=False))
            self.assertEqual(server._connections, set())

    def test_shutdown_request_failure_still_releases_permit(self):
        server = self.server()
        request = object()
        server.finish_request = mock.Mock()
        server.shutdown_request.side_effect = OSError
        server._permits.acquire()
        server._connections.add(request)
        with self.assertRaises(OSError):
            server._worker(request, None)
        self.assertTrue(server._permits.acquire(blocking=False))
        self.assertFalse(server._connections)

    def test_one_worker_and_one_accept_wait_then_success(self):
        server = self.server(timeout=1)
        entered, release, second_entered = threading.Event(), threading.Event(), threading.Event()
        first, second = object(), object()
        def finish(request, _address):
            if request is first:
                entered.set()
                self.assertTrue(release.wait(3))
            else:
                second_entered.set()
        server.finish_request = finish
        server.process_request(first, None)
        self.assertTrue(entered.wait(3))
        accept = threading.Thread(target=server.process_request, args=(second, None))
        accept.start()
        self.assertFalse(second_entered.wait(0.02))
        self.assertEqual(len(server._workers), 1)
        self.assertEqual(len(server._connections), 1)
        release.set()
        accept.join(3)
        self.assertTrue(second_entered.wait(3))
        wait_for(lambda: not server._workers)
        self.assertTrue(server._permits.acquire(blocking=False))

    def test_full_permit_timeout_and_shutdown_end_accept_wait(self):
        for stop in (False, True):
            server = self.server(timeout=0.03)
            server._permits.acquire()
            request = object()
            started = time.monotonic()
            if stop:
                server._stopping.set()
            server.process_request(request, None)
            if not stop:
                self.assertGreaterEqual(time.monotonic() - started, 0.02)
            server.shutdown_request.assert_called_once_with(request)
            self.assertEqual(server._workers, set())
            server._permits.release()
            if stop:
                server.process_request(object(), None)
                self.assertEqual(server._workers, set())
                self.assertTrue(server._permits.acquire(blocking=False))

    def test_configuration_rejects_extreme_timeouts_and_worker_limits_without_socket(self):
        owner = fixtures.make_owner()
        for timeout in (True, 0, -1, float('nan'), float('inf'), 1e308, 10**1000, threading.TIMEOUT_MAX * 2):
            with self.subTest(timeout=str(timeout)[:20]), self.assertRaises(ipc.ServerError):
                ipc.Server(owner, '/tmp/unused.sock', TOKEN, request_timeout=timeout, max_concurrent_requests=1)
        for count in (True, 0, -1, 1.0, sys.maxsize + 1):
            with self.assertRaises(ipc.ServerError):
                ipc.Server(owner, '/tmp/unused.sock', TOKEN, request_timeout=1, max_concurrent_requests=count)
        owner.close()


class SubjectServerLifecycleTest(unittest.TestCase):
    def test_private_path_checks_before_opening_socket(self):
        owner = fixtures.make_owner()
        self.addCleanup(owner.close)
        valid = SimpleNamespace(st_mode=stat.S_IFDIR | 0o700, st_uid=os.geteuid())
        invalid = [SimpleNamespace(st_mode=stat.S_IFLNK | 0o700, st_uid=os.geteuid()),
                   SimpleNamespace(st_mode=stat.S_IFDIR | 0o722, st_uid=os.geteuid()),
                   SimpleNamespace(st_mode=stat.S_IFDIR | 0o700, st_uid=os.geteuid() + 1)]
        with mock.patch.object(ipc, "_UnixServer") as listener:
            for mode in invalid:
                with mock.patch.object(ipc.os, "lstat", return_value=mode), self.assertRaises(ipc.ServerError):
                    ipc.Server(owner, "/tmp/private/sock", TOKEN, request_timeout=1, max_concurrent_requests=1)
            with mock.patch.object(ipc.os, "lstat", return_value=valid), \
                    mock.patch.object(ipc.os.path, "lexists", return_value=True), self.assertRaises(ipc.ServerError):
                ipc.Server(owner, "/tmp/private/sock", TOKEN, request_timeout=1, max_concurrent_requests=1)
            listener.assert_not_called()
        for path, token in (("relative.sock", TOKEN), ("/tmp/private/sock", "short")):
            with self.assertRaises(ipc.ServerError):
                ipc.Server(owner, path, token, request_timeout=1, max_concurrent_requests=1)

    def test_close_unlinks_socket_even_when_owner_fails_and_retries_cleanup(self):
        owner = fixtures.make_owner()
        self.addCleanup(owner.close)
        owner.lifecycle("register", fixtures.command(1))
        underlying = owner._entries[A.subject_key].receiver
        server = object.__new__(ipc.Server)
        server._owner, server._closed, server._thread = owner, False, None
        server._lock = threading.Lock()
        server._path_cleaned = False
        server._path, server._socket_identity = "/tmp/private/sock", (1, 2)
        server._server = SimpleNamespace(_stopping=threading.Event(), server_close=mock.Mock(), stop_workers=mock.Mock())
        metadata = SimpleNamespace(st_dev=1, st_ino=2)
        with mock.patch.object(ipc.os, "lstat", return_value=metadata), \
                mock.patch.object(ipc.os, "unlink") as unlink, \
                mock.patch.object(underlying, "close", side_effect=RuntimeError("secret")):
            with self.assertRaises(ipc.ServerError):
                server.close()
            unlink.assert_called_once_with(server._path)
            with self.assertRaises(ipc.ServerError):
                server.close()
            self.assertEqual(len(owner._entries), 1)
        server.close()
        server.close()
        self.assertEqual(owner._entries, {})
        server._server.server_close.assert_called_once()
        server._server.stop_workers.assert_called_once()

    def test_path_cleanup_retries_errors_without_deleting_replacement(self):
        for operation in ("lstat", "unlink"):
            for replaced in (False, True):
                with self.subTest(operation=operation, replaced=replaced):
                    server = object.__new__(ipc.Server)
                    server._owner = mock.Mock()
                    server._closed, server._path_cleaned, server._thread = False, False, None
                    server._lock = threading.Lock()
                    server._path, server._socket_identity = "/tmp/private/sock", (1, 2)
                    server._server = SimpleNamespace(_stopping=threading.Event(), server_close=mock.Mock(), stop_workers=mock.Mock())
                    original = SimpleNamespace(st_dev=1, st_ino=2)
                    replacement = SimpleNamespace(st_dev=1, st_ino=3) if replaced else original
                    with mock.patch.object(ipc.os, "lstat") as lstat, mock.patch.object(ipc.os, "unlink") as unlink:
                        lstat.side_effect = [OSError("first failure"), replacement] if operation == "lstat" else [original, replacement]
                        unlink.side_effect = [OSError("first failure"), None] if operation == "unlink" else None
                        with self.assertRaises(ipc.ServerError):
                            server.close()
                        server.close()
                        calls = unlink.call_count
                        self.assertEqual(calls, (operation == "unlink") + (not replaced))
                        server.close()
                        self.assertEqual(unlink.call_count, calls)
                        self.assertEqual(lstat.call_count, 2)
                    server._server.server_close.assert_called_once()
                    server._server.stop_workers.assert_called_once()

    def test_concurrent_close_interrupts_saturated_accept_before_joining_workers(self):
        listener = SubjectWorkerTest().server(timeout=threading.TIMEOUT_MAX)
        released, entered = threading.Event(), threading.Event()
        connection = mock.Mock()
        connection.shutdown.side_effect = lambda _how: released.set()
        listener.finish_request = lambda *_args: released.wait(3)
        listener.process_request(connection, None)
        original = listener._permits.acquire
        def acquire(*args, **kwargs):
            entered.set()
            return original(*args, **kwargs)
        listener._permits.acquire = acquire
        accept = threading.Thread(target=listener.process_request, args=(object(), None), daemon=True)
        accept.start()
        self.assertTrue(entered.wait(1))
        listener.shutdown = lambda: accept.join(2)
        listener.server_close = mock.Mock()
        server = object.__new__(ipc.Server)
        server._server, server._owner, server._thread = listener, mock.Mock(), accept
        server._closed, server._path_cleaned = False, False
        server._lock = threading.Lock()
        server._path, server._socket_identity = "/tmp/private/sock", (1, 2)
        errors = []
        def close():
            try:
                server.close()
            except Exception as exc:
                errors.append(exc)
        with mock.patch.object(ipc.os, "lstat", side_effect=FileNotFoundError):
            closers = [threading.Thread(target=close, daemon=True) for _ in range(2)]
            for thread in closers:
                thread.start()
            for thread in closers:
                thread.join(1)
                self.assertFalse(thread.is_alive())
        self.assertFalse(errors)
        self.assertFalse(accept.is_alive())
        self.assertTrue(released.is_set())
        self.assertFalse(listener._workers)
        self.assertFalse(listener._connections)
        listener.server_close.assert_called_once()
        self.assertTrue(original(blocking=False))
        self.assertFalse(original(blocking=False))


class SubjectSocketTest(unittest.TestCase):
    """Mandatory real AF_UNIX HTTP coverage; capability errors must fail CI."""

    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix='ossubj-', dir='/tmp')
        self.addCleanup(self.directory.cleanup)
        self.path = os.path.join(self.directory.name, 'revision.sock')
        self.owner = fixtures.make_owner()
        self.addCleanup(self.owner.close)
        self.server = ipc.Server(self.owner, self.path, TOKEN, request_timeout=0.25, max_concurrent_requests=2)
        self.addCleanup(self.server.close)
        self.server.start()

    def request(self, path, value=None, *, raw=None, token=TOKEN):
        connection = UnixConnection(self.path)
        raw = json.dumps(value).encode() if raw is None else raw
        try:
            connection.request('POST', path, raw, {'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'})
        except (BrokenPipeError, ConnectionResetError):
            pass
        try:
            response = connection.getresponse()
            return response.status, json.loads(response.read())
        finally:
            connection.close()

    def test_real_independent_install_and_private_socket(self):
        self.assertEqual(stat.S_IMODE(os.stat(self.path).st_mode), 0o600)
        for sequence, target in ((1, A), (2, B)):
            self.assertEqual(self.request('/v2/subjects/register', raw=fixtures.command(sequence, target))[0], 200)
            value, item = prepare(target)
            self.assertEqual(self.request('/v2/revisions/prepare', value)[0], 200)
            del value['payload']
            self.assertEqual(self.request('/v2/revisions/commit', value), (200, {'revision': value['revision']}))
            self.assertEqual(self.owner.acquire(target), item)
        self.assertEqual(self.request('/v2/subjects/register', raw=fixtures.command(3), token='x' * 32), (401, {'error': 'unauthorized'}))
        self.assertEqual(self.owner.lifecycle_status(CONTROL)['lastSequence'], 2)
        self.server.close()
        self.assertFalse(os.path.lexists(self.path))

    def test_real_high_timeout_saturated_concurrent_close(self):
        self.server._server.request_timeout = 3600
        waiting = threading.Event()
        original = self.server._server._permits.acquire
        def acquire(*args, **kwargs):
            if len(self.server._server._workers) == 2:
                waiting.set()
            return original(*args, **kwargs)
        self.server._server._permits.acquire = acquire
        connections = []
        for _ in range(3):
            connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            self.addCleanup(connection.close)
            connections.append(connection)
            connection.connect(self.path)
            connection.sendall(b'POST /v2/subjects/register HTTP/1.1\r\n')
        self.assertTrue(waiting.wait(2))
        errors = []
        def close():
            try:
                self.server.close()
            except Exception as exc:
                errors.append(exc)
        closers = [threading.Thread(target=close, daemon=True) for _ in range(2)]
        for thread in closers:
            thread.start()
        try:
            for thread in closers:
                thread.join(2)
            stuck = any(thread.is_alive() for thread in closers)
        finally:
            # A shutdown regression must fail promptly, not leave cleanup
            # waiting for the deliberately long connection timeout.
            for connection in connections:
                try:
                    connection.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
            for thread in closers:
                thread.join(2)
        self.assertFalse(stuck)
        self.assertFalse(errors)
        self.assertFalse(os.path.lexists(self.path))
        self.assertFalse(self.server._server._workers)
        self.assertFalse(self.server._server._connections)

    def test_real_reply_loss_recovers_published_record(self):
        original = ipc._Handler._reply
        lost = threading.Event()
        def reply(handler, status, value):
            if handler.path == '/v2/subjects/register' and not lost.is_set():
                lost.set()
                raise BrokenPipeError
            return original(handler, status, value)
        raw = fixtures.command(1)
        with mock.patch.object(ipc._Handler, '_reply', reply):
            with self.assertRaises((OSError, http.client.HTTPException)):
                self.request('/v2/subjects/register', raw=raw)
        status = self.request('/v2/subjects/lifecycle-status', {'controlGeneration': CONTROL})
        self.assertEqual(status[1]['lastSequence'], 1)
        self.assertEqual(status[1]['commandDigest'], subject.command_digest('register', raw))
        self.assertEqual(self.request('/v2/subjects/register', raw=raw)[0], 200)

    def test_real_saturation_waits_and_shutdown_closes_workers(self):
        held = []
        for _ in range(2):
            connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            self.addCleanup(connection.close)
            connection.connect(self.path)
            connection.sendall(b'POST /v2/subjects/register HTTP/1.1\r\n')
            held.append(connection)
        wait_for(lambda: len(self.server._server._workers) == 2)
        result = []
        worker = threading.Thread(target=lambda: result.append(self.request('/v2/subjects/register', raw=fixtures.command(1))))
        worker.start()
        time.sleep(0.02)
        self.assertEqual(len(self.server._server._workers), 2)
        held[0].close()
        worker.join(3)
        self.assertEqual(result[0][0], 200)
        self.server.close()
        self.assertFalse(self.server._server._workers)
        self.assertFalse(self.server._server._connections)


if __name__ == '__main__':
    unittest.main()
