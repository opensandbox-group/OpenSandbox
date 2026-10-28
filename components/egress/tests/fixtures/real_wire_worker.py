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

"""Real-mitmproxy wire checks for the live credential-bound addon.

This module is *not* a unittest discovery target (the file name does not
match ``test_*``): the discovered tests spawn it in a fresh subprocess so
the fake ``mitmproxy`` package injected into ``sys.modules`` by the unit
harness cannot contaminate real library parsing. Each mode prints one line
per check and exits non-zero on the first failure.

Usage: ``real_wire_worker.py {ech-parse|h1-parse}``
"""

from __future__ import annotations

import hashlib
import importlib.util
import json
import sys
import types
from pathlib import Path

MITMSCRIPTS = Path(__file__).resolve().parents[2] / "mitmscripts"
CONTROL = "wire-control"
SUBJECT = "wire-subject"
BOUND = "bound.example.com"
PUNYCODE = "xn--nxasmq6b.example"
ECH_EXTENSION_TYPE = 0xFE0D


def _build_client_hello(sni: str, extra_extensions: tuple = ()) -> bytes:
    """Minimal TLS 1.2 ClientHello handshake body (no record wrapper)."""
    name = sni.encode("ascii")
    entry = b"\x00" + len(name).to_bytes(2, "big") + name
    sni_body = len(entry).to_bytes(2, "big") + entry
    extensions = (0).to_bytes(2, "big") + len(sni_body).to_bytes(2, "big") + sni_body
    groups = b"\x00\x04\x00\x17\x00\x18"
    extensions += (0x000A).to_bytes(2, "big") + len(groups).to_bytes(2, "big") + groups
    formats = b"\x01\x00"
    extensions += (0x000B).to_bytes(2, "big") + len(formats).to_bytes(2, "big") + formats
    sigalgs = b"\x00\x04\x04\x01\x05\x01"
    extensions += (0x000D).to_bytes(2, "big") + len(sigalgs).to_bytes(2, "big") + sigalgs
    for extension_type, payload in extra_extensions:
        extensions += (
            extension_type.to_bytes(2, "big")
            + len(payload).to_bytes(2, "big")
            + payload
        )
    return (
        b"\x03\x03"
        + b"\x11" * 32
        + b"\x00"
        + (2).to_bytes(2, "big") + b"\xc0\x2f"
        + b"\x01\x00"
        + len(extensions).to_bytes(2, "big") + extensions
    )


class _Log:
    def __init__(self) -> None:
        self.messages: list[str] = []

    def warn(self, message: str) -> None:
        self.messages.append(message)

    def info(self, message: str) -> None:
        self.messages.append(message)


class _ClientConn:
    _counter = 0

    def __init__(self, sni: str | None = None) -> None:
        _ClientConn._counter += 1
        self.id = f"wire-client-{_ClientConn._counter}"
        self.sni = sni
        self.peername = ("127.0.0.1", 40000)
        self.closed = False
        self.state = "open"


