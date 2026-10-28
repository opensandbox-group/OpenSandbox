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

"""Bounded, generation-fenced TLS connection admission foundation."""

import hashlib
import importlib.util
import json
import sys
import threading
import traceback
import unittest
from concurrent.futures import ThreadPoolExecutor
from dataclasses import FrozenInstanceError, replace
from pathlib import Path
from unittest.mock import patch

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
selectors = load("host_selectors")
load("decision_snapshot")
decision = load("tls_decision")
registry_module = load("tls_registry")


class ObservedLock:
    """A real mutex with test-only events at entry and before publication unlock."""

    def __init__(self):
        self.lock = threading.Lock()
        self.attempted = threading.Event()
        self.before_unlock = threading.Event()
        self.resume = threading.Event()
        self.pause_next_exit = False

    def __enter__(self):
        self.attempted.set()
        self.lock.acquire()
        return self

    def __exit__(self, *args):
        try:
            if self.pause_next_exit:
                self.pause_next_exit = False
                self.before_unlock.set()
                if not self.resume.wait(5):
                    raise AssertionError("publication unlock timed out")
        finally:
            self.lock.release()

    def locked(self):
        return self.lock.locked()


class TLSRegistryTest(unittest.TestCase):
    identity = ("control-a", "subject-a")

    def snapshot(
        self, epoch=1, host="api.example.com", identity=None, *, secret="",
        methods=("GET",), paths=("/api/*",), vault_revision=None, policy_epoch=1,
    ):
        control, subject = identity or self.identity
        hosts = list(host) if type(host) is tuple else ([] if host is None else [host])
        bindings = [] if host is None else [{
            "name": "binding-a",
            "match": {
                "schemes": ["https"],
                "hosts": hosts,
                "methods": list(methods),
                "paths": list(paths),
            },
            "headers": [] if not secret else [{"name": "Private-Token", "value": secret}],
        }]
        payload = {
            "version": 1,
            "vaultRevision": (1 if bindings else 0) if vault_revision is None else vault_revision,
            "effectivePolicyEpoch": policy_epoch,
            "interceptionMode": "credential-bound",
            "state": "active" if bindings else "active-empty",
            "tlsBindingHostSelectors": sorted(hosts),
            "fullRenderedBindings": bindings,
            "redactions": [] if not secret else [secret],
        }
        raw = json.dumps(payload, separators=(",", ":")).encode()
        revision = receiver.Revision(
            control, subject, epoch, payload["vaultRevision"], policy_epoch,
            hashlib.sha256(raw).hexdigest(),
        )
        return receiver.Snapshot(revision, raw)

    def admit(self, registry, sni="api.example.com", identity=None, **kwargs):
        return registry.admit(
            identity=self.identity if identity is None else identity,
            sni=sni,
            ech_hidden=kwargs.pop("ech_hidden", False),
            static_passthrough=kwargs.pop("static_passthrough", ()),
        )

    def test_unknown_snapshot_denies_but_early_passthrough_stays_available(self):
        registry = registry_module.BoundConnectionRegistry(capacity=1)
        denied = self.admit(registry)
        self.assertEqual((denied.action, denied.reason, denied.token),
                         ("deny", "snapshot_missing", None))
        self.assertEqual(self.admit(registry, ech_hidden=True).reason, "ech")
        self.assertEqual(self.admit(registry, sni=None).reason, "no_sni")
        self.assertEqual(registry.count, 0)

    def test_bound_capacity_never_downgrades_and_release_is_idempotent(self):
        registry = registry_module.BoundConnectionRegistry(capacity=1)
        registry.activate(self.snapshot())
        first = self.admit(registry)
        self.assertEqual((first.action, first.reason), ("decrypt", "binding_host"))
        self.assertIsNotNone(first.token)
        full = self.admit(registry)
        self.assertEqual((full.action, full.reason, full.token),
                         ("deny", "registry_exhausted", None))
        self.assertEqual(self.admit(registry, sni="other.example.com").action,
                         "passthrough")
        self.assertEqual(registry.count, 1)
        self.assertTrue(registry.release(first.token))
        self.assertFalse(registry.release(first.token))
        self.assertEqual(self.admit(registry).action, "decrypt")

    def test_epoch_change_affects_new_admissions_but_keeps_old_entry_tracked(self):
        registry = registry_module.BoundConnectionRegistry(capacity=2)
        registry.activate(self.snapshot())
        old = self.admit(registry)
        registry.activate(self.snapshot(epoch=2, host="new.example.com"))
        self.assertEqual(self.admit(registry).action, "passthrough")
        new = self.admit(registry, sni="new.example.com")
        self.assertEqual((new.action, new.token.revision.decision_epoch),
                         ("decrypt", 2))
        self.assertEqual(registry.count, 2)
        self.assertTrue(registry.release(old.token))
        self.assertEqual(registry.count, 1)

    def test_stale_epoch_and_generation_replacement_with_live_entries_fail_closed(self):
        registry = registry_module.BoundConnectionRegistry(capacity=1)
        registry.activate(self.snapshot(epoch=2))
        old = self.admit(registry)
        with self.assertRaises(registry_module.RegistryError):
            registry.activate(self.snapshot(epoch=1))
        with self.assertRaises(registry_module.RegistryError):
            registry.activate(self.snapshot(identity=("control-b", "subject-b")))
        self.assertEqual(self.admit(registry, identity=("control-b", "subject-b")).reason,
                         "generation_mismatch")
        self.assertEqual(registry.count, 1)
        self.assertTrue(registry.release(old.token))
        with self.assertRaises(registry_module.RegistryError):
            registry.activate(self.snapshot(identity=("control-b", "subject-b")))
        self.assertEqual(registry.count, 0)

    def test_invalid_capacity_and_malformed_static_input_fail_closed(self):
        for capacity in (0, -1, True, "1"):
            with self.subTest(capacity=capacity):
                with self.assertRaises(ValueError):
                    registry_module.BoundConnectionRegistry(capacity=capacity)
        registry = registry_module.BoundConnectionRegistry(capacity=1)
        registry.activate(self.snapshot())
        result = self.admit(registry, static_passthrough=("api.example.com",))
        self.assertEqual((result.action, result.reason, result.token),
                         ("deny", "invalid_input", None))
        self.assertEqual(registry.count, 0)

    def test_deactivate_denies_new_tls_and_retains_old_entries_for_closure(self):
        registry = registry_module.BoundConnectionRegistry(capacity=1)
        registry.activate(self.snapshot())
        old = self.admit(registry)
        self.assertEqual(registry.deactivate(), (old.token,))
        self.assertEqual(self.admit(registry).reason, "snapshot_missing")
        self.assertEqual(registry.count, 1)
        with self.assertRaises(registry_module.RegistryError):
            registry.activate(self.snapshot(identity=("control-b", "subject-b")))
        self.assertTrue(registry.release(old.token))
        with self.assertRaises(registry_module.RegistryError):
            registry.activate(self.snapshot(identity=("control-b", "subject-b")))

    def test_token_from_another_registry_cannot_release_same_serial(self):
        first_registry = registry_module.BoundConnectionRegistry(capacity=1)
        second_registry = registry_module.BoundConnectionRegistry(capacity=1)
        view = self.snapshot()
        first_registry.activate(view)
        second_registry.activate(view)
        first = self.admit(first_registry)
        second = self.admit(second_registry)
        self.assertFalse(second_registry.release(first.token))
        self.assertEqual(second_registry.count, 1)
        self.assertTrue(second_registry.release(second.token))

    def test_generation_is_immutable_even_without_entries(self):
        registry = registry_module.BoundConnectionRegistry(capacity=1)
        registry.activate(self.snapshot(epoch=9))
        with self.assertRaises(registry_module.RegistryError):
            registry.activate(self.snapshot(identity=("control-b", "subject-b")))

    def test_activation_rejects_unvalidated_selector_view(self):
        registry = registry_module.BoundConnectionRegistry(capacity=1)
        forged = decision.TLSSelectorView(
            receiver.Revision("control-a", "subject-a", 1, 1, 1, "0" * 64),
            (),
        )
        with self.assertRaises(registry_module.RegistryError):
            registry.activate(forged)

    def test_wildcard_to_exact_returns_only_newly_uncovered_memberships(self):
        registry = registry_module.BoundConnectionRegistry(capacity=3)
        registry.activate(self.snapshot(host="*.example.com"))
        api = self.admit(registry, sni="API.EXAMPLE.COM.")
        docs = self.admit(registry, sni="docs.example.com")
        nested = self.admit(registry, sni="a.b.example.com")

        uncovered = registry.activate(self.snapshot(epoch=2, host="api.example.com"))

        self.assertEqual(uncovered, (docs.token, nested.token))
        self.assertEqual(registry.count, 3)
        self.assertEqual(self.admit(registry, sni="docs.example.com").action,
                         "passthrough")
        self.assertTrue(registry.release(api.token))
        self.assertTrue(registry.release(docs.token))
        self.assertTrue(registry.release(nested.token))

    def test_exact_to_wildcard_keeps_existing_host_covered(self):
        registry = registry_module.BoundConnectionRegistry(capacity=2)
        registry.activate(self.snapshot())
        api = self.admit(registry)

        self.assertEqual(registry.activate(
            self.snapshot(epoch=2, host="*.example.com")), ())
        docs = self.admit(registry, sni="docs.example.com")
        self.assertEqual((docs.action, registry.count), ("decrypt", 2))
        self.assertTrue(registry.release(api.token))

    def test_empty_and_unchanged_host_transitions_do_not_repeat_old_uncoverage(self):
        registry = registry_module.BoundConnectionRegistry(capacity=2)
        registry.activate(self.snapshot(host="*.example.com"))
        api = self.admit(registry)
        docs = self.admit(registry, sni="docs.example.com")

        self.assertEqual(registry.activate(
            self.snapshot(epoch=2, host="api.example.com")), (docs.token,))
        self.assertEqual(registry.activate(
            self.snapshot(epoch=3, host="api.example.com")), ())
        self.assertEqual(registry.activate(
            self.snapshot(epoch=4, host=None)), (api.token,))
        self.assertEqual(registry.count, 2)
        self.assertEqual(self.admit(registry).action, "passthrough")

    def test_rejected_transition_preserves_old_view_and_memberships(self):
        registry = registry_module.BoundConnectionRegistry(capacity=2)
        registry.activate(self.snapshot(epoch=2, host="*.example.com"))
        old = self.admit(registry, sni="docs.example.com")

        with self.assertRaises(registry_module.RegistryError):
            registry.activate(self.snapshot(epoch=1, host="api.example.com"))

        self.assertEqual(registry.count, 1)
        other = self.admit(registry, sni="other.example.com")
        self.assertEqual(other.action, "decrypt")
        self.assertEqual(registry.activate(
            self.snapshot(epoch=3, host="api.example.com")),
            (old.token, other.token))

    def test_invalid_snapshot_does_not_replace_active_view(self):
        registry = registry_module.BoundConnectionRegistry(capacity=2)
        registry.activate(self.snapshot(host="*.example.com"))
        old = self.admit(registry, sni="docs.example.com")
        candidate = self.snapshot(epoch=2, host="api.example.com")
        invalid = receiver.Snapshot(candidate.revision, b"invalid snapshot")

        with self.assertRaises(registry_module.RegistryError):
            registry.activate(invalid)

        self.assertEqual(registry.count, 1)
        other = self.admit(registry, sni="other.example.com")
        self.assertEqual(other.action, "decrypt")
        self.assertEqual(registry.activate(candidate),
                         (old.token, other.token))

    def test_admission_racing_with_cutover_is_either_reported_or_passthrough(self):
        old = self.snapshot(host="*.example.com")
        new = self.snapshot(epoch=2, host="api.example.com")
        for _ in range(24):
            registry = registry_module.BoundConnectionRegistry(capacity=1)
            registry.activate(old)
            start = threading.Barrier(3)

            def admit():
                start.wait()
                return self.admit(registry, sni="docs.example.com")

            def cutover():
                start.wait()
                return registry.activate(new)

            with ThreadPoolExecutor(max_workers=2) as pool:
                admitted = pool.submit(admit)
                transitioned = pool.submit(cutover)
                start.wait()
                result = admitted.result(timeout=2)
                uncovered = transitioned.result(timeout=2)

            if result.action == "decrypt":
                self.assertEqual(uncovered, (result.token,))
            else:
                self.assertEqual((result.action, result.reason, uncovered),
                                 ("passthrough", "no_binding_host", ()))


