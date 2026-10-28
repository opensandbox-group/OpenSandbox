# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Execute the runtime observer against real admission objects, without sockets."""
import importlib.util
import os
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

import test_credential_bound_h2_runtime as runner
from test_credential_bound_h2_lifecycle import ReadyScheduler, make_runtime, options
from credential_bound_h2 import BoundH2Server, dependency_versions_supported
from mitmproxy import connection
from mitmproxy.addons.proxyserver import Proxyserver
from mitmproxy.proxy.context import Context
from mitmproxy.proxy.layers import http as layers


class ProductionObserverTests(unittest.TestCase):
    def setUp(self):
        self.scheduler = ReadyScheduler()
        self.addCleanup(self.scheduler.close)
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        spec = importlib.util.spec_from_file_location(
            "h2_observer_under_test", Path(__file__).parent / "fixtures/h2_production_observer.py")
        self.observer = importlib.util.module_from_spec(spec)
        with mock.patch.dict(os.environ, {"H2_PRODUCTION_OBSERVATIONS": self.directory.name}):
            spec.loader.exec_module(self.observer)
        self.live = make_runtime(Proxyserver(), self.scheduler.loop)
        patch = mock.patch.object(self.observer.credential_bound, "runtime", return_value=self.live)
        patch.start()
        self.addCleanup(patch.stop)

    def test_snapshot_and_runner_cleanup_agree_on_real_runtime(self):
        state = self.observer.snapshot()
        self.assertTrue(state["runtime"])
        self.assertEqual(state["registry_connections"], 0)
        self.assertEqual(state["registry_requests"], 0)
        harness = SimpleNamespace(state=lambda: state, expect_origin_closed=mock.Mock())
        def check_once(predicate, **_kwargs):
            self.assertTrue(predicate(), state)
        with mock.patch.object(runner, "wait_for", check_once):
            runner.RuntimeHarness.expect_clean_runtime(harness)
        harness.expect_origin_closed.assert_called_once_with()

    def test_running_emits_state_after_ready_instead_of_crashing_observer_task(self):
        emitted = []
        with mock.patch.object(self.observer, "emit", side_effect=lambda kind, **data: emitted.append((kind, data))):
            self.observer.running()
            self.scheduler.ready()
            self.assertEqual([kind for kind, _ in emitted], ["observer_ready", "state"])
            self.assertTrue(emitted[1][1]["runtime"])
            self.assertFalse(self.observer._task.done())
            self.observer.done()
            self.scheduler.ready()
            self.assertTrue(self.observer._task.cancelled())

    @unittest.skipUnless(dependency_versions_supported(), "supported pinned H2 runtime only")
    def test_real_attached_adapter_snapshot_then_disconnect_cleanup(self):
        context = Context(connection.Client(peername=("127.0.0.1", 1), sockname=("127.0.0.1", 443)), options())
        context.client.tls = True
        context.client.alpn = b"h2"
        context.client.sni = "example.test"
        context.server.address = ("example.test", 443)
        token = self.live.admit_tls("example.test").token
        self.live.track(token, context.client)
        root = layers.HttpLayer(context, layers.HTTPMode.transparent)
        adapter = BoundH2Server(context.fork(), self.live, root, token)
        root.connections[context.client] = adapter
        self.live.attach_h2(context.client, adapter)
        state = self.observer.snapshot()
        self.assertEqual(state["adapters"], 1)
        self.assertEqual(state["registry_connections"], 1)
        self.assertEqual(state["connections"][0]["connection_serial"], token.serial)
        self.assertEqual(state["connections"][0]["adapter"], "BoundH2Server")
        self.assertEqual(state["connections"][0]["active"], [])
        self.live.client_disconnected(context.client)
        state = self.observer.snapshot()
        self.assertEqual(state["connections"], [])
        self.assertEqual(state["registry_connections"], 0)
        self.assertEqual(state["adapters"], 0)


if __name__ == "__main__":
    unittest.main()