def _load_system_module() -> object:
    spec = importlib.util.spec_from_file_location(
        "opensandbox_wire_system", MITMSCRIPTS / "system.py"
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _decision_payload(hosts: list[str]) -> bytes:
    bindings = [
        {
            "name": f"api-{index}",
            "match": {
                "schemes": ["https"],
                "hosts": [host],
                "methods": ["GET"],
                "paths": ["/v1/*"],
            },
            "headers": [{"name": "Private-Token", "value": "wire-secret"}],
        }
        for index, host in enumerate(hosts)
    ]
    return json.dumps(
        {
            "version": 1,
            "vaultRevision": 1,
            "effectivePolicyEpoch": 0,
            "interceptionMode": "credential-bound",
            "state": "active" if bindings else "active-empty",
            "tlsBindingHostSelectors": sorted(set(hosts)),
            "fullRenderedBindings": bindings,
            "redactions": ["wire-secret"],
        },
        separators=(",", ":"),
    ).encode()


def _live_system(hosts: list[str] | None = None) -> tuple[object, object]:
    """Load system.py against the real mitmproxy package and install a live
    runtime snapshot binding the given hosts."""
    sys.path.insert(0, str(MITMSCRIPTS))
    system = _load_system_module()
    import mitmproxy.ctx as real_ctx

    real_ctx.options = types.SimpleNamespace(ignore_hosts=[], ssl_insecure=False)
    real_ctx.log = _Log()
    import credential_bound
    from revision_publication import LiveReceiver
    from revision_receiver import Revision

    receiver = LiveReceiver(
        CONTROL, SUBJECT, max_snapshot_bytes=1 << 20, capacity=64,
        request_capacity=256, drain_timeout_seconds=1,
    )
    payload = _decision_payload(hosts or [BOUND])
    identity = Revision(CONTROL, SUBJECT, 1, 1, 0, hashlib.sha256(payload).hexdigest())
    receiver.prepare(identity, payload)
    receiver.commit(identity)
    runtime = credential_bound.LiveRuntime(receiver, identity=(CONTROL, SUBJECT))
    system._live_runtime = runtime
    return system, runtime


def _hello_data(client_hello: object, conn: _ClientConn) -> object:
    return types.SimpleNamespace(
        client_hello=client_hello,
        context=types.SimpleNamespace(client=conn),
        ignore_connection=False,
    )


def run_ech_parse() -> int:
    from mitmproxy.tls import ClientHello

    system, runtime = _live_system([BOUND])

    # ECH extension on a bound outer SNI: opaque pass-through, no token.
    raw = _build_client_hello(BOUND, [(ECH_EXTENSION_TYPE, b"\x01\x02\x03")])
    hello = ClientHello(raw)
    assert hello.sni == BOUND
    assert (ECH_EXTENSION_TYPE, b"\x01\x02\x03") in hello.extensions
    conn = _ClientConn(BOUND)
    data = _hello_data(hello, conn)
    system.tls_clienthello(data)
    assert data.ignore_connection is True
    assert conn.closed is False
    assert runtime.token_for(conn) is None
    print("ech-opaque ok")

    # Identical hello without ECH: decrypted and tracked.
    raw = _build_client_hello(BOUND, [(0x0010, b"\x00\x03\x02h2")])
    hello = ClientHello(raw)
    conn = _ClientConn(BOUND)
    data = _hello_data(hello, conn)
    system.tls_clienthello(data)
    assert data.ignore_connection is False
    assert conn.closed is False
    assert runtime.token_for(conn) is not None
    print("ech-absent-decrypt ok")

    # Valid hello with a real empty-extension parse result means no ECH.
    hello = ClientHello(_build_client_hello(BOUND))
    assert all(ext[0] != ECH_EXTENSION_TYPE for ext in hello.extensions)
    assert system._clienthello_ech_hidden(hello) is False
    conn = _ClientConn(BOUND)
    data = _hello_data(hello, conn)
    system.tls_clienthello(data)
    assert data.ignore_connection is False
    assert runtime.token_for(conn) is not None
    print("ech-empty-list ok")
    return 0


def _admitted_conn(runtime: object, sni: str) -> _ClientConn:
    conn = _ClientConn(sni)
    result = runtime.admit_tls(sni)
    assert result.token is not None, f"admission refused for {sni}"
    assert runtime.track(result.token, conn)
    return conn


def _real_flow(request: object, conn: _ClientConn) -> object:
    killed: list[bool] = []
    return types.SimpleNamespace(
        request=request,
        client_conn=conn,
        server_conn=types.SimpleNamespace(),
        metadata={},
        response=None,
        error=None,
        live=True,
        killable=True,
        killed=killed,
        kill=lambda: killed.append(True),
    )


def _request_from_head(lines: list[bytes]) -> object:
    """Parse raw head lines like mitmproxy's HTTP/1 layer, then emulate
    transparent-mode destination data WITHOUT the host setters (matching
    mitmproxy/proxy mode which overwrites data.host/data.port/scheme)."""
    from mitmproxy.net.http.http1.read import read_request_head

    request = read_request_head(lines)
    request.data.host = b"10.9.8.7"  # transparent destination may be an IP
    request.data.port = 443
    request.data.scheme = b"https"
    return request


def run_h1_parse() -> int:
    system, runtime = _live_system([BOUND, PUNYCODE])

    def drive(lines: list[bytes], sni: str) -> object:
        conn = _admitted_conn(runtime, sni)
        request = _request_from_head(lines)
        flow = _real_flow(request, conn)
        system.requestheaders(flow)
        return flow

    def injected(flow: object) -> bool:
        return flow.request.headers.get("Private-Token") == "wire-secret"

    def denied(flow: object) -> bool:
        return flow.response is not None or flow.killed

    # -- allowed identities -------------------------------------------------
    ok_cases = [
        ("origin-form", BOUND,
         [b"GET /v1/data HTTP/1.1", b"Host: " + BOUND.encode()]),
        ("absolute-form-explicit-443", BOUND,
         [b"GET https://" + BOUND.encode() + b":443/v1/data HTTP/1.1",
          b"Host: " + BOUND.encode()]),
        ("host-explicit-443", BOUND,
         [b"GET /v1/data HTTP/1.1", b"Host: " + BOUND.encode() + b":443"]),
        ("uppercase-root-dot", BOUND,
         [b"GET /v1/data HTTP/1.1",
          b"Host: " + BOUND.upper().encode() + b"."]),
        ("punycode", PUNYCODE,
         [b"GET /v1/data HTTP/1.1", b"Host: " + PUNYCODE.encode()]),
    ]
    for name, sni, lines in ok_cases:
        flow = drive(lines, sni)
        assert not denied(flow), f"{name}: unexpectedly denied"
        assert injected(flow), f"{name}: expected credential injection"
        print(f"h1-allow-{name} ok")

    # -- denied identities ---------------------------------------------------
    deny_cases = [
        ("host-wrong-host", BOUND,
         [b"GET /v1/data HTTP/1.1", b"Host: other.example.com"]),
        ("host-wrong-port", BOUND,
         [b"GET /v1/data HTTP/1.1", b"Host: " + BOUND.encode() + b":444"]),
        ("absolute-wrong-host", BOUND,
         [b"GET https://other.example.com:443/v1/data HTTP/1.1",
          b"Host: " + BOUND.encode()]),
        ("absolute-wrong-port", BOUND,
         [b"GET https://" + BOUND.encode() + b":444/v1/data HTTP/1.1",
          b"Host: " + BOUND.encode()]),
        ("absolute-host-conflict", BOUND,
         [b"GET https://other.example.com/v1/data HTTP/1.1",
          b"Host: " + BOUND.encode()]),
        ("host-duplicate", BOUND,
         [b"GET /v1/data HTTP/1.1", b"Host: " + BOUND.encode(),
          b"Host: " + BOUND.encode()]),
        ("host-missing", BOUND,
         [b"GET /v1/data HTTP/1.1"]),
        ("host-malformed-space", BOUND,
         [b"GET /v1/data HTTP/1.1", b"Host: " + BOUND.encode() + b" ."]),
        ("host-malformed-userinfo", BOUND,
         [b"GET /v1/data HTTP/1.1", b"Host: user@" + BOUND.encode()]),
        ("host-huge-port", BOUND,
         [b"GET /v1/data HTTP/1.1",
          b"Host: " + BOUND.encode() + b":" + b"9" * 6000]),
        ("cross-authority-out-of-scope-path", BOUND,
         [b"GET /health HTTP/1.1", b"Host: other.example.com"]),
        ("version-http19", BOUND,
         [b"GET /v1/data HTTP/1.9", b"Host: " + BOUND.encode()]),
    ]
    for name, sni, lines in deny_cases:
        flow = drive(lines, sni)
        assert denied(flow), f"{name}: expected denial"
        assert not injected(flow), f"{name}: credentials must not be injected"
        if flow.response is not None:
            assert flow.response.status_code == 403, (
                f"{name}: expected 403, got {flow.response.status_code}"
            )
        print(f"h1-deny-{name} ok")
    return 0


def main() -> int:
    modes = {"ech-parse": run_ech_parse, "h1-parse": run_h1_parse}
    if len(sys.argv) != 2 or sys.argv[1] not in modes:
        print(f"usage: {sys.argv[0]} {{{'|'.join(modes)}}}", file=sys.stderr)
        return 2
    try:
        return modes[sys.argv[1]]()
    except Exception as exc:  # noqa: BLE001 - worker reports failure via rc
        print(f"FAILED {sys.argv[1]}: {exc!r}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
