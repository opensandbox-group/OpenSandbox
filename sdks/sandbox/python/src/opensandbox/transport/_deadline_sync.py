#
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
#
import socket
import ssl
import time
from concurrent.futures import Future, TimeoutError
from threading import Thread
from typing import Any

import httpcore
import httpx

DEADLINE_EXTENSION = "opensandbox_request_deadline"


def remaining(deadline: float, timeout: float | None) -> float:
    budget = deadline - time.monotonic()
    if budget <= 0:
        raise httpcore.ReadTimeout("Request deadline exceeded")
    return min(budget, timeout) if timeout is not None else budget


class _DeadlineStream(httpcore.NetworkStream):
    def __init__(self, stream: httpcore.NetworkStream, deadline: float) -> None:
        self._stream = stream
        self._deadline = deadline

    def read(self, max_bytes: int, timeout: float | None = None) -> bytes:
        return self._stream.read(max_bytes, remaining(self._deadline, timeout))

    def write(self, buffer: bytes, timeout: float | None = None) -> None:
        self._stream.write(buffer, remaining(self._deadline, timeout))

    def start_tls(
        self,
        ssl_context: ssl.SSLContext,
        server_hostname: str | None = None,
        timeout: float | None = None,
    ) -> httpcore.NetworkStream:
        return _DeadlineStream(
            self._stream.start_tls(
                ssl_context, server_hostname, remaining(self._deadline, timeout)
            ),
            self._deadline,
        )

    def close(self) -> None:
        self._stream.close()

    def get_extra_info(self, info: str) -> Any:
        return self._stream.get_extra_info(info)


class _DeadlineBackend(httpcore.SyncBackend):
    def __init__(self, deadline: float) -> None:
        self._deadline = deadline

    def connect_tcp(
        self,
        host: str,
        port: int,
        timeout: float | None = None,
        local_address: str | None = None,
        socket_options=None,
    ) -> httpcore.NetworkStream:
        deadline = (
            min(self._deadline, time.monotonic() + timeout)
            if timeout is not None
            else self._deadline
        )
        addresses: Future[list] = Future()

        def resolve() -> None:
            try:
                addresses.set_result(
                    socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
                )
            except Exception as error:
                addresses.set_exception(error)

        Thread(target=resolve, name="opensandbox-dns", daemon=True).start()
        try:
            resolved = addresses.result(timeout=remaining(deadline, timeout))
        except TimeoutError as error:
            raise httpcore.ConnectTimeout("DNS resolution deadline exceeded") from error
        except OSError as error:
            raise httpcore.ConnectError(str(error)) from error
        last_error = httpcore.ConnectError("DNS returned no addresses")
        for _, _, _, _, address in resolved:
            try:
                stream = super().connect_tcp(
                    address[0],
                    port,
                    remaining(deadline, timeout),
                    local_address,
                    socket_options,
                )
                return _DeadlineStream(stream, self._deadline)
            except (httpcore.ConnectError, httpcore.ConnectTimeout) as error:
                last_error = error
        raise last_error


_CORE_ERRORS = {
    httpcore.ConnectTimeout: httpx.ConnectTimeout,
    httpcore.ReadTimeout: httpx.ReadTimeout,
    httpcore.WriteTimeout: httpx.WriteTimeout,
    httpcore.PoolTimeout: httpx.PoolTimeout,
    httpcore.ConnectError: httpx.ConnectError,
    httpcore.ReadError: httpx.ReadError,
    httpcore.WriteError: httpx.WriteError,
    httpcore.LocalProtocolError: httpx.LocalProtocolError,
    httpcore.RemoteProtocolError: httpx.RemoteProtocolError,
    httpcore.ProxyError: httpx.ProxyError,
    httpcore.UnsupportedProtocol: httpx.UnsupportedProtocol,
}


class DeadlineSyncTransport(httpx.BaseTransport):
    def __init__(self, inner: httpx.BaseTransport, ssl_context: ssl.SSLContext) -> None:
        self._inner = inner
        self._ssl_context = ssl_context

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        deadline = request.extensions.get(DEADLINE_EXTENSION)
        if deadline is None:
            return self._inner.handle_request(request)
        try:
            with httpcore.ConnectionPool(
                network_backend=_DeadlineBackend(deadline),
                ssl_context=self._ssl_context,
            ) as pool:
                response = pool.request(
                    request.method,
                    str(request.url),
                    headers=request.headers.raw,
                    content=request.read(),
                    extensions=request.extensions,
                )
                return httpx.Response(
                    response.status, headers=response.headers, content=response.content
                )
        except tuple(_CORE_ERRORS) as error:
            mapped = next(
                value for key, value in _CORE_ERRORS.items() if isinstance(error, key)
            )
            raise mapped(str(error), request=request) from error

    def close(self) -> None:
        self._inner.close()
