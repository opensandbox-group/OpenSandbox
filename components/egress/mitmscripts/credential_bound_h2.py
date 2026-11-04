# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Fixed mitmproxy 11.0.2 instance adapter for live H2 admission.

Only requestheaders acquires a registry handle. This layer observes the raw
client identity and retains admitted handles until local protocol obligations
finish. SendData consumption is not a claim of remote receipt. Transport/task
termination, including the narrow D3 fallback, belongs to LiveRuntime.
"""

from __future__ import annotations

import importlib.metadata
import sys
import time
from dataclasses import dataclass
from typing import Any

import h2.config
import h2.errors
import h2.events
import h2.exceptions
import h2.stream
from hyperframe.frame import GoAwayFrame
from mitmproxy import http
from mitmproxy.net.http import status_codes
from mitmproxy.proxy import commands, events
from mitmproxy.proxy.layers import http as layers
from mitmproxy.proxy.layers.http._http_h2 import BufferedH2Connection
from mitmproxy.proxy.layers.http._base import HttpEvent, ReceiveHttp
from mitmproxy.proxy.layers.http._http1 import Http1Client
from mitmproxy.proxy.layers.http._http2 import Http2Client, Http2Server
from mitmproxy.proxy.layers.tls import ServerTLSLayer

PINS = {"mitmproxy": "11.0.2", "h2": "4.1.0", "hyperframe": "6.0.1"}
_POLL_SECONDS = 0.1


class H2IdentityError(ValueError):
    """An exact one-shot request identity was lost or replaced."""


class H2OwnershipError(ValueError):
    """The pinned lazy upstream ancestry cannot prove ownership."""


def dependency_versions_supported() -> bool:
    # mitmproxy enables eager tasks on Python >= 3.12. Its ServerConnect hook
    # then runs before transports[connection] is registered, outside this
    # adapter's proven ownership ordering. Reject before installing the layer.
    if sys.implementation.name != "cpython" or sys.version_info[:2] not in ((3, 10), (3, 11)):
        return False
    try:
        return all(
            importlib.metadata.version(name) == pin for name, pin in PINS.items()
        )
    except importlib.metadata.PackageNotFoundError:
        return False


@dataclass(frozen=True, eq=False)
class RequestIdentity:
    stream_id: int
    headers: tuple[tuple[bytes, bytes], ...]
    request_end_stream: bool
    request: http.Request


@dataclass
class _Request:
    identity: RequestIdentity
    flow: http.HTTPFlow
    handle: Any
    application_done: bool = False
    response_done: bool = False
    request_ended: bool = False
    peer_reset: bool = False
    # Only a live request retains this association; no stream/task history.
    upstream: tuple[Any, Any] | None = None


class BoundH2Server(Http2Server):
    """Installed in one not-yet-started HttpLayer client instance slot."""

    def __init__(self, context, runtime, http_layer, token):
        if (
            not dependency_versions_supported()
            or type(http_layer) is not layers.HttpLayer
            or http_layer.context.client is not context.client
            or context.client.alpn != b"h2"
            or context.options.connection_strategy != "lazy"
            or runtime.token_for(context.client) is not token
            or token is None
        ):
            raise H2IdentityError("credential proxy: unsupported H2 adapter context")
        # The upstream class normally shares this configuration across instances.
        # Keep the instance-specific validation setting local to this adapter.
        self.h2_conf = h2.config.H2Configuration(
            **self.h2_conf_defaults, client_side=False
        )
        super().__init__(context)
        self.runtime = runtime
        self.http_layer = http_layer
        self.token = token
        self.client_closed = False
        self.failed = False
        self.close_started = False
        self.goaway_sent = False
        self.retirement_notice_sent = False
        self._close_commands_sent = False
        self._frozen_goaway_bound = None
        self._minimum_serialized_goaway_bound = None
        self._pending: dict[int, RequestIdentity] = {}
        self._claimed: dict[int, RequestIdentity] = {}
        self._refused: dict[int, http.HTTPFlow] = {}
        self._active: dict[int, _Request] = {}
        self._wakeup = None
        self._reset_stream = None
        self._event_depth = 0
        self._install_retirement_serializer()

    def _install_retirement_serializer(self) -> None:
        """Clamp GOAWAY frames on this fresh server connection only.

        The pinned BufferedH2Connection continues to own parsing, HPACK,
        CONTINUATION assembly, flow control, and output buffering. This
        retirement-only wrapper observes frames immediately before the
        dependency serializes them and never changes its state machine.
        """
        connection = self.h2_conn
        if (
            type(connection) is not BufferedH2Connection
            or connection._data_to_send
        ):
            raise H2IdentityError(
                "credential proxy: H2 serializer installed after connection start"
            )
        original_prepare = connection._prepare_for_sending

        def prepare_for_sending(frames):
            # The pinned h2 4.1.0 call sites pass re-iterable frame lists.
            # Ordinary DATA/HEADERS/control batches do not consume retirement
            # state, so keep them off the registry-wide lock and deadline scan.
            if not any(isinstance(frame, GoAwayFrame) for frame in frames):
                original_prepare(frames)
                return

            state = self.runtime.registry.drain_state(self.token)
            if (
                state is not None
                and state.fenced
                and state.frozen_last_stream_id is not None
            ):
                bound = state.frozen_last_stream_id
                if self._frozen_goaway_bound is None:
                    self._frozen_goaway_bound = bound
                else:
                    self._frozen_goaway_bound = min(
                        self._frozen_goaway_bound, bound
                    )

            serialized_bound = self._minimum_serialized_goaway_bound
            for frame in frames:
                if not isinstance(frame, GoAwayFrame):
                    continue
                limits = [frame.last_stream_id]
                if self._frozen_goaway_bound is not None:
                    limits.append(self._frozen_goaway_bound)
                if serialized_bound is not None:
                    limits.append(serialized_bound)
                frame.last_stream_id = min(limits)
                serialized_bound = frame.last_stream_id

            original_prepare(frames)
            if serialized_bound is not None:
                self._minimum_serialized_goaway_bound = serialized_bound

        # An instance attribute shadows the inherited bound method without
        # changing BufferedH2Connection or H2Connection for other connections.
        connection._prepare_for_sending = prepare_for_sending

    def _send_retirement_notice(self, last_stream_id: int):
        """Queue one advisory NO_ERROR GOAWAY without closing h2's state machine."""
        if self.retirement_notice_sent:
            return
        self.h2_conn._prepare_for_sending(
            [
                GoAwayFrame(
                    stream_id=0,
                    last_stream_id=last_stream_id,
                    error_code=h2.errors.ErrorCodes.NO_ERROR,
                    additional_data=b"",
                )
            ]
        )
        self.retirement_notice_sent = True
        data = self.h2_conn.data_to_send()
        if data:
            yield commands.SendData(self.conn, data)

    def fail_closed(self) -> None:
        """Hook-safe flag; normal layer commands run on the next layer event."""
        self.failed = True

    def consume_identity(self, request) -> RequestIdentity | None:
        identity = self._pending.pop(id(request), None)
        if identity is None or identity.request is not request:
            return None
        if self.client_closed or self.close_started or self.failed:
            return None
        self._claimed[identity.stream_id] = identity
        return identity

    def request_is_closed(self, request) -> bool:
        """Recognize a late suspended hook after its peer already reset.

        Raw identity is deliberately cleared by reset. This read-only exact
        HttpStream association only permits terminal kill, never admission.
        """
        candidates = list(self.http_layer.streams.values())
        candidates.extend(self.http_layer.command_sources.values())
        return any(
            isinstance(stream, layers.HttpStream)
            and getattr(stream, "flow", None) is not None
            and stream.flow.request is request
            and stream.flow.client_conn is self.context.client
            and self.is_closed(stream.stream_id)
            for stream in candidates
        )

    def _claim(self, identity, flow) -> None:
        if (
            self._claimed.get(identity.stream_id) is not identity
            or flow.request is not identity.request
            or flow.client_conn is not self.context.client
            or identity.stream_id in self._active
            or self.client_closed
            or self.close_started
            or self.failed
        ):
            self.fail_closed()
            raise H2IdentityError("credential proxy: invalid H2 request identity")
        self._claimed.pop(identity.stream_id)

    def bind_request(self, identity, flow, handle) -> None:
        self._claim(identity, flow)
        if handle is None or handle.client_stream_id != identity.stream_id:
            self.fail_closed()
            raise H2IdentityError("credential proxy: invalid H2 request handle")
        stream = self.h2_conn.streams.get(identity.stream_id)
        ended = identity.request_end_stream or (
            stream is not None
            and stream.state_machine.state is h2.stream.StreamState.HALF_CLOSED_REMOTE
        )
        self._active[identity.stream_id] = _Request(
            identity, flow, handle, request_ended=ended
        )

    def refuse(self, identity, flow) -> None:
        self._claim(identity, flow)
        self._refused[identity.stream_id] = flow
        flow.kill()

    def application_done(self, flow) -> None:
        for record in self._active.values():
            if record.flow is flow:
                record.application_done = True
                record.response_done = (
                    flow.response is not None
                    and flow.response.timestamp_end is not None
                )
                return

    def on_client_disconnected(self) -> None:
        # LiveRuntime releases the token only after its upstream task checks.
        self.client_closed = True
        self.close_started = True
        self._wakeup = None
        self._pending.clear()
        self._claimed.clear()
        self._refused.clear()

    def _forget_identity(self, stream_id) -> None:
        for key, identity in tuple(self._pending.items()):
            if identity.stream_id == stream_id:
                self._pending.pop(key)
        self._claimed.pop(stream_id, None)
        self._refused.pop(stream_id, None)

    def _drop_stream(self, stream_id):
        self._forget_identity(stream_id)
        # A base StreamReset handler still has its own pop after ReceiveHttp.
        if self._reset_stream != stream_id:
            self.streams.pop(stream_id, None)
        yield layers.DropStream(stream_id)

    def handle_h2_event(self, event):
        if isinstance(event, h2.events.RequestReceived):
            if self.failed or self.close_started:
                return True
            count = len(self._pending) + len(self._claimed) + len(self._refused)
            if count >= self.runtime.registry.request_capacity:
                # Association overflow is an abnormal close, never a claim that
                # an as-yet-unvalidated request is safely retryable.
                self.fail_closed()
                return True
            generator = super().handle_h2_event(event)
            reply = None
            while True:
                try:
                    command = generator.send(reply)
                except StopIteration as stopped:
                    if stopped.value:
                        self.fail_closed()
                    return stopped.value  # preserve the parser's stop signal
                if isinstance(command, ReceiveHttp) and isinstance(
                    command.event, layers.RequestHeaders
                ):
                    request = command.event.request
                    identity = RequestIdentity(
                        event.stream_id,
                        tuple(
                            (bytes(name), bytes(value)) for name, value in event.headers
                        ),
                        bool(event.stream_ended),
                        request,
                    )
                    self._pending[id(request)] = identity
                reply = yield command
        elif isinstance(event, h2.events.StreamReset):
            self._forget_identity(event.stream_id)
            record = self._active.get(event.stream_id)
            if record is not None:
                record.peer_reset = True
            self._reset_stream = event.stream_id
            try:
                return (yield from super().handle_h2_event(event))
            finally:
                self._reset_stream = None
        elif isinstance(event, h2.events.StreamEnded):
            record = self._active.get(event.stream_id)
            if record is not None:
                record.request_ended = True
        elif isinstance(
            event, (h2.events.ConnectionTerminated, h2.exceptions.ProtocolError)
        ):
            self.fail_closed()
            self._pending.clear()
            self._claimed.clear()
            self._refused.clear()
        return (yield from super().handle_h2_event(event))

    def _handle_event(self, event):
        if isinstance(event, events.Wakeup):
            # Only our own wakeups are routed to this concrete instance.
            if event.command is self._wakeup:
                self._wakeup = None
            return
        if isinstance(event, HttpEvent) and event.stream_id in self._refused:
            if (
                not isinstance(event, layers.ResponseProtocolError)
                or event.code != status_codes.NO_RESPONSE
                or event.stream_id in self.h2_conn.stream_trailers
            ):
                # check_invalid/check_body_size may produce 400/413 after the
                # hook. Suppress those HTTP bytes without inventing REFUSED.
                self.fail_closed()
                return
            if not self.is_closed(event.stream_id):
                self.h2_conn.reset_stream(
                    event.stream_id, h2.errors.ErrorCodes.REFUSED_STREAM
                )
                data = self.h2_conn.data_to_send()
                if data:
                    yield commands.SendData(self.conn, data)
            yield from self._drop_stream(event.stream_id)
            return
        if self.failed or self.close_started:
            return
        yield from super()._handle_event(event)
        # The fixed dependency's local error path does not DropStream.
        # Do this only after its normal output commands were consumed.
        if isinstance(event, layers.ResponseProtocolError) and self.is_closed(
            event.stream_id
        ):
            yield from self._drop_stream(event.stream_id)

    def handle_event(self, event):
        self._event_depth += 1
        try:
            if (
                isinstance(event, events.ConnectionClosed)
                and event.connection is self.conn
            ):
                self.on_client_disconnected()
                # Continue normal protocol error delivery to active HttpStreams.
                stream_ids = tuple(self.streams)
                yield from super().close_connection("peer closed connection")
                for sid in stream_ids:
                    yield layers.DropStream(sid)
            else:
                yield from super().handle_event(event)
        finally:
            self._event_depth -= 1
        if self._event_depth == 0:
            yield from self._maintain()

    def close_connection(self, msg):
        self.fail_closed()
        self.close_started = True
        stream_ids = tuple(self.streams)
        yield from super().close_connection(msg)
        for sid in stream_ids:
            yield layers.DropStream(sid)
        self._pending.clear()
        self._claimed.clear()
        self._refused.clear()

    def _upstream_child(self, conn, owner):
        if not any(
            key is conn and value is owner
            for key, value in self.http_layer.connections.items()
        ):
            raise H2OwnershipError("credential proxy: stale H2 upstream owner")
        child = owner
        if type(child) is ServerTLSLayer:
            if (
                child.context.client is not self.conn
                or child.context.server is not conn
            ):
                raise H2OwnershipError(
                    "credential proxy: invalid H2 upstream TLS owner"
                )
            child = child.child_layer
        if (
            type(child) is not layers.HttpClient
            or child.context.client is not self.conn
            or child.context.server is not conn
        ):
            raise H2OwnershipError("credential proxy: unsupported H2 upstream ancestry")
        protocol = getattr(child, "child_layer", None)
        if protocol is not None and (
            type(protocol) not in (Http1Client, Http2Client)
            or protocol.conn is not conn
            or protocol.context.client is not self.conn
        ):
            raise H2OwnershipError("credential proxy: unsupported H2 upstream protocol")
        return protocol

    def _association(self, record, *, remember=True):
        sid, flow = record.identity.stream_id, record.flow
        pending = []
        for conn, waiting in self.http_layer.waiting_for_establishment.items():
            for command in waiting:
                stream = self.http_layer.command_sources.get(command)
                if (
                    isinstance(stream, layers.HttpStream)
                    and stream.stream_id == sid
                    and getattr(stream, "flow", None) is flow
                ):
                    pending.append((conn, self.http_layer.connections.get(conn)))
        if len(pending) > 1:
            raise H2OwnershipError("credential proxy: ambiguous H2 pending upstream")
        if pending:
            conn, owner = pending[0]
            child = self._upstream_child(conn, owner)
            if remember:
                record.upstream = (conn, owner)
            return conn, owner, child, "connecting", None

        conn = flow.server_conn
        if (
            record.upstream is not None
            and conn is not record.upstream[0]
            and record.upstream[0].error is not None
        ):
            # A failed GetHttpConnection does not replace flow.server_conn.
            # Keep its previously proven exact pending attempt until the
            # runtime confirms that attempt's task/transport termination.
            conn, owner = record.upstream
            child = self._upstream_child(conn, owner)
            return conn, owner, child, "connecting", None
        owner = self.http_layer.connections.get(conn)
        if owner is None:
            if record.upstream is not None:
                raise H2OwnershipError("credential proxy: missing H2 upstream owner")
            if conn is not self.http_layer.context.server:
                raise H2OwnershipError(
                    "credential proxy: unowned H2 upstream connection"
                )
            return None  # lazy request/local response; no upstream opened
        child = self._upstream_child(conn, owner)
        if child is None:
            raise H2OwnershipError("credential proxy: missing H2 pending command")
        if type(child) is Http2Client:
            ours = child.our_stream_id.get(sid)
            queued = sid in child.stream_queue
            if (ours is not None and queued) or (
                ours is not None and child.their_stream_id.get(ours) != sid
            ):
                raise H2OwnershipError(
                    "credential proxy: invalid H2 upstream stream mapping"
                )
            kind = "queued" if queued else "mapped" if ours is not None else "unsent"
        else:
            ours = None
            kind = (
                "mapped"
                if child.stream_id == sid and child.request is flow.request
                else "unsent"
            )
        if kind == "unsent" and record.upstream is None:
            return None
        if remember:
            record.upstream = (conn, owner)
        return conn, owner, child, kind, ours

    def owned_upstreams(self):
        """Exact current request ownership; LiveRuntime owns idle transports."""
        result = []
        try:
            for record in self._active.values():
                association = self._association(record)
                if association is not None:
                    conn, owner = association[:2]
                    if not any(c is conn and o is owner for c, o in result):
                        result.append((conn, owner))
        except H2OwnershipError:
            self.fail_closed()
            raise
        return tuple(result)

    def _upstream_done(self, record, *, observe=True):
        association = self._association(record, remember=observe)
        if association is None:
            return True, False
        conn, owner, child, kind, ours = association
        terminal = (self.runtime.h2_transport_terminal if observe
                    else self.runtime.h2_transport_known_terminal)
        if terminal(self, conn, owner):
            return True, False
        if kind in ("connecting", "queued"):
            return False, False
        if type(child) is Http2Client:
            buffered = bool(child.h2_conn.stream_buffers.get(ours))
            trailers = ours in child.h2_conn.stream_trailers
            closed = child.is_closed(ours)
            if child._handle_event == child.done:
                # Protocol closure alone cannot prove a cancelled transport is
                # terminal, or erase queued upload obligations.
                return False, closed and trailers
            return closed and not buffered and not trailers, closed and trailers
        if type(child) is Http1Client:
            return child.request_done and child.response_done, False
        raise H2OwnershipError("credential proxy: incomplete H2 upstream association")

    def _close_owned(self):
        if self._close_commands_sent or self.client_closed:
            return
        self._close_commands_sent = True
        self.close_started = True
        observations = []
        try:
            for conn, owner in self.owned_upstreams():
                if not self.runtime.h2_transport_terminal(self, conn, owner):
                    observations.append(
                        (conn, self.runtime.prepare_h2_close(self, conn, owner))
                    )
        except ValueError:
            # Unknown ownership never licenses D3 or handle release. The
            # verified runtime teardown will retain and report the blocker.
            self.failed = True
        if self.failed:
            yield commands.Log("credential proxy: H2 connection terminated abnormally")
        for conn, observation in observations:
            yield commands.CloseConnection(conn)
            self.runtime.h2_close_consumed(observation)
        stream_ids = tuple(self.streams)
        yield from super().close_connection("credential proxy: H2 connection closed")
        for sid in stream_ids:
            yield layers.DropStream(sid)
        self._pending.clear()
        self._claimed.clear()
        self._refused.clear()
        self._handle_event = self.done

    def _request_finished(self, sid, record, upstream_done):
        return (
            record.application_done
            and sid not in self.http_layer.streams
            and self.is_closed(sid)
            and not self.h2_conn.stream_buffers.get(sid)
            and sid not in self.h2_conn.stream_trailers
            and upstream_done
        )

    def maintenance_needed(self):
        """Read-only wake filter; no task observation or protocol commands.

        A fence, pending request, or future deadline alone is not progress.
        Polling those must not refresh mitmproxy's inactivity watchdog.
        """
        if self.client_closed or self.close_started:
            return False
        state = self.runtime.registry.drain_state(self.token)
        if (self.failed or state is None
                or (state.fenced and state.frozen_last_stream_id is None)
                or (state.deadline is not None and time.monotonic() >= state.deadline)
                or any(sid not in self._active for sid in self.h2_conn.stream_trailers)):
            return True
        if (
            state.fenced
            and state.frozen_last_stream_id is not None
            and not self.retirement_notice_sent
        ):
            return True
        try:
            for sid, record in self._active.items():
                upstream_done, upstream_trailers = self._upstream_done(record, observe=False)
                if self._request_finished(sid, record, upstream_done):
                    return True
                if ((self.is_closed(sid) and sid in self.h2_conn.stream_trailers)
                        or upstream_trailers) and state.deadline is None:
                    return True
                if (record.peer_reset and record.response_done
                        and not record.request_ended and not upstream_done):
                    return True
        except H2OwnershipError:
            return True  # normal event dispatch performs the fail-closed transition
        return bool(
            state.fenced and state.frozen_last_stream_id is not None
            and not self._active and not self._pending and not self._claimed
            and not self._refused and not self.http_layer.streams
            and not self.h2_conn.stream_buffers and not self.h2_conn.stream_trailers
        )

    def _maintain(self):
        if self.client_closed:
            return
        state = self.runtime.registry.drain_state(self.token)
        if (
            state is None
            or (state.fenced and state.frozen_last_stream_id is None)
            or state.deadline is not None
            and time.monotonic() >= state.deadline
        ):
            self.fail_closed()
        if any(sid not in self._active for sid in self.h2_conn.stream_trailers):
            self.fail_closed()
        if not self.failed and not self.close_started:
            try:
                for sid, record in tuple(self._active.items()):
                    upstream_done, upstream_trailers = self._upstream_done(record)
                    stranded = (
                        self.is_closed(sid) and sid in self.h2_conn.stream_trailers
                    ) or upstream_trailers
                    if stranded and state.deadline is None:
                        self.fail_closed()
                    if (
                        record.peer_reset
                        and record.response_done
                        and not record.request_ended
                        and not upstream_done
                    ):
                        # An early response suppresses the dependency's reset
                        # propagation. An empty buffer does not finish upload.
                        self.fail_closed()
                    if self._request_finished(sid, record, upstream_done):
                        self._active.pop(sid)
                        self.streams.pop(sid, None)
                        if (
                            record.flow.metadata.get(
                                "opensandbox_credential_request_handle"
                            )
                            is record.handle
                        ):
                            record.flow.metadata.pop(
                                "opensandbox_credential_request_handle"
                            )
                        self.runtime.finish_request(record.handle)
                        self.runtime.prune_h2_transports(self)
            except H2OwnershipError:
                self.fail_closed()
        if self.failed:
            yield from self._close_owned()
            return
        if self.close_started:
            return
        drained = (
            not self._active
            and not self._pending
            and not self._claimed
            and not self._refused
            and not self.http_layer.streams
            and not self.h2_conn.stream_buffers
            and not self.h2_conn.stream_trailers
        )
        if (
            state.fenced
            and state.frozen_last_stream_id is not None
            and not drained
            and not self.retirement_notice_sent
        ):
            yield from self._send_retirement_notice(
                state.frozen_last_stream_id
            )
        if (
            state.fenced
            and state.frozen_last_stream_id is not None
            and drained
        ):
            self.goaway_sent = True
            self.retirement_notice_sent = True
            self.h2_conn.close_connection(
                error_code=h2.errors.ErrorCodes.NO_ERROR,
                last_stream_id=state.frozen_last_stream_id,
            )
            yield commands.SendData(self.conn, self.h2_conn.data_to_send())
            yield from self._close_owned()
            return
        if self._wakeup is None:
            try:
                self.runtime.filter_h2_wakeups(self)
            except ValueError:
                self.fail_closed()
                yield from self._close_owned()
                return
            self._wakeup = commands.RequestWakeup(_POLL_SECONDS)
            yield self._wakeup
