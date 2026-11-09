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

"""JWKS client tests (W3.2): bounded caches, refresh-as-revocation,
negative cache, per-peer rate limiting, and configuration-only URLs.

All tests run against an injected fake fetcher and fake clock — zero
network — with fetch counters asserting the "no fetch" guarantees.
"""

from __future__ import annotations

import json
import threading
import unittest

from vectors import DEFAULT_KID, b64url, generate_signing_key, public_jwk

from opensandbox_resolver_auth import UnknownSigningKeyError
from opensandbox_resolver_auth.jwks import JwksClient, parse_ed25519_jwk

BASE = "https://jwks.example"


class FakeClock:
    def __init__(self, now: float = 1000.0) -> None:
        self.now = now

    def __call__(self) -> float:
        return self.now


class FakeFetcher:
    """Per-kid responses; records every URL it was asked for."""

    def __init__(self, responses: dict[str, tuple[int, bytes]] | None = None) -> None:
        self.responses = dict(responses or {})
        self.urls: list[str] = []
        self.error: Exception | None = None

    def __call__(self, url: str) -> tuple[int, bytes]:
        self.urls.append(url)
        if self.error is not None:
            raise self.error
        kid = url.rsplit("/", 1)[-1]
        return self.responses.get(kid, (404, b""))


