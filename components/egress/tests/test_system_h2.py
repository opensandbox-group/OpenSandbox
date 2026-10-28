# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Pure system-hook tests; no socket, IPC, TLS, or transport-completion claim."""

from __future__ import annotations

import hashlib
import importlib.util
import json
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

MITMSCRIPTS = Path(__file__).resolve().parents[1] / "mitmscripts"
sys.path.insert(0, str(MITMSCRIPTS))
from credential_bound import LiveRuntime
from mitmproxy import connection, http
from revision_publication import LiveReceiver
from revision_receiver import Revision

spec = importlib.util.spec_from_file_location(
    "system_h2_under_test", MITMSCRIPTS / "system.py"
)
system = importlib.util.module_from_spec(spec)
spec.loader.exec_module(system)

HOST = "bound.example.com"
SECRET = "synthetic-h2-old-value"


def install(receiver, epoch=1, secret=SECRET, removed=False):
    bindings = (
        []
        if removed
        else [
            {
                "name": "synthetic",
                "match": {
                    "schemes": ["https"],
                    "hosts": [HOST],
                    "methods": ["GET"],
                    "paths": ["/v1/*"],
                },
                "headers": [{"name": "Authorization", "value": secret}],
            }
        ]
    )
    payload = json.dumps(
        {
            "version": 1,
            "vaultRevision": epoch,
            "effectivePolicyEpoch": 0,
            "interceptionMode": "credential-bound",
            "state": "active-empty" if removed else "active",
            "tlsBindingHostSelectors": [] if removed else [HOST],
            "fullRenderedBindings": bindings,
            "redactions": [] if removed else [secret],
        },
        separators=(",", ":"),
    ).encode()
    revision = Revision(
        "control", "subject", epoch, epoch, 0, hashlib.sha256(payload).hexdigest()
    )
    receiver.prepare(revision, payload)
    receiver.commit(revision)
    return revision


class HookAdapter:
    """Only the identity/notification contract is stubbed in this hook unit."""

    def __init__(self, token):
        self.token = token
        self.http_layer = SimpleNamespace(
            context=SimpleNamespace(
                server=SimpleNamespace(address=("203.0.113.42", 443), tls=True)
            )
        )
        self.pending = {}
        self.bound = []
        self.refused = []
        self.done = []
        self.failed = False
        self.closed_requests = set()

    def observe(self, flow, sid=1, headers=None):
        identity = SimpleNamespace(
            stream_id=sid,
            request_end_stream=True,
            headers=tuple(
                headers
                if headers is not None
                else (
                    (b":method", b"GET"),
                    (b":scheme", b"https"),
                    (b":authority", HOST.encode()),
                    (b":path", b"/v1/data"),
                )
            ),
        )
        self.pending[id(flow.request)] = (flow.request, identity)
        return identity

    def consume_identity(self, request):
        item = self.pending.pop(id(request), None)
        return item[1] if item is not None and item[0] is request else None

    def request_is_closed(self, request):
        return id(request) in self.closed_requests

    def bind_request(self, identity, flow, handle):
        assert identity.stream_id == handle.client_stream_id
        assert handle.connection is self.token
        self.bound.append((flow, handle))

    def refuse(self, identity, flow):
        self.refused.append(identity.stream_id)
        flow.kill()

    def application_done(self, flow):
        self.done.append(flow)

    def fail_closed(self):
        self.failed = True


class HookRuntime(LiveRuntime):
    def __init__(self, receiver):
        super().__init__(receiver, ("control", "subject"))
        self.adapter = None
        self.closed = []
        self.acquisitions = 0

    def h2_for(self, client):
        return self.adapter

    def _close_established(self, client):
        self.closed.append(client)
        return True

    def acquire_request(self, token, *, client_stream_id=None):
        self.acquisitions += 1
        return self.registry.acquire_request(token, client_stream_id=client_stream_id)


