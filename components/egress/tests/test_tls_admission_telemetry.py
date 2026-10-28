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

"""Bounded live TLS admission events, independent of request admission."""
import json
import sys
import types
import unittest
from contextlib import nullcontext
from pathlib import Path
from typing import get_args
from unittest import mock

import test_credential_bound as fixtures

PREFIX = "credential proxy: tls-admission-v1 "


class TLSAdmissionTelemetryTest(unittest.TestCase):
    def setUp(self):
        fixtures._isolate_system_modules(self)
        sys.path.insert(0, str(fixtures.MITMSCRIPTS))
        self.addCleanup(sys.path.remove, str(fixtures.MITMSCRIPTS))
        self.system, self.receiver, self.runtime = fixtures._system_with_live_runtime()

    def events(self):
        return [message for message in self.system.ctx.log.messages if message.startswith(PREFIX)]

    def install_bound(self):
        payload = fixtures._decision_payload(
            [fixtures._binding("api", "api.example.com", "private-token")], ["private-token"], 1)
        revision = fixtures._revision(payload, 1, 1)
        self.receiver.prepare(revision, payload)
        self.receiver.commit(revision)

    def test_shared_event_contract_and_warn_level(self):
        cases = json.loads((Path(__file__).resolve().parents[1] / "testdata/tls_admission_events.json").read_text())
        for item in cases["valid"]:
            with self.subTest(item=item), mock.patch.object(self.system.ctx.log, "warn") as warn:
                self.system._record_tls_admission(**item)
                warn.assert_called_once_with(PREFIX + item["decision"] + " " + item["reason"])
        for item in cases["invalid"]:
            with self.subTest(item=item), mock.patch.object(self.system.ctx.log, "warn") as warn:
                self.system._record_tls_admission(**item)
                warn.assert_not_called()

    def test_fixture_covers_runtime_vocabulary(self):
        # Import after setUp has isolated the runtime modules. RegistryReason
        # is a union of Literal aliases, not a flat Literal of strings.
        from tls_decision import Action, Reason
        from tls_registry import RegistryAction, RegistryReason

        cases = json.loads((Path(__file__).resolve().parents[1] / "testdata/tls_admission_events.json").read_text())
        pairs = {(item["decision"], item["reason"]) for item in cases["valid"]}
        self.assertEqual(len(pairs), len(cases["valid"]), "duplicate fixture pair")
        registry_reasons = {
            reason for branch in get_args(RegistryReason) for reason in get_args(branch)
        }
        self.assertLessEqual(set(get_args(Reason)), registry_reasons)
        self.assertEqual(
            {reason for _, reason in pairs},
            registry_reasons | {"ssl_insecure", "malformed_extensions"},
        )
        self.assertEqual({decision for decision, _ in pairs}, set(get_args(RegistryAction)))
        self.assertEqual(
            set(get_args(RegistryAction)),
            (set(get_args(Action)) - {"needs_registry"}) | {"decrypt"},
        )
        self.assertEqual(pairs, {
            (decision, reason)
            for decision, reasons in self.system._TLS_ADMISSION_REASONS.items()
            for reason in reasons
        })

    def test_each_final_admission_records_once(self):
        cases = json.loads((Path(__file__).resolve().parents[1] / "testdata/tls_admission_events.json").read_text())
        for item in cases["valid"]:
            if item["reason"] in ("ssl_insecure", "malformed_extensions"):
                continue
            with self.subTest(item=item):
                self.system.ctx.log.messages.clear()
                runtime = mock.Mock()
                runtime.admit_tls.return_value = types.SimpleNamespace(
                    action=item["decision"], reason=item["reason"], token=object())
                data = fixtures._ClientHelloData("api.example.com")
                self.system._tls_clienthello_live(runtime, data)
                self.assertEqual(self.events(), [PREFIX + item["decision"] + " " + item["reason"]])
                if item["decision"] == "decrypt":
                    runtime.track.assert_called_once()
                else:
                    runtime.track.assert_not_called()
                self.assertEqual(data.ignore_connection, item["decision"] == "passthrough")
                self.assertEqual(data.client_conn.closed, item["decision"] == "deny")

    def test_pre_admission_denials_and_logging_failures(self):
        for reason in ("ssl_insecure", "malformed_extensions"):
            for broken_log in (False, True):
                with self.subTest(reason=reason, broken_log=broken_log):
                    self.system.ctx.log.messages.clear()
                    self.system.ctx.options.ssl_insecure = reason == "ssl_insecure"
                    data = fixtures._ClientHelloData("api.example.com")
                    if reason == "malformed_extensions":
                        data.client_hello.extensions = None
                    runtime = mock.Mock()
                    with mock.patch.object(self.system.ctx.log, "warn", side_effect=RuntimeError("unavailable")) if broken_log else nullcontext():
                        self.system._tls_clienthello_live(runtime, data)
                    runtime.admit_tls.assert_not_called()
                    self.assertTrue(data.client_conn.closed)
                    if not broken_log:
                        self.assertEqual(self.events(), [PREFIX + "deny " + reason])

    def test_logging_failure_preserves_final_actions(self):
        for action, reason in (("decrypt", "binding_host"), ("passthrough", "no_binding_host"), ("deny", "snapshot_missing")):
            with self.subTest(action=action):
                runtime = mock.Mock()
                runtime.admit_tls.return_value = types.SimpleNamespace(action=action, reason=reason, token=object())
                data = fixtures._ClientHelloData("api.example.com")
                with mock.patch.object(self.system.ctx.log, "warn", side_effect=RuntimeError("unavailable")):
                    self.system._tls_clienthello_live(runtime, data)
                self.assertEqual(data.ignore_connection, action == "passthrough")
                self.assertEqual(data.client_conn.closed, action == "deny")
                self.assertEqual(runtime.track.call_count, int(action == "decrypt"))

    def test_admission_exception_is_not_reclassified(self):
        runtime = mock.Mock()
        runtime.admit_tls.side_effect = RuntimeError("admission failure")
        with self.assertRaisesRegex(RuntimeError, "admission failure"):
            self.system._tls_clienthello_live(runtime, fixtures._ClientHelloData("api.example.com"))
        self.assertEqual(self.events(), [])

    def test_process_exit_is_not_swallowed(self):
        with mock.patch.object(self.system.ctx.log, "warn", side_effect=SystemExit("stop")):
            with self.assertRaises(SystemExit):
                self.system._record_tls_admission("deny", "snapshot_missing")

    def test_requests_do_not_record_and_new_handshake_does(self):
        self.install_bound()
        data = fixtures._ClientHelloData("api.example.com")
        self.system.tls_clienthello(data)
        self.assertEqual(self.events(), [PREFIX + "decrypt binding_host"])
        for version in ("HTTP/1.1", "HTTP/1.1", "HTTP/2.0", "HTTP/2.0"):
            flow = fixtures._Flow(client_conn=data.client_conn, host="api.example.com")
            flow.request.http_version = version
            self.system.requestheaders(flow)
            self.system.response(flow)
        # H2 without an instance adapter is denied by this unit fixture;
        # neither successful H1 nor rejected H2 request hooks count TLS decisions.
        self.assertEqual(self.events(), [PREFIX + "decrypt binding_host"])
        self.system.tls_clienthello(data)
        self.assertEqual(self.events(), [PREFIX + "decrypt binding_host"] * 2)

    def test_legacy_and_track_failure_do_not_change_metric_meaning(self):
        self.system._live_runtime = None
        self.system.tls_clienthello(fixtures._ClientHelloData("api.example.com"))
        self.assertEqual(self.events(), [])
        runtime = mock.Mock()
        runtime.admit_tls.return_value = types.SimpleNamespace(action="decrypt", reason="binding_host", token=object())
        runtime.track.return_value = False
        self.system._tls_clienthello_live(runtime, fixtures._ClientHelloData("api.example.com"))
        self.assertEqual(self.events(), [PREFIX + "decrypt binding_host"])


if __name__ == "__main__":
    unittest.main()