class JwksCacheTest(unittest.TestCase):
    def setUp(self) -> None:
        self.key = generate_signing_key()
        self.kid = DEFAULT_KID
        self.jwk = json.dumps(public_jwk(self.key)).encode()
        self.fetcher = FakeFetcher({self.kid: (200, self.jwk)})
        self.clock = FakeClock()

    def client(self, **overrides: object) -> JwksClient:
        options: dict[str, object] = {
            "jwks_base": BASE,
            "clock": self.clock,
            "fetcher": self.fetcher,
        }
        options.update(overrides)
        return JwksClient(**options)  # type: ignore[arg-type]

    def test_first_use_fetches_then_caches(self) -> None:
        client = self.client()
        key = client.get(self.kid)
        self.assertEqual(self.key.public_key().public_bytes_raw(), key.public_bytes_raw())
        client.get(self.kid)
        client.get(self.kid)
        self.assertEqual([f"{BASE}/jwks/{self.kid}"], self.fetcher.urls)

    def test_refresh_interval_expiry_triggers_refetch(self) -> None:
        # W3.2 acceptance: cached past 1h -> next use re-fetches.
        client = self.client()
        client.get(self.kid)
        self.clock.now += 3599.0
        client.get(self.kid)
        self.assertEqual(1, len(self.fetcher.urls))
        self.clock.now += 1.0  # exactly at the 1h bound
        client.get(self.kid)
        self.assertEqual(2, len(self.fetcher.urls))

    def test_key_absent_on_refresh_rejects_until_it_reappears(self) -> None:
        # W3.2 acceptance: rotation is revocation — a key gone at refresh
        # denies the kid; once the endpoint serves it again, it verifies.
        client = self.client()
        client.get(self.kid)
        del self.fetcher.responses[self.kid]
        self.clock.now += 3601.0
        with self.assertRaises(UnknownSigningKeyError):
            client.get(self.kid)
        # Inside the negative window: denied without a second fetch.
        with self.assertRaises(UnknownSigningKeyError):
            client.get(self.kid)
        self.assertEqual(2, len(self.fetcher.urls))
        # The key returns; after the negative TTL the kid verifies again.
        self.clock.now += 61.0
        self.fetcher.responses[self.kid] = (200, self.jwk)
        key = client.get(self.kid)
        self.assertEqual(self.key.public_key().public_bytes_raw(), key.public_bytes_raw())
        self.assertEqual(3, len(self.fetcher.urls))

    def test_negative_cache_hit_issues_no_network_request(self) -> None:
        # W3.2 acceptance: counter-asserted — repeated unknown kids do
        # not amplify into fetches.
        client = self.client()
        for _ in range(5):
            with self.assertRaises(UnknownSigningKeyError):
                client.get("pool-unknown:v1")
        self.assertEqual(1, len(self.fetcher.urls))
        # After the negative TTL expires exactly one retry happens.
        self.clock.now += 61.0
        with self.assertRaises(UnknownSigningKeyError):
            client.get("pool-unknown:v1")
        self.assertEqual(2, len(self.fetcher.urls))

    def test_off_grammar_kid_is_rejected_locally_with_no_fetch(self) -> None:
        client = self.client()
        for kid in ("../escape", "a b", "", "a::b", "a/b", "a?b", 123, None):
            with self.assertRaises(UnknownSigningKeyError):
                client.get(kid)  # type: ignore[arg-type]
        self.assertEqual([], self.fetcher.urls)

    def test_per_peer_rate_limit_bounds_fetches(self) -> None:
        client = self.client(fetch_rate=2, rate_window=10.0)
        for kid in ("pool-a:v1", "pool-b:v1"):
            with self.assertRaises(UnknownSigningKeyError):
                client.get(kid)
        self.assertEqual(2, len(self.fetcher.urls))
        # Third distinct kid inside the window: denied without fetching.
        with self.assertRaises(UnknownSigningKeyError):
            client.get("pool-c:v1")
        self.assertEqual(2, len(self.fetcher.urls))
        # Outside the window fetching resumes.
        self.clock.now += 11.0
        with self.assertRaises(UnknownSigningKeyError):
            client.get("pool-c:v1")
        self.assertEqual(3, len(self.fetcher.urls))

    def test_negative_cache_is_bounded(self) -> None:
        client = self.client(negative_cache_size=2, negative_ttl=1e9)
        for kid in ("pool-a:v1", "pool-b:v1", "pool-c:v1"):
            with self.assertRaises(UnknownSigningKeyError):
                client.get(kid)
        # pool-a was evicted (FIFO bound); it fetches again instead of
        # being denied from the negative cache.
        with self.assertRaises(UnknownSigningKeyError):
            client.get("pool-a:v1")
        self.assertEqual(
            ["pool-a:v1", "pool-b:v1", "pool-c:v1", "pool-a:v1"],
            [url.rsplit("/", 1)[-1] for url in self.fetcher.urls],
        )

    def test_fetch_failure_fails_closed_and_does_not_retry_storm(self) -> None:
        client = self.client()
        self.fetcher.error = OSError("endpoint unreachable")
        with self.assertRaises(UnknownSigningKeyError):
            client.get(self.kid)
        with self.assertRaises(UnknownSigningKeyError):
            client.get(self.kid)
        self.assertEqual(1, len(self.fetcher.urls))

    def test_malformed_success_body_is_negative_cached(self) -> None:
        client = self.client()
        self.fetcher.responses[self.kid] = (200, b"{not json")
        with self.assertRaises(UnknownSigningKeyError):
            client.get(self.kid)
        with self.assertRaises(UnknownSigningKeyError):
            client.get(self.kid)
        self.assertEqual(1, len(self.fetcher.urls))

    def test_concurrent_first_contact_coalesces_into_one_fetch(self) -> None:
        client = self.client()
        results: list[bytes] = []
        errors: list[Exception] = []

        def worker() -> None:
            try:
                results.append(client.get(self.kid).public_bytes_raw())
            except Exception as error:  # pragma: no cover - must not happen
                errors.append(error)

        threads = [threading.Thread(target=worker) for _ in range(8)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        self.assertEqual([], errors)
        self.assertEqual(1, len(self.fetcher.urls))
        self.assertEqual({self.key.public_key().public_bytes_raw()}, set(results))


class JwksUrlPolicyTest(unittest.TestCase):
    def test_base_url_must_be_https_without_trailing_slash(self) -> None:
        for bad in ("http://jwks.example", "ftp://jwks.example", "jwks.example", "https://", ""):
            with self.subTest(bad=bad):
                with self.assertRaises(ValueError):
                    JwksClient(jwks_base=bad)
        with self.assertRaises(ValueError):
            JwksClient(jwks_base="https://jwks.example/")

    def test_url_is_built_from_configured_base_and_kid_only(self) -> None:
        # W3.2 acceptance: the URL derives from operator configuration and
        # the grammar-validated kid — never from token contents.
        fetcher = FakeFetcher()
        client = JwksClient(
            jwks_base=BASE, clock=FakeClock(), fetcher=fetcher, negative_ttl=1.0
        )
        with self.assertRaises(UnknownSigningKeyError):
            client.get("pool-a:v2")
        self.assertEqual(["https://jwks.example/jwks/pool-a:v2"], fetcher.urls)


class ParseEd25519JwkTest(unittest.TestCase):
    def setUp(self) -> None:
        self.key = generate_signing_key()
        self.jwk = public_jwk(self.key)

    def parse(self, document: object) -> object:
        try:
            parse_ed25519_jwk(document, expected_kid=DEFAULT_KID)
        except ValueError as error:
            return error
        return "ok"

    def test_valid_jwk_with_extra_members(self) -> None:
        document = {**self.jwk, "use": "sig", "alg": "EdDSA", "unheard": [1, 2]}
        self.assertEqual("ok", self.parse(document))

    def test_jwk_without_kid_is_accepted(self) -> None:
        document = {k: v for k, v in self.jwk.items() if k != "kid"}
        self.assertEqual("ok", self.parse(document))

    def test_rejected_jwks(self) -> None:
        cases = {
            "not-object": [self.jwk],
            "wrong-kty": {**self.jwk, "kty": "RSA"},
            "wrong-crv": {**self.jwk, "crv": "P-256"},
            "kid-mismatch": {**self.jwk, "kid": "other:v1"},
            "x-missing": {k: v for k, v in self.jwk.items() if k != "x"},
            "x-not-string": {**self.jwk, "x": 42},
            "x-short": {**self.jwk, "x": b64url(b"\x01" * 31)},
            "x-long": {**self.jwk, "x": b64url(b"\x01" * 33)},
            "x-padded": {**self.jwk, "x": self.jwk["x"] + "="},
            "x-not-base64": {**self.jwk, "x": "??"},
        }
        for name, document in cases.items():
            with self.subTest(name=name):
                self.assertIsInstance(self.parse(document), ValueError)


if __name__ == "__main__":
    unittest.main()
