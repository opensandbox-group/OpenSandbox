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

"""Startup validation stays out of the per-ClientHello critical section."""
import json
import sys
import unittest
from unittest import mock

import test_credential_bound as fixtures


class CompiledStaticPassthroughTest(unittest.TestCase):
    def setUp(self):
        fixtures._isolate_system_modules(self)
        sys.path.insert(0, str(fixtures.MITMSCRIPTS))
        self.addCleanup(sys.path.remove, str(fixtures.MITMSCRIPTS))
        self.system, self.receiver, self.runtime = fixtures._system_with_live_runtime(
            json.dumps([f"a{i:03}.example" for i in range(256)]))

    def test_live_admissions_do_not_reparse_startup_selectors(self):
        import static_passthrough
        with mock.patch.object(static_passthrough, "parse_canonical",
                               side_effect=AssertionError("startup selector reparsed")):
            for host, port, reason in (("a255.example", 443, "static_ignore"),
                                       ("a255.example", 8443, "snapshot_missing"),
                                       ("elsewhere.example", 443, "snapshot_missing")):
                self.assertEqual(self.runtime.admit_tls(host, destination_port=port).reason, reason)
        self.assertEqual(self.receiver.registry.count, 0)

    def test_public_classifier_and_registry_reject_untrusted_inputs(self):
        from host_selectors import Selector
        from tls_decision import classify
        for value in ([Selector("a255.example", False)],
                      (Selector("BAD.example", False),),
                      (Selector("a255.example", 0),), object()):
            args = dict(identity=self.runtime.identity, sni="a255.example", ech_hidden=False,
                        static_passthrough=value, destination_port=443)
            self.assertEqual(classify(**args, view=None).reason, "invalid_input")
            self.assertEqual(self.receiver.registry.admit(**args).reason, "invalid_input")

    def test_compiled_policy_is_validated_immutable_and_not_public_input(self):
        from static_passthrough import StaticPassthrough
        from host_selectors import Selector
        from tls_decision import classify
        policy = StaticPassthrough(self.runtime.static_passthrough)
        self.assertIs(policy.selectors, self.runtime.static_passthrough)
        with self.assertRaises(AttributeError):
            policy.selectors = ()
        for value in ([Selector("a.example", False)], (Selector("BAD.example", False),)):
            with self.assertRaises(ValueError):
                StaticPassthrough(value)
        forged = object.__new__(StaticPassthrough)
        object.__setattr__(forged, "selectors", (Selector("BAD.example", False),))
        for value in (policy, forged):
            args = dict(identity=self.runtime.identity, sni="a255.example", ech_hidden=False,
                        static_passthrough=value, destination_port=443)
            self.assertEqual(classify(**args, view=None).reason, "invalid_input")
            self.assertEqual(self.receiver.registry.admit(**args).reason, "invalid_input")

    def test_equal_but_distinct_tuple_is_validated(self):
        import static_passthrough
        value = tuple(list(self.runtime.static_passthrough))
        self.assertIsNot(value, self.runtime.static_passthrough)
        with mock.patch.object(static_passthrough, "parse_canonical",
                               wraps=static_passthrough.parse_canonical) as parse:
            result = self.receiver.registry.admit(identity=self.runtime.identity,
                sni="a255.example", ech_hidden=False, static_passthrough=value,
                destination_port=443)
        self.assertEqual(result.reason, "static_ignore")
        self.assertEqual(parse.call_count, 256)

    def test_native_diagnostic_names_migration_without_values(self):
        from credential_bound import validate_live_options
        from static_passthrough import ENV
        for name in ("ignore_hosts", "allow_hosts"):
            with mock.patch.object(self.system.ctx.options, name, ["sensitive.example"], create=True):
                with self.assertRaises(SystemExit) as caught:
                    validate_live_options()
                self.assertIn(ENV, str(caught.exception))
                self.assertNotIn("sensitive.example", str(caught.exception))
