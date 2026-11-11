# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Socket-free H2/manual-peer stress tests; no TLS, real IPC or client interoperability claim.

The companion opt-in runner's ManualH2Peer records and withholds GOAWAY from
hyper-h2 state processing. Its frame timing is useful for late-frame stress,
not proof that a normal client received GOAWAY or migrated correctly.
"""

import sys
import unittest
from collections import deque
from dataclasses import FrozenInstanceError
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

try:
    import h2.config
    import h2.connection
    import h2.errors
    import h2.events
    import h2.settings
    from hyperframe.frame import (
        ContinuationFrame, DataFrame, Frame, GoAwayFrame, HeadersFrame,
        RstStreamFrame, SettingsFrame, WindowUpdateFrame,
    )
    from mitmproxy import connection, http
    from mitmproxy.proxy import commands, events
    from mitmproxy.proxy.context import Context
    from mitmproxy.proxy.layers import http as layers
    from mitmproxy.proxy.layers.http._http2 import Http2Server
except ImportError as exc:
    raise unittest.SkipTest(
        "H2 layer tests require pinned mitmproxy dependencies"
    ) from exc

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "mitmscripts"))
from credential_bound_h2 import (
    BoundH2Server,
    H2IdentityError,
    dependency_versions_supported,
)


class Runtime:
    """Layer boundary stand-in; terminal requires a simulated transport event."""

    def __init__(self, client, capacity=64):
        self.client = client
        self.token = object()
        self.state = SimpleNamespace(
            fenced=False,
            frozen_last_stream_id=None,
            deadline=None,
            max_admitted_stream_id=0,
        )
        self.registry = SimpleNamespace(
            request_capacity=capacity, drain_state=lambda token: self.state
        )
        self.finished = []
        self.terminal = []
        self.prepared = []
        self.consumed = []
        self.pruned = 0

    def filter_h2_wakeups(self, adapter):
        pass  # Pure layer harness drives its own Wakeup events.

    def h2_transport_known_terminal(self, *args):
        return self.h2_transport_terminal(*args)

    def token_for(self, client):
        return self.token if client is self.client else None

    def finish_request(self, handle):
        self.finished.append(handle)

    def prune_h2_transports(self, adapter):
        self.pruned += 1

    def h2_transport_terminal(self, adapter, conn, owner):
        return any(c is conn for c in self.terminal)

    def prepare_h2_close(self, adapter, conn, owner):
        self.assert_owner(adapter, conn, owner)
        item = (conn, owner)
        self.prepared.append(item)
        return item

    def h2_close_consumed(self, observation):
        self.consumed.append(observation)

    @staticmethod
    def assert_owner(adapter, conn, owner):
        assert any(c is conn and o is owner for c, o in adapter.owned_upstreams())


class Harness:
    def __init__(
        self,
        *,
        deny=False,
        capacity=64,
        hold=(),
        body_limit=None,
        upstream="h2",
        connected=True,
        stream_request=True,
        initial_window=65535,
        max_streams=100,
        downstream_window=65535,
        local_response=None,
    ):
        self.client = connection.Client(
            peername=("127.0.0.1", 1),
            sockname=("127.0.0.1", 443),
            state=connection.ConnectionState.OPEN,
        )
        self.client.alpn = b"h2"
        self.client.tls = True
        self.client.sni = "example.test"
        options = SimpleNamespace(
            proxy_debug=False,
            validate_inbound_headers=True,
            keep_host_header=False,
            body_size_limit=body_limit,
            stream_large_bodies=None,
            normalize_outbound_headers=True,
            connection_strategy="lazy",
            http2=True,
            http3=False,
            rawtcp=False,
            websocket=False,
            http2_ping_keepalive=0,
        )
        self.context = Context(self.client, options)
        self.context.server.address = ("203.0.113.42", 443)
        self.context.server.tls = True
        self.context.server.alpn = b"h2" if upstream == "h2" and connected else None
        self.context.server.state = (
            connection.ConnectionState.OPEN
            if connected
            else connection.ConnectionState.CLOSED
        )
        self.root = layers.HttpLayer(self.context, layers.HTTPMode.transparent)
        self.runtime = Runtime(self.client, capacity)
        self.adapter = BoundH2Server(
            self.context.fork(), self.runtime, self.root, self.runtime.token
        )
        self.root.connections[self.client] = self.adapter
        self.peer = h2.connection.H2Connection(
            h2.config.H2Configuration(client_side=True, validate_outbound_headers=False)
        )
        self.peer.local_settings.initial_window_size = downstream_window
        self.peer.local_settings.acknowledge()
        self.peer.initiate_connection()
        self.origin = h2.connection.H2Connection(
            h2.config.H2Configuration(client_side=False)
        )
        self.origin.local_settings.initial_window_size = initial_window
        self.origin.local_settings.max_concurrent_streams = max_streams
        self.origin.local_settings.acknowledge()
        self.origin.initiate_connection()
        self.upstream = upstream
        self.upstream_conn = None
        self.deny = deny
        self.stream_request = stream_request
        self.local_response = local_response
        self.hold = set(hold)
        self.held = {}
        self.flows = {}
        self.identities = []
        self.output = []
        self.peer_events = []
        self.client_wire = bytearray()
        self.parse_client_wire = True
        self.origin_events = []
        self.origin_bytes = bytearray()
        self.opens = []
        self.run(events.Start())
        self.flush()
        self.flush()

    def admission(self, flow):
        identity = self.adapter.consume_identity(flow.request)
        if identity is None:
            if not self.adapter.request_is_closed(flow.request):
                self.adapter.fail_closed()
            flow.kill()
            return
        self.identities.append(identity)
        if (
            self.deny
            or self.runtime.state.fenced
            or len(self.adapter._active) >= self.runtime.registry.request_capacity
        ):
            self.adapter.refuse(identity, flow)
        else:
            handle = SimpleNamespace(client_stream_id=identity.stream_id)
            self.runtime.state.max_admitted_stream_id = max(
                self.runtime.state.max_admitted_stream_id, identity.stream_id
            )
            self.adapter.bind_request(identity, flow, handle)
            flow.metadata["opensandbox_credential_request_handle"] = handle
            flow.request.stream = self.stream_request
            if self.local_response is not None:
                flow.response = http.Response.make(200, self.local_response)

    def run(self, event):
        queue = deque([event])
        while queue:
            ev = queue.popleft()
            for command in self.root.handle_event(ev):
                self.output.append(command)
                if isinstance(command, commands.StartHook):
                    flow = command.args()[0]
                    sid = int(flow.request.path.rsplit("/", 1)[-1])
                    self.flows[sid] = flow
                    key = (command.name, sid)
                    if key in self.hold:
                        self.held[key] = command
                    else:
                        self.hook(command)
                        queue.append(events.HookCompleted(command))
                elif isinstance(command, commands.OpenConnection):
                    self.opens.append(command)
                elif isinstance(command, commands.SendData):
                    if command.connection is self.client:
                        self.client_wire.extend(command.data)
                        if self.parse_client_wire:
                            self.peer_events.extend(self.peer.receive_data(command.data))
                    else:
                        self.upstream_conn = command.connection
                        self.origin_bytes.extend(command.data)
                        if self.upstream == "h2":
                            self.origin_events.extend(
                                self.origin.receive_data(command.data)
                            )
                elif isinstance(
                    command,
                    (commands.CloseConnection, commands.RequestWakeup, commands.Log),
                ):
                    pass
                else:
                    raise TypeError(f"Unexpected outer command {command!r}")

    def hook(self, command):
        flow = command.args()[0]
        if isinstance(command, layers.HttpRequestHeadersHook):
            self.admission(flow)
        elif isinstance(command, (layers.HttpResponseHook, layers.HttpErrorHook)):
            self.adapter.application_done(flow)

    def release(self, key):
        command = self.held.pop(key)
        self.hold.discard(key)
        self.hook(command)
        self.run(events.HookCompleted(command))

    def flush(self):
        data = self.peer.data_to_send()
        if data:
            self.run(events.DataReceived(self.client, data))

    @staticmethod
    def decode_frames(data):
        """Decode synthetic wire bytes with pinned hyperframe."""
        frames = []
        offset = 0
        while offset < len(data):
            header = memoryview(data[offset : offset + 9])
            frame, body_length = Frame.parse_frame_header(header)
            end = offset + 9 + body_length
            if end > len(data):
                raise AssertionError("truncated synthetic server frame")
            frame.parse_body(memoryview(data[offset + 9 : end]))
            frames.append(frame)
            offset = end
        return frames

    def client_frames(self):
        return self.decode_frames(self.client_wire)

    def tick(self):
        wake = self.adapter._wakeup
        if wake is not None:
            self.run(events.Wakeup(wake))

    def headers(
        self,
        sid=1,
        *,
        end=False,
        scheme="https",
        authority="example.test",
        extra=(),
        flush=True,
    ):
        self.peer.send_headers(
            sid,
            [
                (":method", "POST"),
                (":scheme", scheme),
                (":authority", authority),
                (":path", f"/{sid}"),
                *extra,
            ],
            end_stream=end,
        )
        if flush:
            self.flush()

    def data(self, sid, data, *, end=False):
        self.peer.send_data(sid, data, end_stream=end)
        self.flush()

    def reset(self, sid):
        self.peer.reset_stream(sid, error_code=8)
        self.flush()

    def response(self, ours=1, *, body=b"", trailers=False):
        if self.upstream == "h1":
            payload = (
                b"HTTP/1.1 200 OK\r\nContent-Length: "
                + str(len(body)).encode()
                + b"\r\n\r\n"
                + body
            )
            self.run(events.DataReceived(self.upstream_conn, payload))
        else:
            self.origin.send_headers(
                ours, [(":status", "200")], end_stream=not body and not trailers
            )
            if body:
                self.origin.send_data(ours, body, end_stream=not trailers)
            if trailers:
                self.origin.send_headers(ours, [("x-trailer", "done")], end_stream=True)
            self.from_origin()

    def from_origin(self):
        self.run(events.DataReceived(self.upstream_conn, self.origin.data_to_send()))

    def fence(self, last=None, deadline=None):
        self.runtime.state.fenced = True
        self.runtime.state.frozen_last_stream_id = (
            self.runtime.state.max_admitted_stream_id if last is None else last
        )
        self.runtime.state.deadline = deadline

    def close_event(self, conn):
        conn.state = connection.ConnectionState.CLOSED
        self.runtime.terminal.append(conn)
        self.run(events.ConnectionClosed(conn))

    def assert_no_origin(self, case):
        case.assertFalse(self.opens)
        case.assertFalse(self.origin_bytes)

    def assert_identity_empty(self, case):
        case.assertFalse(self.adapter._pending)
        case.assertFalse(self.adapter._claimed)
        case.assertFalse(self.adapter._refused)


@unittest.skipUnless(
    dependency_versions_supported(),
    "requires mitmproxy 11.0.2 / h2 4.1.0 / hyperframe 6.0.1",
)
class SocketFreeManualPeerStressTests(unittest.TestCase):
    def test_instance_identity_is_raw_exact_and_one_shot(self):
        h = Harness(deny=True)
        h.headers(
            end=True,
            scheme="http",
            authority="example.test:81",
            extra=[("host", "example.test:81")],
        )
        identity = h.identities[0]
        self.assertIs(h.root.connections[h.client], h.adapter)
        self.assertIsNot(type(h.adapter), Http2Server)
        self.assertIs(identity.request, h.flows[1].request)
        self.assertIn((b":scheme", b"http"), identity.headers)
        self.assertIn((b":authority", b"example.test:81"), identity.headers)
        self.assertEqual(
            (identity.request.scheme, identity.request.host, identity.request.port),
            ("https", "203.0.113.42", 443),
        )
        self.assertIsNone(h.adapter.consume_identity(identity.request))
        with self.assertRaises(FrozenInstanceError):
            identity.stream_id = 3
        h.assert_no_origin(self)

    def test_repeated_refusal_cleans_three_tables_without_history(self):
        h = Harness(deny=True)
        for sid in range(1, 501, 2):
            h.headers(sid, end=True)
            self.assertFalse(h.root.streams)
            self.assertFalse(h.adapter.streams)
            self.assertFalse(h.adapter._active)
            h.assert_identity_empty(self)
            self.assertEqual(len(h.root.command_sources), 1)  # one bounded wakeup
        resets = [
            (x.stream_id, int(x.error_code))
            for x in h.peer_events
            if isinstance(x, h2.events.StreamReset)
        ]
        self.assertEqual(resets, [(sid, 7) for sid in range(1, 501, 2)])
        h.assert_no_origin(self)

    def test_marked_400_and_413_are_abnormal_without_http_or_fake_refusal(self):
        for kw, request in [
            ({}, {"scheme": "ftp"}),
            ({"body_limit": "1"}, {"extra": [("content-length", "100")]}),
        ]:
            with self.subTest(kw=kw):
                h = Harness(deny=True, **kw)
                h.headers(**request)
                self.assertTrue(h.adapter.failed)
                self.assertFalse(h.adapter.goaway_sent)
                self.assertTrue(
                    any(isinstance(x, commands.CloseConnection) for x in h.output)
                )
                self.assertFalse(
                    [
                        x
                        for x in h.peer_events
                        if isinstance(
                            x, (h2.events.StreamReset, h2.events.ResponseReceived)
                        )
                    ]
                )
                self.assertFalse(h.root.streams)
                self.assertFalse(h.adapter.streams)
                h.assert_identity_empty(self)
                h.assert_no_origin(self)

    def test_pending_hook_not_admitted_and_overflow_fails_closed(self):
        h = Harness(deny=True, hold={("requestheaders", 1)}, capacity=1)
        h.headers(1)
        self.assertEqual(len(h.adapter._pending), 1)
        self.assertFalse(h.identities)
        h.headers(3)
        self.assertTrue(h.adapter.failed)
        self.assertFalse(
            [x for x in h.peer_events if isinstance(x, h2.events.StreamReset)]
        )
        h.assert_identity_empty(self)
        h.assert_no_origin(self)

    def test_socket_free_stub_paused_stream_is_refused_after_fence_not_ac07_acceptance(self):
        h = Harness(hold={("requestheaders", 1)})
        h.headers(1)
        h.headers(3, end=True)
        self.assertEqual(list(h.adapter._active), [3])
        self.assertEqual(list(h.adapter._pending.values())[0].stream_id, 1)

        h.fence(last=3)
        h.tick()
        self.assertEqual(
            [event.last_stream_id for event in h.peer_events
             if isinstance(event, h2.events.ConnectionTerminated)],
            [3],
        )
        h.parse_client_wire = False
        h.release(("requestheaders", 1))

        self.assertEqual(list(h.adapter._active), [3])
        self.assertNotIn(1, h.adapter._claimed)
        self.assertNotIn(1, h.adapter._pending)
        self.assertNotIn(1, h.adapter._refused)
        self.assertFalse(h.adapter.failed)
        origin_requests = [
            event for event in h.origin_events
            if isinstance(event, h2.events.RequestReceived)
        ]
        self.assertEqual(len(origin_requests), 1)
        self.assertEqual(dict(origin_requests[0].headers)[b":path"], b"/3")
        resets = [
            frame for frame in h.client_frames()
            if isinstance(frame, RstStreamFrame) and frame.stream_id == 1
        ]
        self.assertEqual(len(resets), 1)
        self.assertEqual(resets[0].error_code, h2.errors.ErrorCodes.REFUSED_STREAM)

    def test_active_capacity_exhaustion_refuses_new_stream_preserving_old(self):
        h = Harness(capacity=1)
        h.headers(1)
        h.from_origin()
        h.headers(3, end=True)
        self.assertFalse(h.adapter.failed)
        self.assertEqual(list(h.adapter._active), [1])
        self.assertFalse(h.runtime.finished)
        self.assertEqual(
            [
                (x.stream_id, int(x.error_code))
                for x in h.peer_events
                if isinstance(x, h2.events.StreamReset)
            ],
            [(3, 7)],
        )
        self.assertEqual(
            [
                x.stream_id
                for x in h.origin_events
                if isinstance(x, h2.events.RequestReceived)
            ],
            [1],
        )
        h.data(1, b"done", end=True)
        h.response()
        h.tick()
        self.assertEqual(len(h.runtime.finished), 1)

    def test_shutdown_fence_without_frozen_bound_abnormally_closes(self):
        h = Harness()
        h.runtime.state.fenced = True
        h.tick()
        self.assertTrue(h.adapter.failed)
        self.assertFalse(h.adapter.goaway_sent)
        self.assertTrue(h.adapter.close_started)

    def test_fragmented_headers_create_identity_only_after_complete_block(self):
        h = Harness(deny=True)
        h.peer.max_outbound_frame_size = 16384
        value = "".join(chr(33 + (index * 17) % 90) for index in range(45000))
        h.headers(extra=[("x-long", value)], end=True, flush=False)
        payload = h.peer.data_to_send()
        first_length = int.from_bytes(payload[:3], "big") + 9
        self.assertLess(first_length, len(payload))
        self.assertEqual(payload[first_length + 3], 9)  # CONTINUATION
        h.run(events.DataReceived(h.client, payload[:first_length]))
        self.assertFalse(h.flows)
        self.assertFalse(h.adapter._pending)
        for offset in range(first_length, len(payload), 137):
            h.run(events.DataReceived(h.client, payload[offset : offset + 137]))
        self.assertEqual(len(h.identities), 1)
        self.assertEqual(h.identities[0].stream_id, 1)
        self.assertEqual(
            [
                (x.stream_id, int(x.error_code))
                for x in h.peer_events
                if isinstance(x, h2.events.StreamReset)
            ],
            [(1, 7)],
        )
        h.assert_identity_empty(self)
        h.assert_no_origin(self)

    def test_same_batch_data_reset_keeps_flow_control_and_no_origin(self):
        h = Harness(deny=True)
        window = h.adapter.h2_conn._inbound_flow_control_window_manager
        before = window._bytes_processed
        h.headers(flush=False)
        h.peer.send_data(1, b"hello", pad_length=7)
        h.peer.reset_stream(1)
        h.flush()
        self.assertEqual(window._bytes_processed - before, 13)
        self.assertFalse(h.root.streams)
        self.assertFalse(h.adapter.streams)
        h.assert_identity_empty(self)
        h.assert_no_origin(self)

    def test_paused_hook_reset_is_terminal_without_admission(self):
        h = Harness(hold={("requestheaders", 1)})
        h.headers()
        h.reset(1)
        self.assertFalse(h.adapter._pending)
        h.release(("requestheaders", 1))
        self.assertFalse(h.identities)
        self.assertFalse(h.adapter.failed)
        self.assertFalse(h.root.streams)
        h.assert_no_origin(self)

    def test_pre_hook_parse_error_preserves_stop_value(self):
        h = Harness()
        h.headers(1, authority="bad.example:invalid", flush=False)
        h.headers(3, flush=False)
        h.flush()
        self.assertTrue(h.adapter.failed)
        self.assertFalse(h.flows)
        h.assert_identity_empty(self)
        h.assert_no_origin(self)

    def test_h2_mapping_reorders_and_max_concurrency_queue_stays_owned(self):
        h = Harness(hold={("requestheaders", 1)})
        h.headers(1)
        h.headers(3)
        h.from_origin()
        h.release(("requestheaders", 1))
        self.assertEqual(h.adapter._association(h.adapter._active[3])[-1], 1)
        self.assertEqual(h.adapter._association(h.adapter._active[1])[-1], 3)
        self.assertEqual(len(h.adapter.owned_upstreams()), 1)
        q = Harness(max_streams=1)
        q.headers(1)
        q.from_origin()
        q.headers(3)
        q.data(3, b"queued")
        association = q.adapter._association(q.adapter._active[3])
        self.assertEqual(association[3], "queued")
        self.assertFalse(q.runtime.finished)

    def test_pending_command_beats_stale_flow_server_connection(self):
        h = Harness(connected=False)
        h.headers()
        record = h.adapter._active[1]
        association = h.adapter._association(record)
        self.assertEqual(association[3], "connecting")
        self.assertIsNot(association[0], h.flows[1].server_conn)
        self.assertIs(association[0], h.opens[0].connection)
        self.assertEqual(
            h.adapter.owned_upstreams(), ((association[0], association[1]),)
        )

    def test_manual_peer_late_goaway_stress_retains_handle_and_delays_close(self):
        h = Harness(downstream_window=0)
        h.headers(end=True)
        h.from_origin()
        h.response(body=b"payload")
        state = h.adapter.h2_conn.state_machine.state
        h.fence()
        h.tick()
        self.assertFalse(h.runtime.finished)
        self.assertTrue(h.adapter.h2_conn.stream_buffers[1])
        self.assertFalse(h.adapter.goaway_sent)
        early = [
            event for event in h.peer_events
            if isinstance(event, h2.events.ConnectionTerminated)
        ]
        self.assertEqual([(event.last_stream_id, event.error_code) for event in early], [(1, 0)])
        self.assertNotIn(
            1,
            [event.stream_id for event in h.peer_events
             if isinstance(event, h2.events.StreamEnded)],
        )
        self.assertIs(h.adapter.h2_conn.state_machine.state, state)
        handle = h.flows[1].metadata["opensandbox_credential_request_handle"]
        h.parse_client_wire = False
        window_update = WindowUpdateFrame(stream_id=1)
        window_update.window_increment = 100
        h.run(events.DataReceived(h.client, window_update.serialize()))
        h.tick()
        self.assertEqual(h.runtime.finished, [handle])
        self.assertNotIn("opensandbox_credential_request_handle", h.flows[1].metadata)
        self.assertTrue(h.adapter.goaway_sent)
        frames = h.client_frames()
        goaways = [
            (index, frame) for index, frame in enumerate(frames)
            if isinstance(frame, GoAwayFrame)
        ]
        self.assertEqual(
            [(frame.last_stream_id, frame.error_code) for _, frame in goaways],
            [(1, 0), (1, 0)],
        )
        early_index, final_index = goaways[0][0], goaways[1][0]
        body_frames = [
            (index, frame) for index, frame in enumerate(frames)
            if isinstance(frame, DataFrame) and frame.stream_id == 1
        ]
        self.assertEqual(
            b"".join(frame.data for _, frame in body_frames), b"payload"
        )
        self.assertIn("END_STREAM", body_frames[-1][1].flags)
        self.assertLess(early_index, body_frames[0][0])
        self.assertLess(body_frames[-1][0], final_index)
        self.assertFalse(h.adapter.failed)

    def test_goaway_serialization_clamps_all_paths_and_preserves_instance_scope(self):
        for path in ("close", "terminate", "protocol_error"):
            with self.subTest(path=path):
                h = Harness()
                h.headers(end=True)
                h.runtime.state.fenced = True
                h.runtime.state.frozen_last_stream_id = 0
                connection_class_method = type(h.adapter.h2_conn)._prepare_for_sending

                if path == "close":
                    h.adapter.h2_conn.close_connection(
                        error_code=h2.errors.ErrorCodes.NO_ERROR,
                        last_stream_id=9,
                    )
                    data = h.adapter.h2_conn.data_to_send()
                    expected_error = h2.errors.ErrorCodes.NO_ERROR
                elif path == "terminate":
                    h.adapter.h2_conn._terminate_connection(
                        h2.errors.ErrorCodes.PROTOCOL_ERROR
                    )
                    data = h.adapter.h2_conn.data_to_send()
                    expected_error = h2.errors.ErrorCodes.PROTOCOL_ERROR
                else:
                    commands_out = list(h.adapter.protocol_error(
                        "synthetic protocol error",
                        h2.errors.ErrorCodes.PROTOCOL_ERROR,
                    ))
                    data = next(
                        command.data for command in commands_out
                        if isinstance(command, commands.SendData)
                    )
                    expected_error = h2.errors.ErrorCodes.PROTOCOL_ERROR

                events = h.peer.receive_data(data)
                goaway = next(
                    event for event in events
                    if isinstance(event, h2.events.ConnectionTerminated)
                )
                self.assertEqual(goaway.last_stream_id, 0)
                self.assertEqual(goaway.error_code, expected_error)
                self.assertEqual(h.adapter.h2_conn.highest_inbound_stream_id, 1)
                self.assertIs(type(h.adapter.h2_conn)._prepare_for_sending, connection_class_method)
                self.assertNotIn("_prepare_for_sending", type(h.adapter.h2_conn).__dict__)

    def test_serialized_goaway_bound_never_increases(self):
        h = Harness()
        h.runtime.state.fenced = True
        h.runtime.state.frozen_last_stream_id = 5
        h.adapter.h2_conn._prepare_for_sending([
            GoAwayFrame(0, last_stream_id=2, error_code=h2.errors.ErrorCodes.NO_ERROR)
        ])
        h.adapter.h2_conn._prepare_for_sending([
            GoAwayFrame(0, last_stream_id=9, error_code=h2.errors.ErrorCodes.PROTOCOL_ERROR)
        ])
        events = h.peer.receive_data(h.adapter.h2_conn.data_to_send())
        goaways = [event for event in events if isinstance(event, h2.events.ConnectionTerminated)]
        self.assertEqual([event.last_stream_id for event in goaways], [2, 2])
        self.assertEqual(
            [event.error_code for event in goaways],
            [h2.errors.ErrorCodes.NO_ERROR, h2.errors.ErrorCodes.PROTOCOL_ERROR],
        )

    def _install_serializer_spy(self, adapter):
        conn = adapter.h2_conn
        del conn._prepare_for_sending
        class_prepare = type(conn)._prepare_for_sending
        original_calls = []

        def prepare_spy(instance, frames):
            original_calls.append(tuple(frames))
            return class_prepare(instance, frames)

        with patch.object(type(conn), "_prepare_for_sending", prepare_spy):
            adapter._install_retirement_serializer()
        return conn, original_calls

    def test_retirement_serializer_skips_registry_on_empty_and_ordinary_batches(self):
        h = Harness()
        h.runtime.state.fenced = True
        h.runtime.state.frozen_last_stream_id = 0
        # Count the original serializer and restore the class method afterward.
        conn, original_calls = self._install_serializer_spy(h.adapter)

        registry_reads = []
        h.runtime.registry.drain_state = lambda token: (
            registry_reads.append(token) or h.runtime.state
        )
        conn._prepare_for_sending([])
        conn._prepare_for_sending([SettingsFrame(0)])

        self.assertEqual(registry_reads, [])
        self.assertEqual(len(original_calls), 2)
        self.assertEqual(original_calls[0], ())
        self.assertEqual(len(original_calls[1]), 1)
        self.assertIsInstance(original_calls[1][0], SettingsFrame)

    def test_retirement_serializer_clamps_direct_mixed_goaways_and_is_instance_scoped(
        self,
    ):
        h = Harness()
        h.runtime.state.fenced = True
        h.runtime.state.frozen_last_stream_id = 5
        conn, original_calls = self._install_serializer_spy(h.adapter)

        registry_reads = []
        h.runtime.registry.drain_state = lambda token: (
            registry_reads.append(token) or h.runtime.state
        )
        # First GOAWAY is a direct error path. Multiple controls in one batch
        # and later batches retain the frozen and serialized minimum bounds.
        batch = [
            SettingsFrame(0),
            GoAwayFrame(
                0,
                last_stream_id=9,
                error_code=h2.errors.ErrorCodes.PROTOCOL_ERROR,
            ),
            GoAwayFrame(
                0,
                last_stream_id=3,
                error_code=h2.errors.ErrorCodes.NO_ERROR,
            ),
            GoAwayFrame(
                0,
                last_stream_id=8,
                error_code=h2.errors.ErrorCodes.PROTOCOL_ERROR,
            ),
        ]
        conn._prepare_for_sending(batch)
        self.assertEqual(
            [frame.last_stream_id for frame in batch[1:]], [5, 3, 3]
        )
        self.assertEqual(len(registry_reads), 1)
        self.assertEqual(len(original_calls), 1)

        # A missing registry state after first serialization cannot loosen the
        # already serialized bound while this concrete transport still lives.
        h.runtime.state = None
        next_batch = [
            GoAwayFrame(
                0,
                last_stream_id=12,
                error_code=h2.errors.ErrorCodes.PROTOCOL_ERROR,
            )
        ]
        conn._prepare_for_sending(next_batch)
        self.assertEqual(next_batch[0].last_stream_id, 3)
        self.assertEqual(len(registry_reads), 2)
        self.assertEqual(len(original_calls), 2)

        # The instance-installed wrapper does not affect a sibling connection.
        sibling = type(conn)(h2.config.H2Configuration(client_side=False))
        sibling_goaway = GoAwayFrame(
            0,
            last_stream_id=12,
            error_code=h2.errors.ErrorCodes.PROTOCOL_ERROR,
        )
        sibling._prepare_for_sending([sibling_goaway])
        self.assertEqual(sibling_goaway.last_stream_id, 12)
        self.assertEqual(len(registry_reads), 2)
        self.assertEqual(len(original_calls), 2)

    def test_h1_normal_response_releases_only_after_upload_finishes(self):
        h = Harness(upstream="h1")
        h.headers(extra=[("content-length", "3")])
        h.response(body=b"ok")
        h.tick()
        self.assertFalse(h.runtime.finished)
        self.assertFalse(h.adapter.failed)
        h.data(1, b"abc", end=True)
        h.tick()
        self.assertEqual(len(h.runtime.finished), 1)
        self.assertFalse(h.adapter._active)

    def test_early_response_then_reset_closes_owned_h1_and_h2_even_empty_buffers(self):
        for protocol, payload in [("h1", b""), ("h2", b""), ("h2", b"buffered")]:
            with self.subTest(protocol=protocol, payload=payload):
                h = Harness(upstream=protocol, initial_window=0)
                h.headers(extra=[("content-length", "8")])
                if protocol == "h2":
                    h.from_origin()
                if payload:
                    h.data(1, payload)
                h.response()
                h.reset(1)
                self.assertTrue(h.adapter.failed)
                self.assertFalse(h.adapter.goaway_sent)
                self.assertFalse(h.runtime.finished)
                self.assertEqual([c for c, _ in h.runtime.prepared], [h.upstream_conn])
                self.assertEqual(h.runtime.prepared, h.runtime.consumed)
                closes = [
                    x.connection
                    for x in h.output
                    if isinstance(x, commands.CloseConnection)
                ]
                self.assertIn(h.client, closes)
                self.assertIn(h.upstream_conn, closes)

    def test_stranded_downstream_trailers_respect_existing_deadline_only(self):
        for deadline in (None, 101.0):
            with (
                self.subTest(deadline=deadline),
                patch("credential_bound_h2.time.monotonic", return_value=100.0),
            ):
                h = Harness(downstream_window=0)
                h.headers(end=True)
                h.from_origin()
                h.response(body=b"body", trailers=True)
                h.runtime.state.deadline = deadline
                h.reset(1)
                self.assertIn(1, h.adapter.h2_conn.stream_trailers)
                self.assertEqual(h.adapter.failed, deadline is None)
                self.assertFalse(h.runtime.finished)
                if deadline is not None:
                    with patch(
                        "credential_bound_h2.time.monotonic", return_value=101.0
                    ):
                        h.tick()
                    self.assertTrue(h.adapter.failed)
                self.assertFalse(h.adapter.goaway_sent)
                self.assertIn(
                    1, h.adapter.h2_conn.stream_trailers
                )  # never manually wiped

    def test_rotation_deadline_closes_without_fence_or_goaway(self):
        with patch("credential_bound_h2.time.monotonic", return_value=100.0):
            h = Harness(downstream_window=0)
            h.headers(end=True)
            h.from_origin()
            h.response(body=b"old")
            h.runtime.state.deadline = 130.0
            h.tick()
            self.assertFalse(h.adapter.failed)
            self.assertFalse(h.runtime.finished)
        with patch("credential_bound_h2.time.monotonic", return_value=130.0):
            h.tick()
        self.assertTrue(h.adapter.failed)
        self.assertFalse(h.adapter.goaway_sent)
        self.assertFalse(h.runtime.finished)

    def test_empty_fenced_attach_uses_frozen_zero_once(self):
        h = Harness()
        h.fence(last=0)
        h.tick()
        self.assertTrue(h.adapter.goaway_sent)
        for _ in range(3):
            h.run(events.DataReceived(h.client, b""))
        self.assertEqual(
            len(
                [
                    x
                    for x in h.peer_events
                    if isinstance(x, h2.events.ConnectionTerminated)
                ]
            ),
            1,
        )

    def test_unknown_upstream_ancestry_fails_closed_retains_handle(self):
        h = Harness()
        h.headers()
        conn, owner = h.adapter.owned_upstreams()[0]
        h.root.connections[conn] = (
            owner.child_layer
        )  # unsupported eager-style direct protocol
        h.tick()
        self.assertTrue(h.adapter.failed)
        self.assertFalse(h.runtime.finished)
        self.assertFalse(h.runtime.prepared)
        self.assertTrue(
            any(
                isinstance(x, commands.CloseConnection) and x.connection is h.client
                for x in h.output
            )
        )

    def test_local_response_has_no_upstream_obligation(self):
        h = Harness(local_response=b"local", stream_request=False)
        h.headers(end=True)
        h.tick()
        self.assertEqual(len(h.runtime.finished), 1)
        h.assert_no_origin(self)

    def test_ordinary_reset_releases_only_reset_stream_without_abnormal_close(self):
        h = Harness(initial_window=0)
        h.headers(1)
        h.from_origin()
        h.headers(3)
        h.data(1, b"buffered")
        handle = h.flows[1].metadata["opensandbox_credential_request_handle"]
        h.reset(1)
        h.tick()
        self.assertFalse(h.adapter.failed)
        self.assertEqual(h.runtime.finished, [handle])
        self.assertEqual(list(h.adapter._active), [3])
        self.assertFalse(h.runtime.prepared)
        self.assertTrue(
            any(
                isinstance(x, h2.events.StreamReset) and x.stream_id == 1
                for x in h.origin_events
            )
        )

    def test_response_headers_without_end_then_reset_still_propagates(self):
        h = Harness(initial_window=0)
        h.headers(1)
        h.from_origin()
        h.data(1, b"upload")
        h.origin.send_headers(1, [(":status", "200")], end_stream=False)
        h.from_origin()
        h.reset(1)
        h.tick()
        self.assertFalse(h.adapter.failed)
        self.assertEqual(len(h.runtime.finished), 1)
        self.assertFalse(h.runtime.prepared)

    def test_response_hook_paused_then_reset_detects_orphan_on_resume(self):
        h = Harness(initial_window=0, hold={("response", 1)})
        h.headers(1)
        h.from_origin()
        h.data(1, b"orphan")
        h.response()
        h.reset(1)
        self.assertFalse(h.adapter.failed)
        h.release(("response", 1))
        self.assertTrue(h.adapter.failed)
        self.assertEqual(h.runtime.prepared, h.runtime.consumed)
        self.assertFalse(h.runtime.finished)

    def test_buffered_completed_upload_and_trailers_keep_handle_until_window(self):
        for trailers in (False, True):
            with self.subTest(trailers=trailers):
                h = Harness(initial_window=0)
                h.headers(1)
                h.from_origin()
                h.data(1, b"upload", end=not trailers)
                if trailers:
                    h.peer.send_headers(1, [("x-upload", "done")], end_stream=True)
                    h.flush()
                h.response()
                h.fence()
                h.tick()
                self.assertFalse(h.runtime.finished)
                self.assertFalse(h.adapter.goaway_sent)
                record = h.adapter._active[1]
                child = h.adapter._association(record)[2]
                self.assertTrue(child.h2_conn.stream_buffers)
                self.assertEqual(bool(child.h2_conn.stream_trailers), trailers)
                h.origin.increment_flow_control_window(100, stream_id=1)
                h.from_origin()
                h.tick()
                self.assertFalse(child.h2_conn.stream_buffers)
                self.assertFalse(child.h2_conn.stream_trailers)
                self.assertEqual(len(h.runtime.finished), 1)
                self.assertTrue(h.adapter.goaway_sent)

    def test_queue_advances_only_through_dependency_and_frozen_bound_is_client_id(self):
        h = Harness(max_streams=1)
        h.headers(1, end=True)
        h.from_origin()
        h.headers(3, end=True, extra=[("x-dynamic", "cached-value")])
        self.assertIn(
            (b"x-dynamic", b"cached-value"),
            h.peer.encoder.header_table.dynamic_entries,
        )
        h.peer.max_outbound_frame_size = 16384
        long_value = "".join(chr(33 + (index * 17) % 90) for index in range(45000))
        h.peer.send_headers(
            5,
            [
                (":method", "POST"),
                (":scheme", "https"),
                (":authority", "example.test"),
                (":path", "/5"),
                ("x-dynamic", "cached-value"),
                ("x-long", long_value),
            ],
            end_stream=False,
        )
        h.peer.send_data(5, b"post-bound", end_stream=True)
        post_bound_bytes = h.peer.data_to_send()
        post_bound_frames = h.decode_frames(post_bound_bytes)
        self.assertTrue(any(isinstance(frame, ContinuationFrame)
                            for frame in post_bound_frames))
        h.fence()
        h.tick()
        h.deny = True
        h.parse_client_wire = False
        data_window = h.adapter.h2_conn._inbound_flow_control_window_manager
        bytes_processed = data_window._bytes_processed
        h.run(events.DataReceived(h.client, post_bound_bytes))
        self.assertEqual(data_window._bytes_processed - bytes_processed, len(b"post-bound"))
        self.assertNotIn(5, h.adapter._active)
        self.assertNotIn(5, h.root.streams)
        h.assert_identity_empty(self)
        self.assertTrue(any(
            (b"x-dynamic", b"cached-value") in identity.headers
            for identity in h.identities if identity.stream_id == 5
        ))
        self.assertNotIn(5, h.adapter.h2_conn.stream_buffers)
        self.assertNotIn(5, h.adapter.h2_conn.stream_trailers)
        self.assertEqual(h.adapter._association(h.adapter._active[3])[3], "queued")
        h.response(1, body=b"first")
        h.tick()
        self.assertFalse(h.adapter.goaway_sent)
        self.assertEqual(h.adapter._association(h.adapter._active[3])[-1], 3)
        h.response(3, body=b"second")
        h.tick()
        self.assertEqual(len(h.runtime.finished), 2)
        self.assertFalse(h.adapter.failed)
        frames = h.client_frames()
        terms = [
            frame for frame in frames if isinstance(frame, GoAwayFrame)
        ]
        self.assertEqual(
            [(frame.last_stream_id, frame.error_code) for frame in terms],
            [(3, 0), (3, 0)],
        )
        refused = [
            frame for frame in frames
            if isinstance(frame, RstStreamFrame) and frame.stream_id == 5
        ]
        self.assertEqual(len(refused), 1)
        self.assertEqual(refused[0].error_code, h2.errors.ErrorCodes.REFUSED_STREAM)
        self.assertFalse(
            any(
                isinstance(frame, (HeadersFrame, DataFrame)) and frame.stream_id == 5
                for frame in frames
            )
        )
        self.assertEqual(
            [
                x.stream_id
                for x in h.origin_events
                if isinstance(x, h2.events.RequestReceived)
            ],
            [1, 3],
        )

    def test_pending_failure_keeps_exact_failed_attempt_until_transport_terminal(self):
        h = Harness(connected=False)
        h.headers(1)
        pending = h.adapter.owned_upstreams()[0]
        pending[0].error = "synthetic cancellation"
        h.run(events.OpenConnectionCompleted(h.opens[0], "synthetic cancellation"))
        self.assertEqual(h.adapter.owned_upstreams(), (pending,))
        self.assertFalse(h.runtime.finished)
        self.assertFalse(h.adapter.failed)
        h.runtime.terminal.append(pending[0])
        h.reset(1)
        h.tick()
        self.assertEqual(len(h.runtime.finished), 1)

    def test_final_send_is_consumed_before_handle_release(self):
        h = Harness(downstream_window=0)
        h.headers(end=True)
        h.from_origin()
        h.response(body=b"done")
        h.peer.increment_flow_control_window(100, stream_id=1)
        generator = h.root.handle_event(
            events.DataReceived(h.client, h.peer.data_to_send())
        )
        command = next(generator)
        self.assertIsInstance(command, commands.SendData)
        self.assertFalse(h.runtime.finished)
        h.peer_events.extend(h.peer.receive_data(command.data))
        list(generator)
        self.assertEqual(len(h.runtime.finished), 1)

    def test_disconnected_repeated_events_do_not_release_or_rearm_protocol(self):
        h = Harness()
        h.headers()
        h.response()
        h.reset(1)
        h.close_event(h.client)
        count = len(h.output)
        h.close_event(h.client)
        h.run(events.DataReceived(h.client, b""))
        self.assertFalse(h.runtime.finished)
        self.assertFalse(h.adapter.goaway_sent)
        self.assertFalse(
            any(isinstance(x, commands.SendData) for x in h.output[count:])
        )

    def test_upstream_reset_stranded_trailers_fail_without_new_deadline(self):
        h = Harness(initial_window=0)
        h.headers(1)
        h.from_origin()
        h.data(1, b"body")
        h.peer.send_headers(1, [("x-trailer", "done")], end_stream=True)
        h.flush()
        child = h.adapter._association(h.adapter._active[1])[2]
        self.assertIn(1, child.h2_conn.stream_trailers)
        h.reset(1)
        self.assertTrue(h.adapter.failed)
        self.assertIn(1, child.h2_conn.stream_trailers)
        self.assertFalse(h.runtime.finished)
        self.assertFalse(h.adapter.goaway_sent)

    def test_repeated_admitted_cleanup_keeps_only_active_records(self):
        h = Harness(capacity=1)
        for sid in range(1, 201, 2):
            h.headers(sid, end=True)
            h.from_origin()
            h.response(sid)
            h.tick()
            self.assertFalse(h.adapter.failed)
            self.assertFalse(h.adapter._active)
            self.assertFalse(h.root.streams)
            self.assertFalse(h.adapter.streams)
            h.assert_identity_empty(self)
            self.assertNotIn(
                "opensandbox_credential_request_handle", h.flows[sid].metadata
            )
        self.assertEqual(len(h.runtime.finished), 100)
        self.assertEqual(h.runtime.state.max_admitted_stream_id, 199)

    def test_incompatible_dependencies_and_eager_strategy_reject_installation(self):
        h = Harness()
        with (
            patch(
                "credential_bound_h2.dependency_versions_supported", return_value=False
            ),
            self.assertRaises(H2IdentityError),
        ):
            BoundH2Server(h.context.fork(), h.runtime, h.root, h.runtime.token)
        h.context.options.connection_strategy = "eager"
        with self.assertRaises(H2IdentityError):
            BoundH2Server(h.context.fork(), h.runtime, h.root, h.runtime.token)

    def test_disconnect_clears_pending_but_keeps_active_transport_claims(self):
        h = Harness(hold={("requestheaders", 3)})
        h.headers(1)
        h.headers(3)
        owned = h.adapter.owned_upstreams()
        h.adapter.on_client_disconnected()
        self.assertTrue(h.adapter.client_closed)
        h.assert_identity_empty(self)
        self.assertEqual(h.adapter.owned_upstreams(), owned)
        self.assertFalse(h.runtime.finished)
        self.assertIsNone(h.adapter._wakeup)


if __name__ == "__main__":
    unittest.main()
