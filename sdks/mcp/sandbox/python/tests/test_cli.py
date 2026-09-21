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

from __future__ import annotations

import sys
from typing import Any

import pytest


class _FakeServer:
    def __init__(self) -> None:
        self.calls: list[tuple[tuple[Any, ...], dict[str, Any]]] = []

    def run(self, *args: Any, **kwargs: Any) -> None:
        self.calls.append((args, kwargs))


@pytest.fixture
def fake_server(monkeypatch: pytest.MonkeyPatch) -> _FakeServer:
    from opensandbox_mcp import __main__

    server = _FakeServer()
    monkeypatch.setattr(
        __main__, "create_server", lambda connection_config=None: server
    )
    return server


def test_streamable_http_uses_mcp_defaults(
    monkeypatch: pytest.MonkeyPatch, fake_server: _FakeServer
) -> None:
    from opensandbox_mcp import __main__

    monkeypatch.setattr(
        sys,
        "argv",
        ["opensandbox-mcp", "--transport", "streamable-http"],
    )

    __main__.main()

    assert fake_server.calls == [
        (
            (),
            {
                "transport": "streamable-http",
                "host": "127.0.0.1",
                "port": 8000,
            },
        )
    ]


def test_streamable_http_forwards_host_and_port(
    monkeypatch: pytest.MonkeyPatch, fake_server: _FakeServer
) -> None:
    from opensandbox_mcp import __main__

    monkeypatch.setattr(
        sys,
        "argv",
        [
            "opensandbox-mcp",
            "--transport",
            "streamable-http",
            "--host",
            "0.0.0.0",
            "--port",
            "9000",
        ],
    )

    __main__.main()

    assert fake_server.calls == [
        (
            (),
            {
                "transport": "streamable-http",
                "host": "0.0.0.0",
                "port": 9000,
            },
        )
    ]


def test_stdio_does_not_pass_http_bind_options(
    monkeypatch: pytest.MonkeyPatch, fake_server: _FakeServer
) -> None:
    from opensandbox_mcp import __main__

    monkeypatch.setattr(
        sys,
        "argv",
        [
            "opensandbox-mcp",
            "--host",
            "0.0.0.0",
            "--port",
            "9000",
        ],
    )

    __main__.main()

    assert fake_server.calls == [
        ((), {"transport": "stdio"}),
    ]
