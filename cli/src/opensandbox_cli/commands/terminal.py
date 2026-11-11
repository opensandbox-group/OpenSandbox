# Copyright 2026 The OpenSandbox Authors.
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

"""Interactive terminal into a sandbox over the execd PTY WebSocket.

Frame protocol is defined in components/execd/PTY.md.
"""

from __future__ import annotations

import asyncio
import json
import os
import signal
import sys
from collections.abc import AsyncIterator, Awaitable, Callable, Iterator
from contextlib import contextmanager
from datetime import timedelta
from typing import IO, Protocol

import click
import httpx
import websockets
from opensandbox.constants import DEFAULT_EXECD_PORT

from opensandbox_cli.client import ClientContext
from opensandbox_cli.utils import handle_errors

# Binary WebSocket frame type bytes, mirroring execd pkg/web/model.
BIN_STDIN = 0x00
BIN_STDOUT = 0x01
BIN_STDERR = 0x02
BIN_REPLAY = 0x03

ReadFn = Callable[[], Awaitable[bytes]]
WriteFn = Callable[[bytes], None]
SizeFn = Callable[[], tuple[int, int]]


class WSDuplex(Protocol):
    """Minimal WebSocket surface used by the relay loop."""

    def send(self, message: bytes | str) -> Awaitable[None]: ...

    def __aiter__(self) -> AsyncIterator[bytes | str]: ...


def _ws_scheme(protocol: str) -> str:
    return "wss" if protocol == "https" else "ws"


def _execd_base(endpoint: str) -> str:
    """Normalize an execd endpoint (``host:port`` or ``host/proxy/path``)."""
    return endpoint.rstrip("/")


def _create_pty_session(
    base_url: str,
    headers: dict[str, str],
    *,
    workdir: str | None,
    timeout: timedelta,
) -> str:
    body: dict[str, str] = {}
    if workdir:
        body["cwd"] = workdir
    try:
        response = httpx.post(
            f"{base_url}/pty",
            headers=headers,
            json=body,
            timeout=timeout.total_seconds(),
        )
    except httpx.HTTPError as exc:
        raise click.ClickException(f"Could not reach the sandbox execd: {exc}") from exc
    if response.status_code != 201:
        detail = response.text.strip() or response.reason_phrase
        raise click.ClickException(
            f"Creating a PTY session failed with HTTP {response.status_code}: {detail}"
        )
    session_id = response.json().get("session_id")
    if not session_id:
        raise click.ClickException("The execd response did not include a session_id.")
    return str(session_id)


def _delete_pty_session(
    base_url: str, headers: dict[str, str], session_id: str, *, timeout: timedelta
) -> None:
    """Best-effort teardown of a finished PTY session; errors are ignored."""
    try:
        httpx.delete(
            f"{base_url}/pty/{session_id}",
            headers=headers,
            timeout=timeout.total_seconds(),
        )
    except httpx.HTTPError:
        pass


@contextmanager
def _raw_mode(fd: int) -> Iterator[None]:
    import termios
    import tty

    old = termios.tcgetattr(fd)
    tty.setraw(fd)
    try:
        yield
    finally:
        termios.tcsetattr(fd, termios.TCSADRAIN, old)


def _make_stdin_reader(fd: int) -> ReadFn:
    """Build a cancellable stdin read driven by event-loop readability.

    A blocking ``os.read`` in the default executor cannot be cancelled: the
    worker thread stays parked in ``read(2)`` and stalls event-loop shutdown
    until a key arrives. A reader callback is cancelled with its task.
    """

    async def stdin_read() -> bytes:
        loop = asyncio.get_running_loop()
        future: asyncio.Future[bytes] = loop.create_future()

        def _on_readable() -> None:
            if future.done():
                return
            try:
                future.set_result(os.read(fd, 4096))
            except BlockingIOError:
                return  # Spurious wakeup; stay registered.
            except OSError:
                future.set_result(b"")  # EOF or hang-up.

        loop.add_reader(fd, _on_readable)
        try:
            return await future
        finally:
            loop.remove_reader(fd)

    return stdin_read