class TLSRequestAdmissionTest(unittest.TestCase):
    identity = TLSRegistryTest.identity
    snapshot = TLSRegistryTest.snapshot
    admit = TLSRegistryTest.admit

    def new_receiver(self, snapshot=None):
        identity = self.identity if snapshot is None else (
            snapshot.revision.control_generation, snapshot.revision.subject_generation,
        )
        active = receiver.Receiver(*identity, decision.validate, max_snapshot_bytes=65536)
        if snapshot is not None:
            self.commit(active, snapshot)
        return active

    def commit(self, active, snapshot):
        active.prepare(snapshot.revision, snapshot.payload)
        active.commit(snapshot.revision)

    def setup_registry(self, snapshot=None, *, capacity=4, request_capacity=None):
        snapshot = self.snapshot() if snapshot is None else snapshot
        active = self.new_receiver(snapshot)
        registry = registry_module.BoundConnectionRegistry(
            capacity=capacity, receiver=active, request_capacity=request_capacity,
        )
        registry.activate(snapshot)
        return registry, active

    def assert_denied(self, registry, token, reason):
        requests = dict(registry._requests)
        index = {key: set(value) for key, value in registry._requests_by_connection.items()}
        result = registry.acquire_request(token)
        self.assertEqual((result.action, result.reason, result.snapshot), ("deny", reason, None))
        self.assertIsNone(result.handle)
        self.assertEqual(registry._requests, requests)
        self.assertEqual(registry._requests_by_connection, index)

    def test_allow_pins_real_immutable_snapshot(self):
        registry, active = self.setup_registry(self.snapshot(secret="canaryCredential"))
        result = registry.acquire_request(self.admit(registry).token)
        self.assertEqual((result.action, result.reason), ("allow", "admitted"))
        self.assertIs(result.snapshot, active.acquire())
        self.assertIs(type(result.snapshot.payload), bytes)
        for target, attribute, value in (
            (result, "action", "deny"),
            (result.snapshot, "payload", b"changed"),
        ):
            with self.assertRaises(FrozenInstanceError):
                setattr(target, attribute, value)
        self.assertFalse(hasattr(result, "__dict__"))
        self.assertNotIn("canaryCredential", repr(result))
        self.assertNotIn("api.example.com", repr(result))

    def test_remove_readd_never_revives_old_token(self):
        registry, active = self.setup_registry()
        old = self.admit(registry).token
        removed = self.snapshot(epoch=2, host=None)
        self.assertEqual(registry.activate(removed), (old,))
        self.commit(active, removed)
        self.assert_denied(registry, old, "connection_fenced")
        restored = self.snapshot(epoch=3)
        self.commit(active, restored)
        self.assertEqual(registry.activate(restored), ())
        self.assert_denied(registry, old, "connection_fenced")
        new = self.admit(registry).token
        self.assertNotEqual(new.serial, old.serial)
        self.assertEqual(registry.acquire_request(new).action, "allow")
        self.assertEqual(registry.activate(self.snapshot(epoch=4, host=None)), (old, new))
        self.assert_denied(registry, old, "connection_fenced")

    def test_mutable_snapshot_payload_is_rejected(self):
        registry = registry_module.BoundConnectionRegistry(capacity=1)
        snapshot = self.snapshot()
        with self.assertRaises(registry_module.RegistryError):
            registry.activate(receiver.Snapshot(snapshot.revision, bytearray(snapshot.payload)))

    def test_request_requires_exact_live_token_without_hashing_bad_serials(self):
        registry, _ = self.setup_registry()
        token = self.admit(registry).token
        other, _ = self.setup_registry()
        foreign = self.admit(other).token

        class BadHash(int):
            def __hash__(self):
                raise AssertionError("must not hash untrusted serial")

        class TokenSubclass(registry_module.AdmissionToken):
            pass

        candidates = (
            None, object(), {}, foreign, replace(token),
            replace(token, serial=[]), replace(token, serial=True),
            replace(token, serial=BadHash(token.serial)),
            replace(token, revision=self.snapshot(epoch=2).revision),
            TokenSubclass(token.serial, token.revision, token.sni, token.owner),
        )
        for candidate in candidates:
            with self.subTest(candidate_type=type(candidate).__name__):
                self.assert_denied(registry, candidate, "invalid_token")
        self.assertEqual(registry.acquire_request(token).action, "allow")
        self.assertTrue(registry.release(token))
        self.assert_denied(registry, token, "invalid_token")

    def test_receiver_is_bound_once_and_missing_or_replaced_receivers_deny(self):
        snapshot = self.snapshot()
        registry = registry_module.BoundConnectionRegistry(capacity=1)
        registry.activate(snapshot)
        self.assert_denied(registry, self.admit(registry).token, "snapshot_missing")
        active = self.new_receiver()
        registry = registry_module.BoundConnectionRegistry(capacity=1, receiver=active)
        registry.activate(snapshot)
        token = self.admit(registry).token
        self.assert_denied(registry, token, "snapshot_missing")
        self.commit(active, snapshot)
        self.assertEqual(registry.acquire_request(token).action, "allow")
        replacement, _ = self.setup_registry(snapshot)
        self.assert_denied(replacement, token, "invalid_token")
        self.assertFalse(hasattr(registry, "rebind"))
        with self.assertRaises(TypeError):
            registry.acquire_request(token, receiver=self.new_receiver(snapshot))

        class ReceiverSubclass(receiver.Receiver):
            pass

        wrong = ReceiverSubclass(*self.identity, decision.validate, max_snapshot_bytes=65536)
        for value in (object(), lambda: snapshot, wrong):
            with self.subTest(receiver_type=type(value).__name__):
                with self.assertRaisesRegex(TypeError, "^TLS registry receiver required$"):
                    registry_module.BoundConnectionRegistry(capacity=1, receiver=value)

    def test_both_publication_orders_deny_until_revisions_agree(self):
        for receiver_first in (True, False):
            with self.subTest(receiver_first=receiver_first):
                registry, active = self.setup_registry()
                token = self.admit(registry).token
                new = self.snapshot(epoch=2)
                if receiver_first:
                    self.commit(active, new)
                else:
                    registry.activate(new)
                self.assert_denied(registry, token, "snapshot_mismatch")
                if receiver_first:
                    registry.activate(new)
                else:
                    self.commit(active, new)
                self.assertIs(registry.acquire_request(token).snapshot, active.acquire())

    def test_full_revision_identity_is_required_even_at_same_epoch(self):
        original = self.snapshot()
        variants = (
            self.snapshot(epoch=2),
            self.snapshot(secret="differentDigest"),
            self.snapshot(vault_revision=2),
            self.snapshot(policy_epoch=2),
            self.snapshot(identity=("control-b", "subject-a")),
            self.snapshot(identity=("control-a", "subject-b")),
        )
        for variant in variants:
            with self.subTest(revision=variant.revision):
                active = self.new_receiver(variant)
                registry = registry_module.BoundConnectionRegistry(capacity=1, receiver=active)
                registry.activate(original)
                self.assert_denied(registry, self.admit(registry).token, "snapshot_mismatch")

    def test_rotation_and_request_selectors_keep_token_but_pin_new_snapshot(self):
        for change in (
            {"secret": "newCredential", "vault_revision": 2},
            {"methods": ("POST",)},
            {"paths": ("/changed/*",)},
        ):
            with self.subTest(change=change):
                original = self.snapshot(secret="oldCredential")
                registry, active = self.setup_registry(original)
                token = self.admit(registry).token
                old_request = registry.acquire_request(token)
                kwargs = {"secret": "oldCredential", **change}
                new = self.snapshot(epoch=2, **kwargs)
                self.commit(active, new)
                self.assertEqual(registry.activate(new), ())
                new_request = registry.acquire_request(token)
                self.assertEqual(new_request.action, "allow")
                self.assertIs(new_request.snapshot, active.acquire())
                self.assertEqual(new_request.snapshot.payload, new.payload)
                self.assertEqual(old_request.snapshot.payload, original.payload)
                for request, secret in (
                    (old_request, "oldCredential"),
                    (new_request, kwargs["secret"]),
                ):
                    payload = json.loads(request.snapshot.payload)
                    header = payload["fullRenderedBindings"][0]["headers"][0]
                    self.assertEqual(header["value"], secret)
                    self.assertEqual(payload["redactions"], [secret])
                self.assertEqual(registry._request_fenced, set())
                self.assertTrue(registry.finish_request(old_request.handle))
                self.assertTrue(registry.finish_request(new_request.handle))
                self.assertEqual(registry.request_count, 0)

    def test_semantic_coverage_fences_only_truly_uncovered_hosts(self):
        cases = (
            ("*.example.com", "api.example.com", ("allow", "deny")),
            ("api.example.com", "*.example.com", ("allow",)),
            (("*.example.com", "api.example.com"), "*.example.com", ("allow", "allow")),
            ("*.example.com", None, ("deny", "deny")),
        )
        for previous, new_host, expected in cases:
            with self.subTest(previous=previous, new=new_host):
                registry, active = self.setup_registry(self.snapshot(host=previous))
                tokens = [self.admit(registry, sni="API.EXAMPLE.COM.").token]
                if len(expected) == 2:
                    tokens.append(self.admit(registry, sni="docs.example.com").token)
                new = self.snapshot(epoch=2, host=new_host)
                self.commit(active, new)
                uncovered = registry.activate(new)
                self.assertEqual(uncovered, tuple(
                    token for token, action in zip(tokens, expected) if action == "deny"
                ))
                for token, action in zip(tokens, expected):
                    result = registry.acquire_request(token)
                    self.assertEqual(result.action, action)
                    if action == "deny":
                        self.assertIsNone(result.snapshot)
                        self.assertEqual(result.reason, "connection_fenced")

    def test_rejected_activation_keeps_view_members_and_fences_unchanged(self):
        registry, _ = self.setup_registry(self.snapshot(host="*.example.com"))
        docs = self.admit(registry, sni="docs.example.com").token
        api = self.admit(registry).token
        current = self.snapshot(epoch=2)
        registry.activate(current)
        view, generation = registry._view, registry._generation
        entries = dict(registry._entries)
        fenced = set(registry._request_fenced)
        candidate = self.snapshot(epoch=3, secret="canaryCredential")

        class RevisionLike:
            def __getattr__(self, name):
                return getattr(candidate.revision, name)

            def __eq__(self, other):
                raise AssertionError("canaryCredential")

        class RevisionSubclass(receiver.Revision):
            pass

        class BytesSubclass(bytes):
            pass

        invalid = (
            self.snapshot(epoch=1),
            self.snapshot(epoch=2, host=None),
            self.snapshot(epoch=3, identity=("control-b", "subject-a")),
            self.snapshot(epoch=3, identity=("control-a", "subject-b")),
            receiver.Snapshot(candidate.revision, b"canaryCredential"),
            receiver.Snapshot(candidate.revision, bytearray(candidate.payload)),
            receiver.Snapshot(candidate.revision, BytesSubclass(candidate.payload)),
            receiver.Snapshot(RevisionLike(), candidate.payload),
            receiver.Snapshot(RevisionSubclass(
                *self.identity, 3, 1, 1, candidate.revision.digest,
            ), candidate.payload),
        )
        for snapshot in invalid:
            with self.subTest(snapshot_type=type(snapshot.payload).__name__):
                try:
                    registry.activate(snapshot)
                except registry_module.RegistryError as error:
                    self.assertEqual(str(error), "invalid TLS registry activation")
                    self.assertIsNone(error.__context__)
                    self.assertNotIn("canaryCredential", traceback.format_exception(error)[-1])
                else:
                    self.fail("invalid snapshot accepted")
                self.assertIs(registry._view, view)
                self.assertEqual(registry._generation, generation)
                self.assertEqual(registry._entries, entries)
                self.assertEqual(registry._request_fenced, fenced)
        self.assertEqual(fenced, {docs.serial})
        self.assertNotIn(api.serial, fenced)

    def test_release_reclaims_fence_budget_and_serials_never_repeat(self):
        registry, active = self.setup_registry(capacity=1)
        last_serial = 0
        for index in range(20):
            token = self.admit(registry).token
            self.assertGreater(token.serial, last_serial)
            last_serial = token.serial
            removed = self.snapshot(epoch=2 * index + 2, host=None)
            self.commit(active, removed)
            registry.activate(removed)
            self.assertEqual(registry._request_fenced, {token.serial})
            self.assertEqual(registry.count, 1)
            restored = self.snapshot(epoch=2 * index + 3)
            self.commit(active, restored)
            registry.activate(restored)
            self.assertEqual(self.admit(registry).reason, "registry_exhausted")
            self.assertTrue(registry.release(token))
            self.assertFalse(registry.release(token))
            self.assertEqual(registry.count, 0)
            self.assertEqual(registry._request_fenced, set())
            self.assert_denied(registry, token, "invalid_token")

    def test_close_and_deactivate_deny_new_requests_but_preserve_old_snapshot(self):
        for operation, reason in (
            ("close", "receiver_unavailable"), ("deactivate", "registry_closed"),
        ):
            with self.subTest(operation=operation):
                registry, active = self.setup_registry(self.snapshot(secret="oldCredential"))
                token = self.admit(registry).token
                request = registry.acquire_request(token)
                if operation == "close":
                    active.close()
                else:
                    self.assertEqual(registry.deactivate(), (token,))
                    self.assertEqual(registry.deactivate(), (token,))
                    self.assertIs(active.acquire(), request.snapshot)
                    with self.assertRaises(registry_module.RegistryError):
                        registry.activate(self.snapshot(epoch=2))
                self.assert_denied(registry, token, reason)
                self.assertIn(b"oldCredential", request.snapshot.payload)
                self.assertTrue(registry.release(token))
                self.assertIn(b"oldCredential", request.snapshot.payload)

    def test_receiver_failure_is_sanitized_without_old_snapshot_fallback(self):
        registry, active = self.setup_registry(self.snapshot(secret="canaryCredential"))
        token = self.admit(registry).token
        old = registry.acquire_request(token)
        error = RuntimeError("api.example.com canaryCredential")
        with patch.object(active, "acquire", side_effect=error):
            result = registry.acquire_request(token)
        self.assertEqual((result.action, result.reason, result.snapshot),
                         ("deny", "receiver_unavailable", None))
        self.assertNotIn("canaryCredential", repr(result))
        self.assertNotIn("api.example.com", repr(result))
        self.assertIsNotNone(old.snapshot)

    def test_request_holds_registry_lock_before_and_after_receiver_pin(self):
        # Both pause locations catch check -> unlock -> acquire implementations.
        for pinned in (False, True):
            for operation in ("remove", "release", "deactivate"):
                with self.subTest(pinned=pinned, operation=operation):
                    registry, active = self.setup_registry()
                    token = self.admit(registry).token
                    old = active.acquire()
                    entered, resume = threading.Event(), threading.Event()
                    mutex = ObservedLock()
                    registry._lock = mutex
                    acquire = active.acquire

                    def paused_acquire():
                        snapshot = acquire() if pinned else None
                        entered.set()
                        if not resume.wait(5):
                            raise AssertionError("request pin timed out")
                        return snapshot if pinned else acquire()

                    def mutate():
                        if operation == "remove":
                            return registry.activate(self.snapshot(epoch=2, host=None))
                        if operation == "release":
                            return registry.release(token)
                        return registry.deactivate()

                    with patch.object(active, "acquire", paused_acquire):
                        with ThreadPoolExecutor(max_workers=2) as pool:
                            request = pool.submit(registry.acquire_request, token)
                            try:
                                self.assertTrue(entered.wait(5))
                                self.assertTrue(mutex.locked())
                                mutex.attempted.clear()
                                mutation = pool.submit(mutate)
                                self.assertTrue(mutex.attempted.wait(5))
                                self.assertFalse(mutation.done())
                            finally:
                                resume.set()
                            result = request.result(timeout=5)
                            mutation.result(timeout=5)
                    self.assertEqual(result.action, "allow")
                    self.assertIs(result.snapshot, old)
                    self.assertEqual(registry.request_count, 0 if operation == "release" else 1)
                    self.assertEqual(
                        registry.finish_request(result.handle), operation != "release",
                    )
                    self.assertEqual(registry._requests_by_connection, {})
                    reason = {
                        "remove": "connection_fenced", "release": "invalid_token",
                        "deactivate": "registry_closed",
                    }[operation]
                    self.assert_denied(registry, token, reason)

    def test_published_fence_release_or_close_wins_before_request_checks(self):
        for operation in ("remove", "release", "deactivate"):
            with self.subTest(operation=operation):
                registry, active = self.setup_registry()
                token = self.admit(registry).token
                mutex = ObservedLock()
                registry._lock = mutex
                mutex.pause_next_exit = True

                def mutate():
                    if operation == "remove":
                        return registry.activate(self.snapshot(epoch=2, host=None))
                    if operation == "release":
                        return registry.release(token)
                    return registry.deactivate()

                with patch.object(active, "acquire", wraps=active.acquire) as acquire:
                    with ThreadPoolExecutor(max_workers=2) as pool:
                        mutation = pool.submit(mutate)
                        try:
                            self.assertTrue(mutex.before_unlock.wait(5))
                            mutex.attempted.clear()
                            request = pool.submit(registry.acquire_request, token)
                            self.assertTrue(mutex.attempted.wait(5))
                            self.assertFalse(request.done())
                        finally:
                            mutex.resume.set()
                        mutation.result(timeout=5)
                        result = request.result(timeout=5)
                    acquire.assert_not_called()
                reason = {
                    "remove": "connection_fenced", "release": "invalid_token",
                    "deactivate": "registry_closed",
                }[operation]
                self.assertEqual((result.action, result.reason, result.snapshot),
                                 ("deny", reason, None))
                self.assertIsNone(result.handle)
                self.assertEqual(registry.request_count, 0)

    def test_receiver_commit_and_close_before_or_after_pin(self):
        for pinned in (False, True):
            for operation in ("commit", "close"):
                with self.subTest(pinned=pinned, operation=operation):
                    registry, active = self.setup_registry()
                    token = self.admit(registry).token
                    old = active.acquire()
                    new = self.snapshot(epoch=2)
                    if operation == "commit":
                        active.prepare(new.revision, new.payload)
                    entered, resume = threading.Event(), threading.Event()
                    acquire = active.acquire

                    def paused_acquire():
                        snapshot = acquire() if pinned else None
                        entered.set()
                        if not resume.wait(5):
                            raise AssertionError("receiver pin timed out")
                        return snapshot if pinned else acquire()

                    with patch.object(active, "acquire", paused_acquire):
                        with ThreadPoolExecutor(max_workers=2) as pool:
                            request = pool.submit(registry.acquire_request, token)
                            try:
                                self.assertTrue(entered.wait(5))
                                if operation == "commit":
                                    mutation = pool.submit(active.commit, new.revision)
                                else:
                                    mutation = pool.submit(active.close)
                                # Receiver state can advance while Registry stays locked.
                                mutation.result(timeout=5)
                            finally:
                                resume.set()
                            result = request.result(timeout=5)
                    if pinned:
                        self.assertEqual(result.action, "allow")
                        self.assertIs(result.snapshot, old)
                        self.assertEqual(registry.request_count, 1)
                        self.assertTrue(registry.finish_request(result.handle))
                    else:
                        reason = (
                            "snapshot_mismatch" if operation == "commit"
                            else "receiver_unavailable"
                        )
                        self.assertEqual((result.action, result.reason, result.snapshot),
                                         ("deny", reason, None))
                        self.assertIsNone(result.handle)
                    self.assertEqual(registry.request_count, 0)
                    self.assertEqual(registry._requests_by_connection, {})

    def test_request_before_view_publication_denies_receiver_ahead(self):
        registry, active = self.setup_registry()
        token = self.admit(registry).token
        new = self.snapshot(epoch=2)
        self.commit(active, new)
        entered, resume = threading.Event(), threading.Event()
        compile_view = registry_module.compile_view

        def paused_compile(snapshot):
            view = compile_view(snapshot)
            entered.set()
            if not resume.wait(5):
                raise AssertionError("view compilation timed out")
            return view

        with patch.object(registry_module, "compile_view", paused_compile):
            with ThreadPoolExecutor(max_workers=1) as pool:
                publication = pool.submit(registry.activate, new)
                try:
                    self.assertTrue(entered.wait(5))
                    self.assert_denied(registry, token, "snapshot_mismatch")
                finally:
                    resume.set()
                publication.result(timeout=5)
        self.assertIs(registry.acquire_request(token).snapshot, active.acquire())

    def test_slow_old_compilation_cannot_replace_new_view_or_fences(self):
        registry, active = self.setup_registry()
        token = self.admit(registry).token
        slow, latest = self.snapshot(epoch=2), self.snapshot(epoch=3, host=None)
        entered, resume = threading.Event(), threading.Event()
        compile_view = registry_module.compile_view

        def paused_compile(snapshot):
            view = compile_view(snapshot)
            if snapshot is slow:
                entered.set()
                if not resume.wait(5):
                    raise AssertionError("old compilation timed out")
            return view

        with patch.object(registry_module, "compile_view", paused_compile):
            with ThreadPoolExecutor(max_workers=1) as pool:
                publication = pool.submit(registry.activate, slow)
                try:
                    self.assertTrue(entered.wait(5))
                    self.commit(active, latest)
                    self.assertEqual(registry.activate(latest), (token,))
                    view = registry._view
                finally:
                    resume.set()
                with self.assertRaises(registry_module.RegistryError):
                    publication.result(timeout=5)
        self.assertIs(registry._view, view)
        self.assertEqual(registry._request_fenced, {token.serial})
        self.assert_denied(registry, token, "connection_fenced")


