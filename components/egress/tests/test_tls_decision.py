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

"""Pure TLS classification over validated OSEP-0023 snapshots."""

import hashlib
import importlib.util
import json
import sys
import unittest
from pathlib import Path

MITMSCRIPTS = Path(__file__).resolve().parents[1] / "mitmscripts"


def load(name):
    if name in sys.modules:
        return sys.modules[name]
    spec = importlib.util.spec_from_file_location(name, MITMSCRIPTS / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def reload_module(name):
    spec = importlib.util.spec_from_file_location(name, MITMSCRIPTS / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


receiver = load("revision_receiver")
selectors = load("host_selectors")
previous_validator = sys.modules.get("decision_snapshot")
reload_module("decision_snapshot")
load("static_passthrough")
tls_decision = load("tls_decision")
if previous_validator is None:
    del sys.modules["decision_snapshot"]
else:
    sys.modules["decision_snapshot"] = previous_validator
_DEFAULT = object()


def binding(name, schemes, hosts, *, secret=""):
    headers = [] if not secret else [{"name": "Private-Token", "value": secret}]
    return {
        "name": name,
        "match": {
            "schemes": schemes,
            "hosts": hosts,
            "methods": ["GET"],
            "paths": ["/api/*"],
        },
        "headers": headers,
    }


class TLSDecisionTest(unittest.TestCase):
    identity = ("control-a", "subject-a")

    def payload(
        self, *, state="active", host="api.example.com", secret="hidden-secret"
    ):
        bindings = (
            []
            if host is None
            else [binding("binding-a", ["https"], [host], secret=secret)]
        )
        return {
            "version": 1,
            "vaultRevision": 7 if bindings else 0,
            "effectivePolicyEpoch": 11 if bindings else 0,
            "interceptionMode": "credential-bound",
            "state": state,
            "tlsBindingHostSelectors": [] if host is None else [host],
            "fullRenderedBindings": bindings,
            "redactions": [] if host is None else [secret],
        }

    def snapshot(self, payload=None, *, identity=None):
        payload = self.payload() if payload is None else payload
        raw = json.dumps(payload, separators=(",", ":")).encode()
        control, subject = self.identity if identity is None else identity
        revision = receiver.Revision(
            control,
            subject,
            1,
            payload["vaultRevision"],
            payload["effectivePolicyEpoch"],
            hashlib.sha256(raw).hexdigest(),
        )
        return receiver.Snapshot(revision, raw)

    def compile(self, payload=None, *, identity=None):
        return tls_decision.compile_view(self.snapshot(payload, identity=identity))

    def classify(
        self,
        *,
        sni="api.example.com",
        identity=_DEFAULT,
        ech=False,
        static=(),
        view=_DEFAULT,
    ):
        return tls_decision.classify(
            identity=self.identity if identity is _DEFAULT else identity,
            sni=sni,
            ech_hidden=ech,
            static_passthrough=static,
            destination_port=443,
            view=self.compile() if view is _DEFAULT else view,
        )

    def test_compiled_view_retains_only_revision_metadata_and_selectors(self):
        snapshot = self.snapshot()
        view = tls_decision.compile_view(snapshot)
        self.assertEqual(view.revision, snapshot.revision)
        self.assertEqual(
            view.selectors, (selectors.parse_canonical("api.example.com"),)
        )
        self.assertNotIn(snapshot.payload, repr(view).encode())
        self.assertNotIn("hidden-secret", repr(view))
        with self.assertRaises((AttributeError, TypeError)):
            view.selectors = ()

    def test_exact_and_wildcard_host_matching_reuses_canonical_selector_rules(self):
        exact = self.compile()
        exact_match = self.classify(view=exact)
        self.assertEqual(
            (exact_match.action, exact_match.reason),
            ("needs_registry", "binding_host"),
        )
        wildcard_payload = self.payload(host="*.example.com")
        wildcard = self.compile(wildcard_payload)
        for sni in ("API.EXAMPLE.COM", "api.example.com.", "one.api.example.com"):
            with self.subTest(sni=sni):
                result = self.classify(sni=sni, view=wildcard)
                self.assertEqual(
                    (result.action, result.reason),
                    ("needs_registry", "binding_host"),
                )
        unmatched = self.classify(sni="example.com", view=wildcard)
        self.assertEqual(
            (unmatched.action, unmatched.reason),
            ("passthrough", "no_binding_host"),
        )

    def test_ascii_idna_wire_name_can_match(self):
        view = self.compile(self.payload(host="xn--bcher-kva.example"))
        matched = self.classify(sni="XN--BCHER-KVA.EXAMPLE.", view=view)
        self.assertEqual(
            (matched.action, matched.reason),
            ("needs_registry", "binding_host"),
        )
        invalid = self.classify(sni="bücher.example", view=view)
        self.assertEqual(
            (invalid.action, invalid.reason), ("deny", "invalid_sni")
        )

    def test_acknowledged_empty_is_distinct_from_missing_view(self):
        empty = self.compile(self.payload(state="active-empty", host=None))
        self.assertEqual(self.classify(view=empty).action, "passthrough")
        self.assertEqual(self.classify(view=empty).reason, "no_binding_host")
        missing = self.classify(view=None)
        self.assertEqual((missing.action, missing.reason), ("deny", "snapshot_missing"))

    def test_unknown_identity_ech_no_sni_and_static_have_required_priority(self):
        view = self.compile()
        cases = (
            (
                {"identity": None, "ech": True, "sni": None},
                ("deny", "unknown_identity"),
            ),
            ({"ech": True, "sni": None}, ("passthrough", "ech")),
            ({"sni": None}, ("passthrough", "no_sni")),
            (
                {
                    "sni": "api.example.com",
                    "static": (selectors.parse_canonical("api.example.com"),),
                },
                ("passthrough", "static_ignore"),
            ),
        )
        for arguments, expected in cases:
            with self.subTest(arguments=arguments):
                result = self.classify(view=view, **arguments)
                self.assertEqual((result.action, result.reason), expected)

    def test_non_boolean_ech_flag_is_denied_after_identity_check(self):
        view = self.compile()
        result = self.classify(ech="false", view=view)
        self.assertEqual((result.action, result.reason), ("deny", "invalid_input"))
        self.assertNotIn("false", repr(result))

        unknown = self.classify(identity=None, ech="false", view=view)
        self.assertEqual((unknown.action, unknown.reason), ("deny", "unknown_identity"))

    def test_invalid_static_selectors_are_denied_after_ech_and_no_sni(self):
        view = self.compile()
        malformed = ("api.example.com",)
        invalid = self.classify(static=malformed, view=view)
        self.assertEqual((invalid.action, invalid.reason), ("deny", "invalid_input"))
        self.assertNotIn("api.example.com", repr(invalid))

        ech = self.classify(
            ech=True, sni="api.example.com", static=malformed, view=view
        )
        self.assertEqual((ech.action, ech.reason), ("passthrough", "ech"))

        no_sni = self.classify(sni=None, static=malformed, view=view)
        self.assertEqual((no_sni.action, no_sni.reason), ("passthrough", "no_sni"))

    def test_static_selector_requires_exact_field_types(self):
        malformed = (selectors.Selector("example.com", 1),)
        result = self.classify(static=malformed)
        self.assertEqual((result.action, result.reason), ("deny", "invalid_input"))
        self.assertNotIn("example.com", repr(result))

    def test_invalid_sni_is_denied_before_snapshot_routing(self):
        for sni in (
            "bücher.example",
            "127.0.0.1",
            "example..com",
            "-bad.example",
            "bad.example..",
        ):
            with self.subTest(sni=sni):
                result = self.classify(sni=sni, view=None)
                self.assertEqual(
                    (result.action, result.reason), ("deny", "invalid_sni")
                )

    def test_foreign_generation_is_denied_without_echoing_identity(self):
        foreign = self.compile(identity=("foreign-control", "foreign-subject"))
        result = self.classify(view=foreign)
        self.assertEqual(
            (result.action, result.reason), ("deny", "generation_mismatch")
        )
        self.assertNotIn("foreign", repr(result))

    def test_malformed_snapshot_fails_with_fixed_sanitized_error(self):
        payload = self.payload()
        payload["tlsBindingHostSelectors"] = ["other.example.com"]
        secret_bearing = self.snapshot(payload)
        with self.assertRaises(tls_decision.TLSDecisionError) as caught:
            tls_decision.compile_view(secret_bearing)
        self.assertEqual(str(caught.exception), "invalid TLS decision snapshot")
        self.assertIsNone(caught.exception.__context__)
        self.assertNotIn("hidden-secret", repr(caught.exception))

    def test_classification_output_is_immutable_and_never_includes_hosts(self):
        result = self.classify()
        self.assertEqual(
            (result.action, result.reason), ("needs_registry", "binding_host")
        )
        self.assertNotIn("api.example.com", repr(result))
        self.assertNotIn("hidden-secret", repr(result))
        with self.assertRaises((AttributeError, TypeError)):
            result.reason = "no_binding_host"


if __name__ == "__main__":
    unittest.main()
