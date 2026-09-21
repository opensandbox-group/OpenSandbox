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

"""Conformance tests for the OSEP-0023 canonical decision payload."""

import copy
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


receiver = load("revision_receiver")
load("host_selectors")
decision = load("decision_snapshot")


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


class DecisionSnapshotTest(unittest.TestCase):
    def active(self):
        return {
            "version": 1,
            "vaultRevision": 7,
            "effectivePolicyEpoch": 11,
            "interceptionMode": "credential-bound",
            "state": "active",
            "tlsBindingHostSelectors": ["*.example.com", "api.example.com"],
            "fullRenderedBindings": [
                binding(
                    "a-https",
                    ["https", "http"],
                    ["*.example.com", "api.example.com"],
                    secret="never-log-me",
                ),
                binding("z-http", ["http"], ["plain.example.com"]),
            ],
            "redactions": ["never-log-me"],
        }

    def snapshot(self, value, *, vault=7, policy=11, digest=None):
        raw = (
            value
            if type(value) is bytes
            else json.dumps(value, separators=(",", ":")).encode()
        )
        revision = receiver.Revision(
            "control-a",
            "subject-a",
            1,
            vault,
            policy,
            digest or hashlib.sha256(raw).hexdigest(),
        )
        return receiver.Snapshot(revision, raw)

    def assert_rejected(self, value, **revision):
        with self.assertRaises(decision.DecisionSnapshotError) as caught:
            decision.validate(self.snapshot(value, **revision))
        self.assertEqual(str(caught.exception), "invalid credential decision snapshot")
        self.assertIsNone(caught.exception.__context__)
        self.assertNotIn("never-log-me", repr(caught.exception))

    def test_accepts_active_and_authoritative_empty_payloads(self):
        active = self.snapshot(self.active())
        self.assertIsNone(decision.validate(active))
        store = receiver.Receiver(
            "control-a", "subject-a", decision.validate, max_snapshot_bytes=4096
        )
        self.assertEqual(
            store.prepare(active.revision, active.payload), active.revision
        )
        self.assertEqual(store.commit(active.revision), active.revision)
        self.assertEqual(store.acquire().payload, active.payload)
        empty = {
            "version": 1,
            "vaultRevision": 0,
            "effectivePolicyEpoch": 0,
            "interceptionMode": "credential-bound",
            "state": "active-empty",
            "tlsBindingHostSelectors": [],
            "fullRenderedBindings": [],
            "redactions": [],
        }
        self.assertIsNone(decision.validate(self.snapshot(empty, vault=0, policy=0)))

    def test_rejects_schema_identity_and_derived_state_mismatches(self):
        cases = []
        for field, value in (
            ("version", 2),
            ("boolean version", True),
            ("vaultRevision", 8),
            ("effectivePolicyEpoch", 12),
            ("interceptionMode", "all"),
            ("state", "active-empty"),
            ("tlsBindingHostSelectors", ["api.example.com"]),
            ("redactions", ["short", "much-longer"]),
        ):
            candidate = copy.deepcopy(self.active())
            candidate[field] = value
            cases.append((field, candidate, {}))
        extra = copy.deepcopy(self.active())
        extra["decisionEpoch"] = 1
        cases.append(("unknown field", extra, {}))
        cases.append(("digest", self.active(), {"digest": "0" * 64}))
        raw = json.dumps(self.active(), separators=(",", ":")).encode()
        duplicate = raw.replace(b'"version":1', b'"version":1,"version":1', 1)
        cases.append(("duplicate field", duplicate, {}))
        for name, candidate, revision in cases:
            with self.subTest(name=name):
                self.assert_rejected(candidate, **revision)

    def test_rejects_noncanonical_or_unredacted_bindings(self):
        mutations = (
            lambda value: value["fullRenderedBindings"].reverse(),
            lambda value: value["fullRenderedBindings"][0]["match"].update(
                hosts=["EXAMPLE.com"]
            ),
            lambda value: value["fullRenderedBindings"][0].update(
                headers=[{"name": "Content-Length", "value": "never-log-me"}]
            ),
            lambda value: value.update(redactions=[]),
            lambda value: value["fullRenderedBindings"].append(
                value["fullRenderedBindings"][0]
            ),
        )
        for mutate in mutations:
            candidate = copy.deepcopy(self.active())
            mutate(candidate)
            self.assert_rejected(candidate)

    def test_accepts_empty_substitution_and_legacy_empty_wildcard_language(self):
        candidate = self.active()
        candidate["fullRenderedBindings"] = [
            binding("empty", ["https"], ["api.example.com"])
        ]
        candidate["fullRenderedBindings"][0]["substitutions"] = [
            {"placeholder": "__token__", "value": "", "in": ["header"]}
        ]
        candidate["tlsBindingHostSelectors"] = ["api.example.com"]
        candidate["redactions"] = ["__token__"]
        self.assertIsNone(decision.validate(self.snapshot(candidate)))

        base = ".".join(("a" * 63, "b" * 63, "c" * 63, "d" * 60))
        self.assertEqual(len(base), 252)
        candidate["fullRenderedBindings"] = [
            binding("legacy", ["https"], ["*." + base])
        ]
        candidate["tlsBindingHostSelectors"] = []
        candidate["redactions"] = []
        self.assertIsNone(decision.validate(self.snapshot(candidate)))

    def test_accepts_go_del_redaction_variants(self):
        candidate = self.active()
        candidate["fullRenderedBindings"] = [
            binding("del", ["https"], ["api.example.com"])
        ]
        candidate["fullRenderedBindings"][0]["substitutions"] = [
            {"placeholder": "__del__", "value": "\x7f", "in": ["body"]}
        ]
        candidate["tlsBindingHostSelectors"] = ["api.example.com"]
        candidate["redactions"] = ["__del__", "%7F", "%7f", "\x7f"]
        self.assertIsNone(decision.validate(self.snapshot(candidate)))

    def test_redaction_order_uses_utf8_byte_length(self):
        candidate = self.active()
        candidate["fullRenderedBindings"] = [
            binding("unicode", ["https"], ["api.example.com"], secret="a")
        ]
        candidate["tlsBindingHostSelectors"] = ["api.example.com"]
        candidate["redactions"] = ["é", "a"]
        self.assertIsNone(decision.validate(self.snapshot(candidate)))
        candidate["redactions"].reverse()
        self.assert_rejected(candidate)


if __name__ == "__main__":
    unittest.main()