class TLSRequestLifecycleTest(unittest.TestCase):
    identity = TLSRequestAdmissionTest.identity
    snapshot = TLSRequestAdmissionTest.snapshot
    admit = TLSRequestAdmissionTest.admit
    new_receiver = TLSRequestAdmissionTest.new_receiver
    commit = TLSRequestAdmissionTest.commit
    setup_registry = TLSRequestAdmissionTest.setup_registry
    assert_denied = TLSRequestAdmissionTest.assert_denied

    def assert_consistent(self, registry):
        with registry._lock:
            expected = {}
            for serial, handle in registry._requests.items():
                self.assertEqual(serial, handle.serial)
                self.assertIs(registry._entries[handle.connection.serial], handle.connection)
                expected.setdefault(handle.connection.serial, set()).add(serial)
            self.assertEqual(registry._requests_by_connection, expected)
            self.assertLessEqual(len(registry._requests), registry._request_capacity)
            self.assertLessEqual(registry._request_fenced, set(registry._entries))

    def test_success_is_registered_and_capacity_is_reclaimed_by_finish(self):
        registry, _ = self.setup_registry(capacity=1, request_capacity=2)
        token = self.admit(registry).token
        first, second = [registry.acquire_request(token) for _ in range(2)]
        self.assertEqual(registry.request_count, 2)
        self.assertIs(first.snapshot, first.handle.snapshot)
        self.assertIs(first.handle.connection, token)
        self.assertIs(first.handle.revision, first.snapshot.revision)
        denied = registry.acquire_request(token)
        self.assertEqual((denied.action, denied.reason, denied.snapshot, denied.handle),
                         ("deny", "request_registry_exhausted", None, None))
        self.assertTrue(registry.finish_request(first.handle))
        self.assertFalse(registry.finish_request(first.handle))
        third = registry.acquire_request(token)
        self.assertGreater(third.handle.serial, second.handle.serial)
        self.assertTrue(registry.finish_request(second.handle))
        self.assertTrue(registry.finish_request(third.handle))
        self.assertEqual(registry.request_count, 0)
        self.assert_consistent(registry)

    def test_connection_release_rejects_copied_token(self):
        # This test also runs against the old constructor to expose its equality bug.
        registry = registry_module.BoundConnectionRegistry(capacity=1)
        registry.activate(self.snapshot())
        token = self.admit(registry).token
        self.assertFalse(registry.release(replace(token)))
        self.assertEqual(registry.count, 1)

    def test_default_and_explicit_capacity_are_strict_and_independent(self):
        for value in (0, -1, True, False, 1.0, "1", [], object()):
            with self.subTest(value_type=type(value).__name__):
                with self.assertRaisesRegex(
                    ValueError, "^positive request registry capacity required$",
                ):
                    registry_module.BoundConnectionRegistry(capacity=2, request_capacity=value)
        for capacity, request_capacity, expected in ((2, None, 2), (1, 3, 3), (3, 1, 1)):
            with self.subTest(capacity=capacity, requests=request_capacity):
                registry, _ = self.setup_registry(
                    capacity=capacity, request_capacity=request_capacity,
                )
                token = self.admit(registry).token
                handles = [registry.acquire_request(token).handle for _ in range(expected)]
                self.assertEqual(registry.count, 1)
                self.assertEqual(registry.request_count, expected)
                self.assert_denied(registry, token, "request_registry_exhausted")
                self.assertTrue(registry.release(token))
                for handle in handles:
                    self.assertFalse(registry.finish_request(handle))
                self.assertEqual(registry.request_count, 0)
                self.assert_consistent(registry)

    def test_full_capacity_keeps_identity_and_fence_reason_priority(self):
        for action, reason in (("invalid", "invalid_token"),
                               ("remove", "connection_fenced"),
                               ("deactivate", "registry_closed")):
            with self.subTest(action=action):
                registry, active = self.setup_registry(request_capacity=1)
                token = self.admit(registry).token
                held = registry.acquire_request(token).handle
                candidate = token
                if action == "invalid":
                    candidate = replace(token)
                elif action == "remove":
                    registry.activate(self.snapshot(epoch=2, host=None))
                else:
                    registry.deactivate()
                with patch.object(active, "acquire", wraps=active.acquire) as acquire:
                    self.assert_denied(registry, candidate, reason)
                    acquire.assert_not_called()
                self.assertEqual(registry.request_count, 1)
                self.assertTrue(registry.finish_request(held))
                self.assert_consistent(registry)

    def test_handles_are_exact_immutable_private_capabilities(self):
        registry, _ = self.setup_registry(self.snapshot(secret="canaryCredential"))
        token = self.admit(registry).token
        handle = registry.acquire_request(token).handle
        other, _ = self.setup_registry()
        foreign = other.acquire_request(self.admit(other).token).handle

        class BadHash(int):
            def __hash__(self):
                raise AssertionError("untrusted hash evaluated")

        class HandleSubclass(registry_module.RequestHandle):
            pass

        bad = (None, {}, object(), foreign, replace(handle),
               replace(handle, serial=True), replace(handle, serial=[]),
               replace(handle, serial=BadHash(handle.serial)),
               replace(handle, snapshot=self.snapshot(epoch=2)),
               HandleSubclass(handle.serial, handle.owner, handle.connection, handle.snapshot))
        for candidate in bad:
            self.assertFalse(registry.finish_request(candidate))
            self.assertEqual(registry.request_count, 1)
            self.assert_consistent(registry)
        self.assertNotEqual(handle, replace(handle))
        with self.assertRaises(FrozenInstanceError):
            handle.serial = 99
        self.assertFalse(hasattr(handle, "__dict__"))
        self.assertEqual(repr(handle), f"RequestHandle(serial={handle.serial})")
        self.assertTrue(registry.finish_request(handle))
        self.assertFalse(registry.finish_request(handle))
        self.assertEqual(other.request_count, 1)
        self.assertTrue(other.finish_request(foreign))

    def test_only_exact_terminal_connection_cascades_its_requests(self):
        registry, _ = self.setup_registry(request_capacity=4)
        first, second = self.admit(registry).token, self.admit(registry).token
        requests = [registry.acquire_request(token).handle for token in (first, first, second)]
        other, _ = self.setup_registry()
        foreign = self.admit(other).token

        class BadHash(int):
            def __hash__(self):
                raise AssertionError("untrusted hash evaluated")

        class TokenSubclass(registry_module.AdmissionToken):
            pass

        for token in (None, {}, foreign, replace(first), replace(first, serial=True),
                      replace(first, serial=[]), replace(first, serial=BadHash(first.serial)),
                      TokenSubclass(first.serial, first.revision, first.sni, first.owner)):
            self.assertFalse(registry.release(token))
            self.assertEqual(registry.request_count, 3)
            self.assert_consistent(registry)
        self.assertTrue(registry.release(first))
        self.assertEqual(registry.request_count, 1)
        self.assertEqual(registry.count, 1)
        self.assertFalse(registry.release(first))
        self.assertFalse(registry.finish_request(requests[0]))
        self.assertFalse(registry.finish_request(requests[1]))
        self.assertTrue(registry.finish_request(requests[2]))
        self.assert_consistent(registry)

    def test_rotations_and_shutdown_preserve_registered_old_requests(self):
        for terminal in ("remove", "deactivate", "receiver_close"):
            with self.subTest(terminal=terminal):
                registry, active = self.setup_registry(self.snapshot(secret="oldCredential"))
                token = self.admit(registry).token
                old = registry.acquire_request(token).handle
                new_snapshot = self.snapshot(epoch=2, secret="newCredential")
                self.commit(active, new_snapshot)
                registry.activate(new_snapshot)
                new = registry.acquire_request(token).handle
                self.assertEqual(registry.request_count, 2)
                if terminal == "remove":
                    registry.activate(self.snapshot(epoch=3, host=None))
                    restored = self.snapshot(epoch=4, secret="newCredential")
                    self.commit(active, restored)
                    registry.activate(restored)
                    self.assert_denied(registry, token, "connection_fenced")
                elif terminal == "deactivate":
                    self.assertEqual(registry.deactivate(), (token,))
                    self.assertEqual(registry.deactivate(), (token,))
                    self.assert_denied(registry, token, "registry_closed")
                else:
                    active.close()
                    self.assert_denied(registry, token, "receiver_unavailable")
                self.assertEqual(registry.request_count, 2)
                self.assertIn(b"oldCredential", old.snapshot.payload)
                self.assertIn(b"newCredential", new.snapshot.payload)
                self.assertTrue(registry.finish_request(old))
                self.assertEqual(
                    [item.serial for item in registry.pending_requests()], [new.serial],
                )
                self.assertTrue(registry.release(token))
                self.assertFalse(registry.finish_request(new))
                self.assertIn(b"oldCredential", old.snapshot.payload)
                self.assertIn(b"newCredential", new.snapshot.payload)
                self.assert_consistent(registry)

    def test_pending_metadata_is_bounded_private_and_not_a_finish_handle(self):
        registry, _ = self.setup_registry(
            self.snapshot(secret="canaryCredential"), capacity=1, request_capacity=130,
        )
        token = self.admit(registry).token
        handles = [registry.acquire_request(token).handle for _ in range(130)]
        page = registry.pending_requests()
        self.assertIs(type(page), tuple)
        self.assertEqual(len(page), 128)
        self.assertEqual([row.serial for row in page], [h.serial for h in handles[:128]])
        self.assertEqual(set(page[0].__dataclass_fields__),
                         {"serial", "connection_serial", "revision"})
        self.assertFalse(hasattr(page[0], "__dict__"))
        with self.assertRaises(FrozenInstanceError):
            page[0].serial = 99
        self.assertFalse(registry.finish_request(page[0]))
        for secret in ("canaryCredential", "api.example.com", "Snapshot", "owner="):
            self.assertNotIn(secret, repr(page))
        rest = registry.pending_requests(after_serial=page[-1].serial)
        self.assertEqual([row.serial for row in rest], [h.serial for h in handles[128:]])
        self.assertEqual(registry.pending_requests(after_serial=rest[-1].serial), ())
        registry.release(token)
        self.assertEqual(registry.pending_requests(), ())
        self.assertEqual(registry.pending_requests(connection=token), ())
        self.assert_consistent(registry)

    def test_pending_filters_use_complete_revision_and_exact_connection(self):
        registry, active = self.setup_registry(request_capacity=5)
        first, second = self.admit(registry).token, self.admit(registry).token
        old = registry.acquire_request(first).handle
        new_snapshot = self.snapshot(epoch=2)
        self.commit(active, new_snapshot)
        registry.activate(new_snapshot)
        new = [registry.acquire_request(token).handle for token in (first, second)]
        self.assertEqual([r.serial for r in registry.pending_requests(revision=old.revision)],
                         [old.serial])
        self.assertEqual([r.serial for r in registry.pending_requests(
            connection=first, revision=new_snapshot.revision)], [new[0].serial])
        for field, value in (
            ("control_generation", "other"), ("subject_generation", "other"),
            ("decision_epoch", 9), ("vault_revision", 9), ("policy_epoch", 9),
            ("digest", "0" * 64),
        ):
            changed = replace(old.revision, **{field: value})
            self.assertEqual(registry.pending_requests(revision=changed), ())
        for token in (replace(first), replace(first, serial=[]), replace(first, serial=True), {}):
            self.assertEqual(registry.pending_requests(connection=token), ())
        registry.release(first)
        self.assertEqual([r.serial for r in registry.pending_requests()], [new[1].serial])
        self.assert_consistent(registry)

    def test_pending_rejects_invalid_query_values_without_data_in_error(self):
        registry, _ = self.setup_registry()
        for kwargs in (
            {"limit": 0}, {"limit": -1}, {"limit": 129}, {"limit": True},
            {"limit": 1.0}, {"after_serial": -1}, {"after_serial": True},
            {"after_serial": []}, {"after_serial": "canaryCredential"},
            {"revision": "canaryCredential"}, {"revision": self.snapshot()},
        ):
            with self.subTest(kwargs=kwargs):
                with self.assertRaisesRegex(ValueError, "^invalid pending request query$"):
                    registry.pending_requests(**kwargs)

    def test_pending_pages_allow_completion_and_new_admission_between_calls(self):
        registry, _ = self.setup_registry(request_capacity=4)
        token = self.admit(registry).token
        handles = [registry.acquire_request(token).handle for _ in range(3)]
        first = registry.pending_requests(limit=1)
        registry.finish_request(handles[0])
        registry.finish_request(handles[1])
        added = registry.acquire_request(token).handle
        rest = registry.pending_requests(after_serial=first[-1].serial)
        self.assertEqual([r.serial for r in rest], [handles[2].serial, added.serial])
        self.assert_consistent(registry)

    def test_registration_failure_rolls_back_both_indexes_without_secret_context(self):
        class FailingDict(dict):
            def __setitem__(self, key, value):
                super().__setitem__(key, value)
                raise RuntimeError("canaryCredential")

        class FailingSet(set):
            def add(self, value):
                super().add(value)
                raise RuntimeError("canaryCredential")

        for stage in ("requests", "connection_index", "new_members", "existing_members"):
            with self.subTest(stage=stage):
                registry, _ = self.setup_registry(request_capacity=2)
                token = self.admit(registry).token
                old = None
                if stage == "existing_members":
                    old = registry.acquire_request(token).handle
                    registry._requests_by_connection[token.serial] = FailingSet({old.serial})
                elif stage == "requests":
                    registry._requests = FailingDict()
                elif stage == "connection_index":
                    registry._requests_by_connection = FailingDict()
                before = registry.pending_requests()
                factory = FailingSet if stage == "new_members" else set
                with patch.object(registry_module, "set", factory, create=True):
                    try:
                        registry.acquire_request(token)
                    except registry_module.RegistryError as error:
                        self.assertEqual(str(error), "request registration failed")
                        self.assertIsNone(error.__context__)
                        formatted = "".join(traceback.format_exception(error))
                        self.assertNotIn("canaryCredential", formatted)
                    else:
                        self.fail("registration failure unexpectedly admitted")
                self.assertEqual(registry.pending_requests(), before)
                self.assert_consistent(registry)
                if old:
                    self.assertTrue(registry.finish_request(old))

    def test_long_finish_and_terminal_cleanup_loops_leave_no_tombstones(self):
        registry, active = self.setup_registry(capacity=1, request_capacity=3)
        last_request = 0
        for iteration in range(100):
            token = self.admit(registry).token
            handles = [registry.acquire_request(token).handle for _ in range(3)]
            self.assertGreater(handles[0].serial, last_request)
            last_request = handles[-1].serial
            self.assertTrue(registry.finish_request(handles[1]))
            self.assert_consistent(registry)
            if iteration % 2:
                removed = self.snapshot(epoch=iteration * 2 + 2, host=None)
                self.commit(active, removed)
                registry.activate(removed)
            self.assertTrue(registry.release(token))
            self.assertEqual(registry.request_count, 0)
            self.assertEqual(registry._requests_by_connection, {})
            self.assertEqual(registry._request_fenced, set())
            self.assert_consistent(registry)
            restored = self.snapshot(epoch=iteration * 2 + 3)
            self.commit(active, restored)
            registry.activate(restored)

    def test_two_requests_compete_for_last_global_slot(self):
        registry, _ = self.setup_registry(capacity=2, request_capacity=1)
        tokens = [self.admit(registry).token for _ in range(2)]
        start = threading.Barrier(3)

        def acquire(token):
            start.wait(timeout=5)
            return registry.acquire_request(token)

        with ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(acquire, token) for token in tokens]
            start.wait(timeout=5)
            results = [future.result(timeout=5) for future in futures]
        self.assertEqual(sorted(result.action for result in results), ["allow", "deny"])
        denied = next(result for result in results if result.action == "deny")
        self.assertEqual((denied.reason, denied.handle, denied.snapshot),
                         ("request_registry_exhausted", None, None))
        held = next(result.handle for result in results if result.action == "allow")
        self.assertEqual(registry.request_count, 1)
        self.assert_consistent(registry)
        self.assertTrue(registry.finish_request(held))
        self.assert_consistent(registry)

    def test_concurrent_duplicate_finish_removes_one_record(self):
        registry, _ = self.setup_registry()
        handle = registry.acquire_request(self.admit(registry).token).handle
        start = threading.Barrier(3)

        def finish():
            start.wait(timeout=5)
            return registry.finish_request(handle)

        with ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(finish) for _ in range(2)]
            start.wait(timeout=5)
            results = [future.result(timeout=5) for future in futures]
        self.assertEqual(sorted(results), [False, True])
        self.assertEqual(registry.request_count, 0)
        self.assert_consistent(registry)

    def test_finish_and_terminal_release_have_both_lock_orderings(self):
        for finish_first in (True, False):
            with self.subTest(finish_first=finish_first):
                registry, _ = self.setup_registry()
                token = self.admit(registry).token
                handle = registry.acquire_request(token).handle
                mutex = ObservedLock()
                registry._lock = mutex
                mutex.pause_next_exit = True
                finish = lambda: registry.finish_request(handle)
                release = lambda: registry.release(token)
                with ThreadPoolExecutor(max_workers=2) as pool:
                    first = pool.submit(finish if finish_first else release)
                    try:
                        self.assertTrue(mutex.before_unlock.wait(5))
                        mutex.attempted.clear()
                        second = pool.submit(release if finish_first else finish)
                        self.assertTrue(mutex.attempted.wait(5))
                        self.assertFalse(second.done())
                    finally:
                        mutex.resume.set()
                    self.assertTrue(first.result(timeout=5))
                    self.assertEqual(second.result(timeout=5), finish_first)
                self.assertEqual(registry.request_count, 0)
                self.assertEqual(registry.count, 0)
                self.assert_consistent(registry)

    def test_finish_and_admission_serialize_capacity_reuse(self):
        for finish_first in (True, False):
            with self.subTest(finish_first=finish_first):
                registry, _ = self.setup_registry(request_capacity=1)
                token = self.admit(registry).token
                old = registry.acquire_request(token).handle
                mutex = ObservedLock()
                registry._lock = mutex
                mutex.pause_next_exit = True
                finish = lambda: registry.finish_request(old)
                acquire = lambda: registry.acquire_request(token)
                with ThreadPoolExecutor(max_workers=2) as pool:
                    first = pool.submit(finish if finish_first else acquire)
                    try:
                        self.assertTrue(mutex.before_unlock.wait(5))
                        mutex.attempted.clear()
                        second = pool.submit(acquire if finish_first else finish)
                        self.assertTrue(mutex.attempted.wait(5))
                        self.assertFalse(second.done())
                    finally:
                        mutex.resume.set()
                    first_result, second_result = first.result(timeout=5), second.result(timeout=5)
                result = second_result if finish_first else first_result
                self.assertTrue(first_result if finish_first else second_result)
                if finish_first:
                    self.assertEqual(result.action, "allow")
                    self.assertGreater(result.handle.serial, old.serial)
                    self.assertEqual(registry.request_count, 1)
                    self.assertTrue(registry.finish_request(result.handle))
                else:
                    self.assertEqual((result.reason, result.handle, result.snapshot),
                                     ("request_registry_exhausted", None, None))
                self.assertEqual(registry.request_count, 0)
                self.assert_consistent(registry)

    def test_query_cannot_observe_partial_registration(self):
        registry, _ = self.setup_registry()
        token = self.admit(registry).token
        entered, resume = threading.Event(), threading.Event()
        mutex = ObservedLock()
        registry._lock = mutex

        class PausingDict(dict):
            def __setitem__(self, key, value):
                super().__setitem__(key, value)
                entered.set()
                if not resume.wait(5):
                    raise AssertionError("request registration timed out")

        registry._requests = PausingDict()
        with ThreadPoolExecutor(max_workers=2) as pool:
            request = pool.submit(registry.acquire_request, token)
            try:
                self.assertTrue(entered.wait(5))
                mutex.attempted.clear()
                query = pool.submit(registry.pending_requests)
                self.assertTrue(mutex.attempted.wait(5))
                self.assertFalse(query.done())
            finally:
                resume.set()
            result = request.result(timeout=5)
            page = query.result(timeout=5)
        self.assertEqual([row.serial for row in page], [result.handle.serial])
        self.assert_consistent(registry)
        self.assertTrue(registry.finish_request(result.handle))