def _posix_tty_io() -> tuple[ReadFn, WriteFn, SizeFn, int]:
    """Build TTY stdin/stdout/size hooks plus the stdin fd for raw mode."""
    stdin_fd = sys.stdin.fileno()
    stdout: IO[bytes] = sys.stdout.buffer
    stdin_read = _make_stdin_reader(stdin_fd)

    def stdout_write(data: bytes) -> None:
        stdout.write(data)
        stdout.flush()

    def get_size() -> tuple[int, int]:
        size = os.get_terminal_size(stdin_fd)
        return size.columns, size.lines

    return stdin_read, stdout_write, get_size, stdin_fd


def _consume_task_result(task: asyncio.Future) -> None:
    """Retrieve a fire-and-forget task's outcome to suppress task warnings."""
    if not task.cancelled():
        task.exception()


async def _pump_stdin(
    ws: WSDuplex, send_lock: asyncio.Lock, stdin_read: ReadFn
) -> None:
    """Forward local stdin to the PTY as binary frames until EOF or close."""
    while True:
        data = await stdin_read()
        if not data:
            return
        try:
            async with send_lock:
                await ws.send(bytes([BIN_STDIN]) + data)
        except websockets.ConnectionClosed:
            return


async def stream_pty(
    ws: WSDuplex,
    *,
    stdin_read: ReadFn,
    stdout_write: WriteFn,
    error_write: WriteFn | None = None,
    send_lock: asyncio.Lock | None = None,
) -> tuple[int | None, list[str]]:
    """Relay frames; the exit code is ``None`` without an ``exit`` frame."""
    exit_code: int | None = None
    errors: list[str] = []

    async def read_ws() -> None:
        nonlocal exit_code
        try:
            async for message in ws:
                if isinstance(message, bytes) and message:
                    kind, payload = message[0], message[1:]
                    if kind == BIN_REPLAY:
                        if len(payload) <= 8:
                            continue
                        payload = payload[8:]
                    if payload and kind in (BIN_STDOUT, BIN_STDERR, BIN_REPLAY):
                        stdout_write(payload)
                elif isinstance(message, str):
                    try:
                        frame = json.loads(message)
                    except json.JSONDecodeError:
                        continue
                    frame_type = frame.get("type")
                    if frame_type == "exit":
                        exit_code = int(frame.get("exit_code") or 0)
                        return
                    if frame_type == "error":
                        message = str(frame.get("error") or "unknown error")
                        errors.append(message)
                        if error_write is not None:
                            # The terminal is in raw mode: CRLF, not LF.
                            error_write(message.encode() + b"\r\n")
        except websockets.ConnectionClosed:
            # Abnormal close: keep the collected exit state and errors so the
            # caller can surface them (e.g. execd's START_FAILED closes
            # without a WebSocket close frame).
            pass

    stdin_task = asyncio.create_task(
        _pump_stdin(ws, send_lock or asyncio.Lock(), stdin_read)
    )
    try:
        # Give the stdin pump a head start before the read loop drains frames.
        await asyncio.sleep(0)
        # A local stdin EOF must not cut the output stream: reap the pump and
        # keep reading until the server sends ``exit`` or closes.
        await read_ws()
    finally:
        stdin_task.cancel()
        await asyncio.gather(stdin_task, return_exceptions=True)
    return exit_code, errors


async def _connect_and_stream(
    ws_url: str,
    headers: dict[str, str],
    *,
    stdin_read: ReadFn,
    stdout_write: WriteFn,
    get_size: SizeFn,
    error_write: WriteFn | None = None,
    on_connected: Callable[[], None] | None = None,
) -> tuple[int | None, list[str]]:
    """Connect to the PTY WebSocket, drive the relay loop, handle resize."""
    async with websockets.connect(ws_url, additional_headers=headers or None) as ws:
        if on_connected is not None:
            on_connected()
        # websockets forbids concurrent sends: stdin frames and SIGWINCH
        # resize frames share one lock, and fire-and-forget resize tasks get
        # their results consumed so failures never surface as task warnings.
        send_lock = asyncio.Lock()

        async def send_frame(frame: str) -> None:
            try:
                async with send_lock:
                    await ws.send(frame)
            except websockets.ConnectionClosed:
                pass

        async def send_resize() -> None:
            try:
                cols, rows = get_size()
            except OSError:
                return
            await send_frame(json.dumps({"type": "resize", "cols": cols, "rows": rows}))

        await send_resize()
        loop = asyncio.get_running_loop()
        has_winch = hasattr(signal, "SIGWINCH")
        if has_winch:
            try:
                loop.add_signal_handler(
                    signal.SIGWINCH,
                    lambda: asyncio.ensure_future(send_resize()).add_done_callback(
                        _consume_task_result
                    ),
                )
            except NotImplementedError:  # pragma: no cover - platform guard
                has_winch = False
        try:
            return await stream_pty(
                ws,
                stdin_read=stdin_read,
                stdout_write=stdout_write,
                error_write=error_write,
                send_lock=send_lock,
            )
        finally:
            if has_winch:
                loop.remove_signal_handler(signal.SIGWINCH)


