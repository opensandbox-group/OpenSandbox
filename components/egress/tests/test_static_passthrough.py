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

"""Strict launcher handoff and frozen static TLS selection."""
import json
import unittest
from pathlib import Path

import test_tls_decision as fixtures

static = fixtures.load("static_passthrough")


class StaticPassthroughTest(unittest.TestCase):
    def test_strict_handoff(self):
        self.assertEqual(static.parse_handoff("[]"), ())
        for value in (None, "", "null", "{}", '"a.example"', '[null]', '[1]',
                      '["A.example"]', '["a.example."]', '["a.example","a.example"]',
                      '["b.example","a.example"]', '["é.example"]', '["\\ud800.example"]',
                      '[] []', '["127.0.0.1"]', '["a.example:443"]'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                static.parse_handoff(value)

    def test_limits_and_immutable_validation(self):
        self.assertEqual(static.parse_handoff('[]' + ' ' * 65534), ())
        for value in ('[]' + ' ' * 65535, json.dumps([f"a{i:03}.example" for i in range(257)])):
            with self.assertRaises(ValueError):
                static.parse_handoff(value)
        value = static.parse_handoff('["*.example.com","xn--bcher-kva.example"]')
        self.assertIs(static.validate_selectors(value), value)
        for forged in ([value[0]], (fixtures.selectors.Selector("BAD.example", False),),
                       (fixtures.selectors.Selector("a.example", 0),), value + value):
            with self.assertRaises(ValueError):
                static.validate_selectors(forged)

    def test_port_and_precedence(self):
        args = dict(identity=("control", "subject"), sni="A.B.Example.com.",
                    ech_hidden=False, static_passthrough=static.parse_handoff('["*.example.com"]'), view=None)
        self.assertEqual(fixtures.tls_decision.classify(**args, destination_port=443).reason, "static_ignore")
        for port in (80, 8443):
            self.assertEqual(fixtures.tls_decision.classify(**args, destination_port=port).reason, "snapshot_missing")
        for port in (None, True, "443", 0, 65536):
            self.assertEqual(fixtures.tls_decision.classify(**args, destination_port=port).action, "deny")
        for override, reason in ((dict(identity=None), "unknown_identity"), (dict(ech_hidden=True), "ech"),
                                 (dict(sni=None), "no_sni"), (dict(sni="bad"), "invalid_sni")):
            self.assertEqual(fixtures.tls_decision.classify(**(args | override), destination_port=None).reason, reason)

    def test_shared_canonical_and_overlap_contract(self):
        data = json.loads((Path(__file__).resolve().parents[1] / "testdata/static_passthrough.json").read_text())
        for case in data["canonical"]:
            with self.subTest(name=case["name"]):
                raw = case["input"] if case["present"] else None
                if case["valid"]:
                    self.assertEqual([item.text for item in static.parse_handoff(raw)], case["canonical"])
                else:
                    with self.assertRaises(ValueError):
                        static.parse_handoff(raw)
        for case in data["overlaps"]:
            left = fixtures.selectors.parse_canonical(case["left"])
            right = fixtures.selectors.parse_canonical(case["right"])
            self.assertEqual(left.overlaps(right), case["overlap"])

    def test_pinned_clienthello_original_address_fixture(self):
        # Real dependency objects in a fresh interpreter avoid fake-module
        # contamination. This is not evidence of kernel original-dst lookup.
        import os
        import subprocess
        import sys
        code = '''
from types import SimpleNamespace
from mitmproxy import connection, ctx, options, tls
from mitmproxy.proxy.context import Context
from mitmproxy.proxy.mode_specs import ProxyMode
from credential_bound import LiveRuntime
from revision_publication import LiveReceiver
from static_passthrough import parse_handoff
from test_ech_wire import _build_client_hello
import system
ctx.options = options.Options()
ctx.log = SimpleNamespace(warn=lambda message: None)
for mode in ("transparent", "socks5"):
    owner = LiveReceiver("c", "s", max_snapshot_bytes=100000, capacity=1,
                         static_passthrough=parse_handoff('["static.example.com"]'))
    runtime = LiveRuntime(owner, ("c", "s"))
    for port in (443, 80, 8443):
        client = connection.Client(peername=("127.0.0.1", 1234), sockname=("127.0.0.1", 8080),
                                   proxy_mode=ProxyMode.parse(mode), state=connection.ConnectionState.OPEN)
        context = Context(client, ctx.options)
        context.server.address = ("192.0.2.1", port)
        assert context.server.peername is None
        hello = tls.ClientHello(_build_client_hello("static.example.com"))
        data = tls.ClientHelloData(context, hello)
        system._tls_clienthello_live(runtime, data)
        assert data.ignore_connection is (port == 443), (mode, port)
        assert not owner.registry._entries
print("transparent/socks5 ClientHello address fixture ok; no kernel claim")
'''
        env = dict(os.environ, PYTHONPATH=os.pathsep.join((str(fixtures.MITMSCRIPTS),
                                                        str(Path(__file__).resolve().parent))))
        result = subprocess.run([sys.executable, "-c", code], capture_output=True,
                                text=True, timeout=30, env=env)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