class TLSRetirementDeadlineTest(unittest.TestCase):
    identity = TLSRequestAdmissionTest.identity
    snapshot = TLSRequestAdmissionTest.snapshot
    admit = TLSRequestAdmissionTest.admit
    new_receiver = TLSRequestAdmissionTest.new_receiver
    commit = TLSRequestAdmissionTest.commit
    assert_denied = TLSRequestAdmissionTest.assert_denied

    def setUp(self):
        self.clock_patch = patch.object(registry_module.time, "monotonic", return_value=100.0)
        self.clock = self.clock_patch.start()
        self.addCleanup(self.clock_patch.stop)

    def setup_registry(self, *, capacity=4, request_capacity=4, timeout=30):
        snapshot = self.snapshot(secret="private-old-value")
        active = self.new_receiver(snapshot)
        registry = registry_module.BoundConnectionRegistry(
            capacity=capacity, request_capacity=request_capacity, receiver=active,
            drain_timeout_seconds=timeout,
        )
        registry.activate(snapshot)
        return registry, active

    def rotate(self, registry, active, epoch=2, **kwargs):
        snapshot = self.snapshot(epoch=epoch, **kwargs)
        self.commit(active, snapshot)
        uncovered = registry.activate(snapshot)
        return snapshot, uncovered

    def test_timeout_is_strict_and_bounded(self):
        for timeout in (0, -1, 301, True, 30.0, "30", None, float("nan"), float("inf")):
            with self.subTest(timeout=timeout):
                with self.assertRaisesRegex(ValueError, "integer from 1 to 300"):
                    self.setup_registry(timeout=timeout)
        for timeout in (1, 30, 300):
            with self.subTest(timeout=timeout):
                registry, active = self.setup_registry(timeout=timeout)
                token = self.admit(registry).token
                self.rotate(registry, active, host=None)
                self.clock.return_value = 100 + timeout - 0.01
                self.assertEqual(registry.expired_connections(), ())
                self.clock.return_value = 100 + timeout
                self.assertEqual(registry.expired_connections(), (token,))
                self.clock.return_value = 100.0

    def test_active_requests_do_not_expire_without_a_cutover(self):
        registry, _ = self.setup_registry()
        token = self.admit(registry).token
        request = registry.acquire_request(token)
        self.clock.return_value = 10000.0
        self.assertEqual(registry.expired_connections(), ())
        self.assertEqual(registry._request_deadlines, {})
        self.assertEqual(registry._connection_deadlines, {})
        self.assertEqual(registry.request_count, 1)
        self.assertTrue(registry.finish_request(request.handle))

    def test_credential_rotation_expires_only_unfinished_old_requests(self):
        registry, active = self.setup_registry()
        token = self.admit(registry).token
        old = registry.acquire_request(token)
        new_snapshot, uncovered = self.rotate(registry, active, secret="private-new-value")
        self.assertEqual(uncovered, ())
        new = registry.acquire_request(token)
        self.assertEqual(new.action, "allow")
        self.assertIs(new.snapshot, active.acquire())
        self.assertEqual(new.snapshot.revision, new_snapshot.revision)
        self.assertEqual(registry._request_deadlines, {old.handle.serial: 130.0})
        self.clock.return_value = 129.999
        self.assertEqual(registry.expired_connections(), ())
        self.clock.return_value = 130.0
        self.assertEqual(registry.expired_connections(), (token,))
        self.assertEqual(registry.expired_connections(), (token,))
        self.assertEqual(registry.request_count, 2)
        self.assertEqual(registry.count, 1)
        self.assertIn(b"private-old-value", old.snapshot.payload)
        self.assertTrue(registry.finish_request(old.handle))
        self.assertEqual(registry.expired_connections(), ())
        self.assertEqual(registry.request_count, 1)
        self.assertTrue(registry.finish_request(new.handle))

    def test_path_only_and_policy_only_changes_retire_old_requests(self):
        for change in ({"paths": ("/different/*",)}, {"policy_epoch": 2}):
            with self.subTest(change=change):
                registry, active = self.setup_registry()
                token = self.admit(registry).token
                request = registry.acquire_request(token)
                self.rotate(registry, active, **change)
                self.assertEqual(registry._request_deadlines, {request.handle.serial: 130.0})
                self.assertEqual(registry._connection_deadlines, {})
                self.assertEqual(registry.acquire_request(token).action, "allow")

    def test_policy_only_idle_connection_and_completed_request_do_not_expire(self):
        registry, active = self.setup_registry()
        token = self.admit(registry).token
        old = registry.acquire_request(token)
        target, uncovered = self.rotate(
            registry, active, policy_epoch=2, vault_revision=1, secret="private-old-value",
        )
        self.assertEqual(uncovered, ())
        self.assertEqual(registry._connection_deadlines, {})
        new = registry.acquire_request(token)
        self.assertEqual(new.snapshot.revision, target.revision)
        self.assertEqual(old.snapshot.revision.vault_revision, new.snapshot.revision.vault_revision)
        self.assertEqual(old.snapshot.revision.policy_epoch, 1)
        self.assertEqual(new.snapshot.revision.policy_epoch, 2)
        self.assertIn(b"private-old-value", new.snapshot.payload)
        self.assertTrue(registry.finish_request(old.handle))
        self.clock.return_value = 150.0
        self.assertEqual(registry.expired_connections(), ())
        self.assertTrue(registry.finish_request(new.handle))
        self.rotate(registry, active, epoch=3, policy_epoch=3, vault_revision=1, secret="private-old-value")
        self.clock.return_value = 1000.0
        self.assertEqual(registry.expired_connections(), ())
        self.assertEqual(registry.acquire_request(token).action, "allow")

    def test_policy_only_earliest_request_deadline_closes_entire_connection(self):
        registry, active = self.setup_registry()
        token = self.admit(registry).token
        old = registry.acquire_request(token)
        self.rotate(registry, active, policy_epoch=2, vault_revision=1, secret="private-old-value")
        middle = registry.acquire_request(token)
        self.clock.return_value = 120.0
        self.rotate(registry, active, epoch=3, policy_epoch=3, vault_revision=1, secret="private-old-value")
        newest = registry.acquire_request(token)
        self.assertEqual(registry._request_deadlines, {
            old.handle.serial: 130.0, middle.handle.serial: 150.0,
        })
        self.clock.return_value = 130.0
        self.assertEqual(registry.expired_connections(), (token,))
        # The existing transport owner closes the entire connection, including
        # a new request pinned after the latest policy cutover.
        self.assertTrue(registry.release(token))
        self.assertEqual(registry.request_count, 0)
        for request in (old, middle, newest):
            self.assertFalse(registry.finish_request(request.handle))

    def test_repeated_rotations_keep_first_deadline_and_deduplicate_connections(self):
        registry, active = self.setup_registry(request_capacity=8)
        first, second = [self.admit(registry).token for _ in range(2)]
        old = [registry.acquire_request(first) for _ in range(2)]
        self.rotate(registry, active)
        middle = [registry.acquire_request(token) for token in (first, second)]
        self.clock.return_value = 120.0
        self.rotate(registry, active, epoch=3)
        latest = registry.acquire_request(second)
        self.assertEqual(registry._request_deadlines, {
            **{request.handle.serial: 130.0 for request in old},
            **{request.handle.serial: 150.0 for request in middle},
        })
        self.clock.return_value = 130.0
        self.assertEqual(registry.expired_connections(), (first,))
        self.assertTrue(registry.finish_request(old[0].handle))
        self.assertEqual(registry.expired_connections(), (first,))
        self.assertTrue(registry.finish_request(old[1].handle))
        self.assertEqual(registry.expired_connections(), ())
        self.clock.return_value = 150.0
        self.assertEqual(registry.expired_connections(), (first, second))
        self.assertNotIn(latest.handle.serial, registry._request_deadlines)

    def test_idle_uncovered_connections_keep_deadline_across_remove_readd_remove(self):
        registry, active = self.setup_registry()
        token = self.admit(registry).token
        _, uncovered = self.rotate(registry, active, host=None)
        self.assertEqual(uncovered, (token,))
        self.clock.return_value = 110.0
        self.rotate(registry, active, epoch=3)
        fresh = self.admit(registry).token
        self.clock.return_value = 120.0
        _, uncovered = self.rotate(registry, active, epoch=4, host=None)
        self.assertEqual(uncovered, (token, fresh))
        self.assertEqual(registry._connection_deadlines, {token.serial: 130.0, fresh.serial: 150.0})
        self.assert_denied(registry, token, "connection_fenced")
        self.clock.return_value = 130.0
        self.assertEqual(registry.expired_connections(), (token,))
        self.assertEqual(registry.request_count, 0)

    def test_finishing_last_request_does_not_clear_uncovered_connection_deadline(self):
        registry, active = self.setup_registry()
        token = self.admit(registry).token
        request = registry.acquire_request(token)
        self.rotate(registry, active, host=None)
        self.assertTrue(registry.finish_request(request.handle))
        self.assertEqual(registry._request_deadlines, {})
        self.clock.return_value = 130.0
        self.assertEqual(registry.expired_connections(), (token,))
        self.assertTrue(registry.release(token))
        self.assertEqual(registry.expired_connections(), ())
        self.assertEqual(registry._connection_deadlines, {})

    def test_idempotent_and_rejected_activation_do_not_reschedule(self):
        registry, active = self.setup_registry()
        token = self.admit(registry).token
        request = registry.acquire_request(token)
        current, _ = self.rotate(registry, active, host=None)
        self.clock.reset_mock()
        self.clock.return_value = 120.0
        registry.activate(current)
        for rejected in (self.snapshot(), self.snapshot(epoch=3, identity=("other", "subject-a"))):
            with self.assertRaises(registry_module.RegistryError):
                registry.activate(rejected)
        self.clock.assert_not_called()
        self.assertEqual(registry._connection_deadlines, {token.serial: 130.0})
        self.assertEqual(registry._request_deadlines, {request.handle.serial: 130.0})

    def test_deactivation_and_receiver_close_preserve_deadlines_and_handles(self):
        registry, active = self.setup_registry()
        token = self.admit(registry).token
        request = registry.acquire_request(token)
        self.rotate(registry, active)
        self.clock.return_value = 110.0
        self.assertEqual(registry.deactivate(), (token,))
        active.close()
        self.clock.return_value = 130.0
        self.assertEqual(registry.expired_connections(), (token,))
        self.assertEqual(registry.request_count, 1)
        self.assertIn(b"private-old-value", request.handle.snapshot.payload)
        self.assertTrue(registry.release(token))
        self.assertFalse(registry.finish_request(request.handle))
        self.assertEqual(registry._request_deadlines, {})
        self.assertEqual(registry._connection_deadlines, {})

    def test_copied_handles_and_tokens_cannot_clear_deadlines(self):
        registry, active = self.setup_registry()
        token = self.admit(registry).token
        request = registry.acquire_request(token)
        self.rotate(registry, active, host=None)
        self.assertFalse(registry.finish_request(replace(request.handle)))
        self.assertFalse(registry.release(replace(token)))
        self.clock.return_value = 130.0
        self.assertIs(registry.expired_connections()[0], token)
        self.assertEqual(registry.request_count, 1)
        self.assertTrue(registry.release(token))
        self.assertEqual(registry._request_deadlines, {})
        self.assertEqual(registry._connection_deadlines, {})

    def test_expiration_never_reclaims_capacity_or_finishes_work(self):
        registry, active = self.setup_registry(capacity=1, request_capacity=1)
        token = self.admit(registry).token
        request = registry.acquire_request(token)
        self.rotate(registry, active)
        self.clock.return_value = 1000.0
        self.assertEqual(registry.expired_connections(), (token,))
        self.assert_denied(registry, token, "request_registry_exhausted")
        self.assertEqual(self.admit(registry).reason, "registry_exhausted")
        self.assertTrue(registry.finish_request(request.handle))
        self.assertEqual(registry.acquire_request(token).action, "allow")

    def test_expiration_pages_are_bounded_ordered_and_restartable(self):
        registry, active = self.setup_registry(capacity=130)
        tokens = [self.admit(registry).token for _ in range(130)]
        self.rotate(registry, active, host=None)
        self.clock.return_value = 130.0
        first = registry.expired_connections()
        self.assertEqual(first, tuple(tokens[:128]))
        self.assertEqual(registry.expired_connections(after_serial=first[-1].serial), tuple(tokens[128:]))
        self.assertEqual(registry.expired_connections(limit=1), (tokens[0],))
        self.assertTrue(registry.release(tokens[128]))
        self.assertEqual(registry.expired_connections(after_serial=first[-1].serial), (tokens[129],))
        self.assertEqual(registry.expired_connections(after_serial=tokens[-1].serial), ())
        self.assertEqual(registry.expired_connections(limit=1), (tokens[0],))
        self.assertNotIn("private-old-value", repr(first))
        self.assertNotIn("api.example.com", repr(first))

    def test_expiration_query_rejects_invalid_bounds(self):
        registry, _ = self.setup_registry()
        for kwargs in ({"after_serial": -1}, {"after_serial": True}, {"after_serial": "1"},
                       {"limit": 0}, {"limit": 129}, {"limit": True}, {"limit": 1.5}):
            with self.subTest(kwargs=kwargs):
                with self.assertRaisesRegex(ValueError, "invalid expired connection query"):
                    registry.expired_connections(**kwargs)

    def test_admission_racing_cutover_is_retired_only_if_it_pins_old_revision(self):
        for admission_first in (True, False):
            with self.subTest(admission_first=admission_first):
                registry, active = self.setup_registry()
                token = self.admit(registry).token
                target = self.snapshot(epoch=2)
                lock = ObservedLock()
                registry._lock = lock
                lock.pause_next_exit = True
                with ThreadPoolExecutor(max_workers=2) as pool:
                    if admission_first:
                        first = pool.submit(registry.acquire_request, token)
                    else:
                        self.commit(active, target)
                        first = pool.submit(registry.activate, target)
                    self.assertTrue(lock.before_unlock.wait(5))
                    self.clock.return_value = 110.0
                    lock.attempted.clear()
                    if admission_first:
                        self.commit(active, target)
                        second = pool.submit(registry.activate, target)
                    else:
                        second = pool.submit(registry.acquire_request, token)
                    self.assertTrue(lock.attempted.wait(5))
                    lock.resume.set()
                    result = first.result(5) if admission_first else second.result(5)
                    (second if admission_first else first).result(5)
                self.assertEqual(result.action, "allow")
                expected = {result.handle.serial: 140.0} if admission_first else {}
                self.assertEqual(registry._request_deadlines, expected)
                self.clock.return_value = 100.0

    def test_expiration_and_terminal_cleanup_are_lock_consistent(self):
        registry, active = self.setup_registry(capacity=64, request_capacity=64)
        tokens = [self.admit(registry).token for _ in range(64)]
        requests = [registry.acquire_request(token) for token in tokens]
        self.rotate(registry, active)
        self.clock.return_value = 130.0
        with ThreadPoolExecutor(max_workers=8) as pool:
            queries = [pool.submit(registry.expired_connections) for _ in range(32)]
            cleanup = [pool.submit(registry.finish_request, request.handle) for request in requests[::2]]
            cleanup += [pool.submit(registry.release, token) for token in tokens[1::2]]
            for query in queries:
                page = query.result(5)
                self.assertEqual(list(page), sorted(page, key=lambda token: token.serial))
                self.assertTrue(all(any(token is original for original in tokens) for token in page))
            self.assertTrue(all(result.result(5) for result in cleanup))
        self.assertEqual(registry.expired_connections(), ())
        self.assertEqual(registry._request_deadlines, {})
        self.assertEqual(registry._connection_deadlines, {})
        self.assertEqual(registry.request_count, 0)

    def test_query_and_finish_serialize_but_returned_target_can_become_stale(self):
        for query_first in (True, False):
            with self.subTest(query_first=query_first):
                self.clock.return_value = 100.0
                registry, active = self.setup_registry()
                token = self.admit(registry).token
                request = registry.acquire_request(token)
                self.rotate(registry, active)
                self.clock.return_value = 130.0
                lock = ObservedLock()
                registry._lock = lock
                lock.pause_next_exit = True
                with ThreadPoolExecutor(max_workers=2) as pool:
                    if query_first:
                        first = pool.submit(registry.expired_connections)
                    else:
                        first = pool.submit(registry.finish_request, request.handle)
                    self.assertTrue(lock.before_unlock.wait(5))
                    lock.attempted.clear()
                    if query_first:
                        second = pool.submit(registry.finish_request, request.handle)
                    else:
                        second = pool.submit(registry.expired_connections)
                    self.assertTrue(lock.attempted.wait(5))
                    lock.resume.set()
                    page = (first if query_first else second).result(5)
                    self.assertTrue((second if query_first else first).result(5))
                self.assertEqual(page, (token,) if query_first else ())
                self.assertEqual(registry.expired_connections(), ())
                self.assertEqual(registry.request_count, 0)
                self.assertEqual(registry.count, 1)