def _require_posix_tty() -> None:
    if os.name != "posix":
        raise click.ClickException(
            "osb terminal is only supported on Unix, macOS, and Linux."
        )
    if not sys.stdin.isatty() or not sys.stdout.isatty():
        raise click.ClickException("osb terminal requires an interactive TTY.")


@click.command(
    "terminal",
    help="Open an interactive terminal into a sandbox via the execd PTY.",
    epilog=(
        "The shell runs inside the sandbox; exit it (or press Ctrl-D) to leave. "
        "The remote exit code becomes this command's exit code."
    ),
)
@click.argument("sandbox_id")
@click.option("-w", "--workdir", default=None, help="Initial working directory.")
@click.pass_obj
@handle_errors
def terminal(
    obj: ClientContext,
    sandbox_id: str,
    workdir: str | None,
) -> None:
    """Open an interactive terminal into a sandbox."""
    _require_posix_tty()

    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        endpoint = sandbox.get_endpoint(DEFAULT_EXECD_PORT)
    finally:
        sandbox.close()

    headers = endpoint.build_request_headers(obj.connection_config)
    protocol = obj.connection_config.protocol
    base = _execd_base(endpoint.endpoint)
    http_base = f"{protocol}://{base}"
    ws_url = f"{_ws_scheme(protocol)}://{base}"

    timeout = timedelta(seconds=obj.resolved_config.get("request_timeout", 30))
    session_id = _create_pty_session(
        http_base, headers, workdir=workdir, timeout=timeout
    )

    stdin_read, stdout_write, get_size, stdin_fd = _posix_tty_io()

    def report_error(data: bytes) -> None:
        # CRLF line endings are added by the sender: the terminal is in raw
        # mode, where a bare LF does not return the carriage.
        sys.stderr.buffer.write(data)
        sys.stderr.buffer.flush()

    exit_code: int | None = None
    errors: list[str] = []
    teardown = False
    connected = False

    def mark_connected() -> None:
        nonlocal connected
        connected = True

    try:
        with _raw_mode(stdin_fd):
            exit_code, errors = asyncio.run(
                _connect_and_stream(
                    f"{ws_url}/pty/{session_id}/ws",
                    headers,
                    stdin_read=stdin_read,
                    stdout_write=stdout_write,
                    get_size=get_size,
                    error_write=report_error,
                    on_connected=mark_connected,
                )
            )
        teardown = exit_code is not None or bool(errors)
    except websockets.InvalidStatus as exc:
        raise click.ClickException(
            f"The execd refused the terminal connection: {exc}"
        ) from exc
    except (OSError, websockets.WebSocketException) as exc:
        raise click.ClickException(f"Terminal connection failed: {exc}") from exc
    finally:
        # Clean exit tears the session down, as does a reported failure (e.g.
        # START_FAILED: the shell never ran, so there is nothing to preserve)
        # or a WebSocket that never opened (the shell starts on the first
        # one). A lost connection on a live shell is the one case worth keeping.
        if teardown or not connected:
            _delete_pty_session(http_base, headers, session_id, timeout=timeout)

    if exit_code is None:
        if errors:
            raise click.ClickException("Shell startup failed: " + "; ".join(errors))
        sys.stderr.write(
            "Warning: the terminal connection closed before the shell exited.\n"
            f"The PTY session was kept on the sandbox: {session_id}\n"
            f"  status: GET    {http_base}/pty/{session_id}\n"
            f"  remove: DELETE {http_base}/pty/{session_id}\n"
        )
        sys.stderr.flush()
        raise click.ClickException("Terminal connection lost.")
    sys.exit(exit_code)