class SystemH2Hooks(unittest.TestCase):
    def setUp(self):
        self.receiver = LiveReceiver(
            "control",
            "subject",
            max_snapshot_bytes=100_000,
            capacity=4,
            request_capacity=4,
        )
        install(self.receiver)
        self.runtime = HookRuntime(self.receiver)
        self.client = connection.Client(
            peername=("127.0.0.1", 1), sockname=("127.0.0.1", 443)
        )
        self.client.alpn = b"h2"
        self.client.tls = True
        self.client.sni = HOST
        self.token = self.runtime.admit_tls(HOST).token
        self.runtime.track(self.token, self.client)
        self.adapter = HookAdapter(self.token)
        self.runtime.adapter = self.adapter
        self.log = []
        context = SimpleNamespace(
            options=SimpleNamespace(ssl_insecure=False),
            log=SimpleNamespace(warn=self.log.append, info=self.log.append),
        )
        self.context = context
        self.patches = [
            patch.object(system, "ctx", context),
            patch.object(system, "_live_runtime", self.runtime),
            patch.object(system, "_live_vault_cache", None),
        ]
        for p in self.patches:
            p.start()
            self.addCleanup(p.stop)

    def flow(self, sid=1, headers=None):
        server = connection.Server(address=("203.0.113.42", 443))
        flow = http.HTTPFlow(self.client, server)
        flow.live = True
        flow.request = http.Request.make("GET", f"https://{HOST}/v1/data")
        flow.request.http_version = "HTTP/2.0"
        flow.request.authority = HOST
        flow.request.data.host = "203.0.113.42"
        self.adapter.observe(flow, sid, headers)
        return flow

    def test_single_admission_pins_snapshot_and_response_redaction(self):
        flow = self.flow()
        system.requestheaders(flow)
        handle = flow.metadata[system.FLOW_REQUEST_HANDLE_KEY]
        self.assertEqual(handle.client_stream_id, 1)
        self.assertEqual(self.runtime.acquisitions, 1)
        self.assertEqual(flow.request.headers["authorization"], SECRET)
        install(self.receiver, 2, "synthetic-h2-new-value")
        flow.response = http.Response.make(200, b"", {"X-Echo": SECRET})
        system.responseheaders(flow)
        self.assertNotIn(SECRET, flow.response.headers["x-echo"])
        system.response(flow)
        self.assertIs(flow.metadata[system.FLOW_REQUEST_HANDLE_KEY], handle)
        self.assertEqual(self.runtime.registry.request_count, 1)
        self.assertIsNotNone(self.runtime.registry.drain_state(self.token).deadline)
        self.assertEqual(self.adapter.done, [flow])
        newer = self.flow(3)
        system.requestheaders(newer)
        self.assertEqual(
            newer.request.headers["authorization"], "synthetic-h2-new-value"
        )
        self.assertEqual(handle.snapshot.revision.decision_epoch, 1)

    def test_raw_invalid_scheme_cannot_use_plain_http_shortcut(self):
        raw = [
            (b":method", b"GET"),
            (b":scheme", b"http"),
            (b":authority", HOST.encode()),
        ]
        flow = self.flow(headers=raw)
        flow.request.scheme = "http"
        system.requestheaders(flow)
        self.assertTrue(self.adapter.failed)
        self.assertIsNotNone(flow.error)
        self.assertNotIn("authorization", flow.request.headers)
        self.assertEqual(self.runtime.acquisitions, 1)

    def test_authority_matrix(self):
        invalid = [
            b"",
            b"other.example.com",
            b"127.0.0.1",
            b"[::1]",
            b"user@bound.example.com",
            b"bound.example.com:80",
            b"bound.example.com:0443",
            b"bound.example.com%2e",
            b"bound.example.com ",
            b"bound.example.com,other.example.com",
            b"\xff",
        ]
        for authority in invalid:
            with self.subTest(authority=authority):
                flow = self.flow()
                identity = SimpleNamespace(
                    headers=(
                        (b":method", b"GET"),
                        (b":scheme", b"https"),
                        (b":authority", authority),
                    )
                )
                actual = system._live_h2_authority(flow, identity, self.adapter)
                # Cross-SNI host parses but is rejected by the hook below.
                if authority == b"other.example.com":
                    self.assertEqual(actual, "other.example.com")
                else:
                    self.assertIsNone(actual)
        for authority in (b"BOUND.EXAMPLE.COM.", b"bound.example.com:443"):
            identity = SimpleNamespace(
                headers=(
                    (b":method", b"GET"),
                    (b":scheme", b"https"),
                    (b":authority", authority),
                )
            )
            self.assertEqual(
                system._live_h2_authority(self.flow(), identity, self.adapter), HOST
            )

    def test_duplicate_missing_host_conflict_connect_and_real_port_deny(self):
        base = [
            (b":method", b"GET"),
            (b":scheme", b"https"),
            (b":authority", HOST.encode()),
        ]
        cases = [
            base + [(b":authority", HOST.encode())],
            base[:2],
            base + [(b"host", b"other.example.com")],
            base + [(b"host", HOST.encode()), (b"host", HOST.encode())],
            [(b":method", b"CONNECT")] + base[1:],
            base + [(b":protocol", b"websocket")],
        ]
        for headers in cases:
            with self.subTest(headers=headers):
                self.assertIsNone(
                    system._live_h2_authority(
                        self.flow(),
                        SimpleNamespace(headers=tuple(headers)),
                        self.adapter,
                    )
                )
        self.adapter.http_layer.context.server.address = ("203.0.113.42", 8443)
        self.assertIsNone(
            system._live_h2_authority(
                self.flow(), SimpleNamespace(headers=tuple(base)), self.adapter
            )
        )

    def test_cross_authority_out_of_scope_path_still_closes(self):
        flow = self.flow(
            headers=[
                (b":method", b"GET"),
                (b":scheme", b"https"),
                (b":authority", b"other.example.com"),
            ]
        )
        flow.request.path = "/not-a-binding"
        system.requestheaders(flow)
        self.assertTrue(self.adapter.failed)
        self.assertNotIn("authorization", flow.request.headers)

    def test_post_removal_uses_marked_refusal_without_response(self):
        flow = self.flow()
        install(self.receiver, 2, removed=True)
        system.requestheaders(flow)
        self.assertEqual(self.adapter.refused, [1])
        self.assertIsNone(flow.response)
        self.assertFalse(self.adapter.failed)
        self.assertEqual(self.runtime.registry.request_count, 0)
        self.assertEqual(
            self.runtime.registry.drain_state(self.token).frozen_last_stream_id, 0
        )

    def test_missing_adapter_or_identity_closes_without_injection(self):
        flow = self.flow()
        self.runtime.adapter = None
        system.requestheaders(flow)
        self.assertEqual(self.runtime.closed, [self.client])
        self.assertNotIn("authorization", flow.request.headers)
        self.assertEqual(self.runtime.acquisitions, 0)
        self.runtime.adapter = self.adapter
        second = self.flow(3)
        self.adapter.pending.clear()
        system.requestheaders(second)
        self.assertTrue(self.adapter.failed)
        self.assertEqual(self.runtime.acquisitions, 0)

    def test_legal_reset_late_hook_does_not_close_other_streams(self):
        flow = self.flow()
        self.adapter.pending.clear()
        self.adapter.closed_requests.add(id(flow.request))
        system.requestheaders(flow)
        self.assertIsNotNone(flow.error)
        self.assertFalse(self.adapter.failed)
        self.assertEqual(self.runtime.acquisitions, 0)

    def test_snapshot_validation_error_keeps_handle_for_teardown(self):
        flow = self.flow()
        with patch.object(
            system,
            "_live_snapshot_vault",
            side_effect=system.ActiveVaultLookupError("invalid"),
        ):
            system.requestheaders(flow)
        self.assertTrue(self.adapter.failed)
        self.assertEqual(self.runtime.registry.request_count, 1)
        self.assertNotIn("authorization", flow.request.headers)
        system.error(flow)
        self.assertEqual(self.adapter.done, [flow])
        self.assertEqual(self.runtime.registry.request_count, 1)

    def test_ascii_idna_authority_selects_ascii_binding_without_target_rewrite(self):
        with patch(__name__ + ".HOST", "xn--bcher-kva.example"):
            install(self.receiver, 2)
            self.client.sni = HOST
            self.token = self.runtime.admit_tls(HOST).token
            self.runtime.track(self.token, self.client)
            self.adapter.token = self.token
            flow = self.flow()
            self.assertEqual(flow.request.pretty_host, "bücher.example")
            system.requestheaders(flow)
            self.assertFalse(self.adapter.failed)
            self.assertEqual(flow.request.headers["authorization"], SECRET)
            self.assertEqual(system._request_host(flow), HOST)
            self.assertEqual(flow.request.host, "203.0.113.42")

    def test_duplicate_hook_cannot_acquire_a_second_snapshot(self):
        flow = self.flow()
        system.requestheaders(flow)
        first = flow.metadata[system.FLOW_REQUEST_HANDLE_KEY]
        install(self.receiver, 2, "synthetic-newer")
        system.requestheaders(flow)
        self.assertEqual(self.runtime.acquisitions, 1)
        self.assertIs(flow.metadata[system.FLOW_REQUEST_HANDLE_KEY], first)
        self.assertTrue(self.adapter.failed)

    def test_unsupported_interpreter_denies_before_adapter_or_request_admission(self):
        self.runtime.adapter = None
        data = SimpleNamespace(layer=None, context=SimpleNamespace(
            client=self.client, server=SimpleNamespace(via=None),
            options=SimpleNamespace(connection_strategy="lazy")))
        with patch("credential_bound_h2.sys.version_info", (3, 12, 0)):
            system.next_layer(data)
        self.assertEqual(self.runtime.closed, [self.client])
        self.assertIsNone(data.layer)
        self.assertEqual(self.runtime.acquisitions, 0)
        self.assertEqual(self.runtime.registry.request_count, 0)
        # TLS admission precedes ALPN; normal disconnect reclaims that slot.
        self.runtime.client_disconnected(self.client)
        self.assertEqual(self.runtime.registry.count, 0)

    def test_genuine_cleartext_h2_is_not_credential_admitted(self):
        flow = self.flow()
        self.runtime.adapter = None
        self.runtime.client_disconnected(self.client)
        self.client.tls = False
        self.client.alpn = None
        flow.request.scheme = "http"
        flow.request.port = 80
        system.requestheaders(flow)
        self.assertEqual(self.runtime.acquisitions, 0)
        self.assertEqual(self.runtime.closed, [])
        self.assertIsNone(flow.error)
        self.assertIsNone(flow.response)
        self.assertNotIn("authorization", flow.request.headers)

    def test_untracked_tls_h2_cannot_bypass_with_http_scheme(self):
        flow = self.flow()
        self.runtime.adapter = None
        self.runtime.client_disconnected(self.client)
        flow.request.scheme = "http"
        system.requestheaders(flow)
        self.assertEqual(self.runtime.closed, [self.client])
        self.assertEqual(self.runtime.acquisitions, 0)
        self.assertIsNotNone(flow.error)

    def test_known_chained_h2_rejected_before_selector_or_attachment(self):
        data = SimpleNamespace(
            layer=None,
            context=SimpleNamespace(
                client=self.client,
                server=SimpleNamespace(via=("http", ("127.0.0.1", 8080))),
                options=SimpleNamespace(connection_strategy="lazy"),
            ),
        )
        system.next_layer(data)
        self.assertEqual(self.runtime.closed, [self.client])
        self.assertIsNone(data.layer)
        self.assertEqual(self.runtime.acquisitions, 0)

    def test_h1_next_layer_is_unchanged_with_chained_proxy(self):
        self.client.alpn = b"http/1.1"
        data = SimpleNamespace(
            layer=None,
            context=SimpleNamespace(
                client=self.client,
                server=SimpleNamespace(via=("http", ("127.0.0.1", 8080))),
            ),
        )
        system.next_layer(data)
        self.assertEqual(self.runtime.closed, [])
        self.assertIsNone(data.layer)

    def test_known_chained_proxy_h2_denies_before_admission(self):
        flow = self.flow()
        flow.server_conn.via = ("http", ("127.0.0.1", 8080))
        system.requestheaders(flow)
        self.assertTrue(self.adapter.failed)
        self.assertEqual(self.runtime.acquisitions, 0)
        self.assertNotIn("authorization", flow.request.headers)

    def test_ssl_insecure_does_not_admit(self):
        self.context.options.ssl_insecure = True
        flow = self.flow()
        system.requestheaders(flow)
        self.assertTrue(self.adapter.failed)
        self.assertEqual(self.runtime.acquisitions, 0)
        self.assertNotIn("authorization", flow.request.headers)


if __name__ == "__main__":
    unittest.main()