class TLSH2RequestAdmissionTest(unittest.TestCase):
    identity = TLSRequestAdmissionTest.identity
    snapshot = TLSRequestAdmissionTest.snapshot
    admit = TLSRequestAdmissionTest.admit
    new_receiver = TLSRequestAdmissionTest.new_receiver
    commit = TLSRequestAdmissionTest.commit
    setup_registry = TLSRequestAdmissionTest.setup_registry
    assert_consistent = TLSRequestLifecycleTest.assert_consistent

    def test_client_ids_are_exact_immutable_and_h1_stays_optional(self):
        registry, active = self.setup_registry(request_capacity=4)
        token = self.admit(registry).token
        for stream_id in (None, 1, 0x7fffffff):
            request = registry.acquire_request(token, client_stream_id=stream_id)
            self.assertEqual(request.action, "allow")
            self.assertEqual(request.handle.client_stream_id, stream_id)
            self.assertIs(request.handle.snapshot, active.acquire())
            self.assertIs(request.handle.snapshot, request.snapshot)
            with self.assertRaises(FrozenInstanceError):
                request.handle.client_stream_id = 3
        self.assertEqual(registry.drain_state(token).max_admitted_stream_id, 0x7fffffff)
        self.assertEqual(registry.acquire_request(token).action, "allow")
        self.assert_consistent(registry)

    def test_invalid_client_ids_never_pin_or_change_high_water(self):
        class IntSubclass(int):
            def __lt__(self, other):
                raise AssertionError("untrusted comparison")

        registry, active = self.setup_registry()
        token = self.admit(registry).token
        before = registry.drain_state(token)
        invalid = (-1, 0, 2, 4, 0x80000000, 0x80000001, True, False,
                   1.0, "1", [], {}, object(), IntSubclass(1))
        with patch.object(active, "acquire", wraps=active.acquire) as acquire:
            for stream_id in invalid:
                with self.subTest(stream_id_type=type(stream_id).__name__):
                    request = registry.acquire_request(token, client_stream_id=stream_id)
                    self.assertEqual(
                        (request.action, request.reason, request.handle, request.snapshot),
                        ("deny", "invalid_client_stream_id", None, None),
                    )
                    self.assertEqual(registry.drain_state(token), before)
                    self.assertEqual(registry.request_count, 0)
            acquire.assert_not_called()
        self.assertEqual(registry._max_admitted_stream_ids, {})

    def test_duplicate_ids_are_connection_scoped_and_only_active_ids_are_indexed(self):
        registry, active = self.setup_registry(request_capacity=3)
        first, second = (self.admit(registry).token for _ in range(2))
        original = registry.acquire_request(first, client_stream_id=7)
        parallel = registry.acquire_request(second, client_stream_id=7)
        with patch.object(active, "acquire", wraps=active.acquire) as acquire:
            duplicate = registry.acquire_request(first, client_stream_id=7)
            self.assertEqual(duplicate.reason, "duplicate_client_stream_id")
            self.assertIsNone(duplicate.handle)
            self.assertIsNone(duplicate.snapshot)
            acquire.assert_not_called()
        self.assertEqual(registry.request_count, 2)
        self.assertTrue(registry.finish_request(original.handle))
        # The protocol adapter prevents post-terminal reuse. The registry keeps
        # only active request membership, not an unbounded stream-ID history.
        repeated = registry.acquire_request(first, client_stream_id=7)
        self.assertEqual(repeated.action, "allow")
        self.assertGreater(repeated.handle.serial, original.handle.serial)
        self.assertEqual(registry.drain_state(first).max_admitted_stream_id, 7)
        self.assertTrue(registry.finish_request(parallel.handle))
        self.assertTrue(registry.finish_request(repeated.handle))
        self.assert_consistent(registry)

    def test_concurrent_duplicate_admissions_register_exactly_once(self):
        registry, _ = self.setup_registry(request_capacity=2)
        token = self.admit(registry).token
        barrier = threading.Barrier(3)

        def admit():
            barrier.wait(5)
            return registry.acquire_request(token, client_stream_id=11)

        with ThreadPoolExecutor(max_workers=2) as pool:
            requests = [pool.submit(admit) for _ in range(2)]
            barrier.wait(5)
            results = [request.result(5) for request in requests]
        self.assertEqual(sorted(result.reason for result in results),
                         ["admitted", "duplicate_client_stream_id"])
        self.assertEqual(registry.request_count, 1)
        self.assertEqual(registry.drain_state(token).max_admitted_stream_id, 11)
        self.assert_consistent(registry)

    def test_capacity_and_snapshot_denials_do_not_advance_high_water(self):
        registry, active = self.setup_registry(request_capacity=1)
        token = self.admit(registry).token
        first = registry.acquire_request(token, client_stream_id=3)
        denied = registry.acquire_request(token, client_stream_id=99)
        self.assertEqual(denied.reason, "request_registry_exhausted")
        self.assertEqual(registry.drain_state(token).max_admitted_stream_id, 3)
        self.assertTrue(registry.finish_request(first.handle))
        self.commit(active, self.snapshot(epoch=2))
        denied = registry.acquire_request(token, client_stream_id=99)
        self.assertEqual(denied.reason, "snapshot_mismatch")
        self.assertEqual(registry.drain_state(token).max_admitted_stream_id, 3)
        self.assert_consistent(registry)

    def test_finished_max_is_frozen_once_even_with_a_lower_live_id(self):
        registry, active = self.setup_registry()
        token = self.admit(registry).token
        high = registry.acquire_request(token, client_stream_id=13)
        self.assertTrue(registry.finish_request(high.handle))
        low = registry.acquire_request(token, client_stream_id=3)
        removed = self.snapshot(epoch=2, host=None)
        with patch.object(registry_module.time, "monotonic", return_value=10):
            self.commit(active, removed)
            registry.activate(removed)
        frozen = registry.drain_state(token)
        self.assertEqual(frozen, registry_module.DrainState(True, 13, 40, 13))
        self.assertTrue(registry.finish_request(low.handle))
        self.assertEqual(registry.drain_state(token), frozen)
        restored = self.snapshot(epoch=3)
        self.commit(active, restored)
        registry.activate(restored)
        with patch.object(registry_module.time, "monotonic", return_value=100):
            registry.activate(self.snapshot(epoch=4, host=None))
        self.assertEqual(registry.drain_state(token), frozen)
        self.assertEqual(registry.acquire_request(token, client_stream_id=15).reason,
                         "connection_fenced")

    def test_drain_state_and_request_capacity_are_readonly_and_exact_token_scoped(self):
        registry, _ = self.setup_registry(request_capacity=7)
        token = self.admit(registry).token
        state = registry.drain_state(token)
        self.assertEqual(state, registry_module.DrainState(False, None, None, 0))
        self.assertFalse(hasattr(state, "__dict__"))
        for field, value in (("fenced", True), ("frozen_last_stream_id", 9),
                             ("deadline", 123), ("max_admitted_stream_id", 9)):
            with self.assertRaises(FrozenInstanceError):
                setattr(state, field, value)
        self.assertEqual(registry.request_capacity, 7)
        with self.assertRaises(AttributeError):
            registry.request_capacity = 8
        self.assertEqual(registry_module.BoundConnectionRegistry(capacity=3).request_capacity, 3)
        other, _ = self.setup_registry()
        foreign = self.admit(other).token
        for invalid in (None, {}, foreign, replace(token), replace(token, serial=[]),
                        replace(token, serial=True)):
            self.assertIsNone(registry.drain_state(invalid))
        registry.acquire_request(token, client_stream_id=5)
        self.assertEqual(state.max_admitted_stream_id, 0)
        self.assertEqual(registry.drain_state(token).max_admitted_stream_id, 5)
        registry.deactivate()
        self.assertEqual(registry.drain_state(token),
                         registry_module.DrainState(True, None, None, 5))
        self.assertTrue(registry.release(token))
        self.assertIsNone(registry.drain_state(token))

    def test_h2_registration_faults_preserve_indexes_and_previous_high_water(self):
        class FailingDict(dict):
            def __setitem__(self, key, value):
                super().__setitem__(key, value)
                raise MemoryError("private-request-value")

        class FailingSet(set):
            def add(self, value):
                super().add(value)
                raise MemoryError("private-request-value")

        class FailingCopy(dict):
            def copy(self):
                raise MemoryError("private-request-value")

        class FailingPreparedCopy(dict):
            def copy(self):
                return FailingDict(self)

        for prior_request in (False, True):
            for stage in ("high_water_copy", "high_water_write", "requests", "index", "members"):
                with self.subTest(prior_request=prior_request, stage=stage):
                    registry, _ = self.setup_registry(request_capacity=2)
                    token = self.admit(registry).token
                    if prior_request:
                        registry.acquire_request(token, client_stream_id=7)
                    before_requests = registry.pending_requests()
                    before_drain = registry.drain_state(token)
                    if stage == "high_water_copy":
                        registry._max_admitted_stream_ids = FailingCopy(registry._max_admitted_stream_ids)
                    elif stage == "high_water_write":
                        registry._max_admitted_stream_ids = FailingPreparedCopy(registry._max_admitted_stream_ids)
                    elif stage == "requests":
                        registry._requests = FailingDict(registry._requests)
                    elif stage == "index":
                        registry._requests_by_connection = FailingDict(registry._requests_by_connection)
                    elif prior_request:
                        registry._requests_by_connection[token.serial] = FailingSet(
                            registry._requests_by_connection[token.serial],
                        )
                    factory = FailingSet if stage == "members" else set
                    with patch.object(registry_module, "set", factory, create=True):
                        try:
                            registry.acquire_request(token, client_stream_id=101)
                        except registry_module.RegistryError as error:
                            self.assertEqual(str(error), "request registration failed")
                            self.assertIsNone(error.__context__)
                            self.assertNotIn("private-request-value", "".join(traceback.format_exception(error)))
                        else:
                            self.fail("failed H2 registration was admitted")
                    self.assertEqual(registry.pending_requests(), before_requests)
                    self.assertEqual(registry.drain_state(token), before_drain)
                    self.assert_consistent(registry)

    def test_h2_finish_has_constant_history_and_release_reclaims_all_metadata(self):
        registry, active = self.setup_registry(capacity=1, request_capacity=2)
        token = self.admit(registry).token
        for stream_id in range(1, 1000, 2):
            request = registry.acquire_request(token, client_stream_id=stream_id)
            self.assertTrue(registry.finish_request(request.handle))
            self.assertEqual(registry.request_count, 0)
            self.assertEqual(registry._requests_by_connection, {})
            self.assertEqual(registry._max_admitted_stream_ids, {token.serial: stream_id})
            self.assertEqual(registry._frozen_last_stream_ids, {})
        held = registry.acquire_request(token, client_stream_id=1001)
        removed = self.snapshot(epoch=2, host=None)
        self.commit(active, removed)
        registry.activate(removed)
        self.assertTrue(registry.release(token))
        self.assertFalse(registry.release(token))
        self.assertFalse(registry.finish_request(held.handle))
        for records in (registry._max_admitted_stream_ids, registry._frozen_last_stream_ids,
                        registry._requests, registry._requests_by_connection,
                        registry._connection_deadlines, registry._request_deadlines):
            self.assertEqual(records, {})
        self.assertEqual(registry._request_fenced, set())
        self.assert_consistent(registry)


if __name__ == "__main__":
    unittest.main()
