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

"""Verifier-core tests: the OSEP-0025 credential matrix (W3.1).

Temporary self-made vectors stand in for the shared W0 fixtures until
they are frozen in ``components/egress/pkg/credentialvault/testdata/``;
the cases below already mirror the W0 matrix one-to-one, each labeled
with its OSEP rule.
"""

from __future__ import annotations

import json
import unittest

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
from vectors import DEFAULT_NOW, b64url, generate_signing_key, mint, public_jwk

from opensandbox_resolver_auth import (
    CREDENTIAL_INVALID,
    CREDENTIAL_MISSING,
    SUBJECT_MISMATCH,
    CredentialVerifier,
    StaticKeyProvider,
    VerificationError,
)

KID = "pool-a:v2"
OTHER_KEY_KID = "pool-b:v1"
SUBJECT = "sbx_01HZYEXAMPLE"


class FakeClock:
    def __init__(self, now: float = DEFAULT_NOW) -> None:
        self.now = now


class CountingProvider(StaticKeyProvider):
    """Static provider that counts lookups — vectors assert zero fetch."""

    def __init__(self, keys: dict[str, Ed25519PublicKey]) -> None:
        super().__init__(keys)
        self.calls: list[str] = []

    def get(self, kid: str) -> Ed25519PublicKey:
        self.calls.append(kid)
        return super().get(kid)


class VerifierMatrixTest(unittest.TestCase):
    def setUp(self) -> None:
        self.key = generate_signing_key()
        self.other_key = generate_signing_key()
        self.provider = CountingProvider(
            {
                KID: self.key.public_key(),
                OTHER_KEY_KID: self.other_key.public_key(),
                "sbx_01HZYEXAMPLE": self.key.public_key(),
            }
        )
        self.clock = FakeClock()
        self.verifier = CredentialVerifier(
            key_provider=self.provider,
            expected_issuer="opensandbox",
            expected_aud="credential-resolver@resolver.internal",
            clock=lambda: self.clock.now,
        )

    def verify(self, token: object, subject: str = SUBJECT) -> tuple[str, str]:
        try:
            self.verifier.verify(token, subject=subject)
        except VerificationError as error:
            return error.reason, error.detail
        return "ok", ""

    def test_happy_path_returns_verified_claims(self) -> None:
        token = mint(self.key)
        verified = self.verifier.verify(token, subject=SUBJECT)
        self.assertEqual(KID, verified.kid)
        self.assertEqual("opensandbox", verified.issuer)
        self.assertEqual("credential-resolver@resolver.internal", verified.audience)
        self.assertEqual(SUBJECT, verified.subject)
        self.assertEqual(DEFAULT_NOW, verified.issued_at)
        self.assertEqual(DEFAULT_NOW + 3600, verified.expires_at)
        self.assertEqual("kX9sT2vZ8qLm4NpR6wYb3A", verified.jti)

    # -- 401 mapping: every failure path is a 401-class VerificationError.

    def test_missing_credential_maps_to_credential_missing(self) -> None:
        for absent in (None, ""):
            reason, _ = self.verify(absent)
            self.assertEqual(CREDENTIAL_MISSING, reason, repr(absent))

    def test_all_failure_paths_map_to_credential_invalid_or_mismatch(self) -> None:
        cases = {
            "alg-none": mint(self.key, alg="none"),
            "alg-hs256-confusion": mint(self.key, alg="HS256"),
            "typ-wrong": mint(self.key, typ="JOSE"),
            "header-jku": mint(self.key, header_extra={"jku": "https://evil.example/jwks"}),
            "header-x5u": mint(self.key, header_extra={"x5u": "https://evil.example/x"}),
            "header-embedded-jwk": mint(self.key, header_extra={"jwk": public_jwk(self.key)}),
            "header-unknown-param": mint(self.key, header_extra={"crit": ["exp"]}),
            "kid-missing": mint(self.key, kid=None),
            "kid-off-grammar-slash": mint(self.key, kid="../escape"),
            "kid-off-grammar-double-colon": mint(self.key, kid="a::b"),
            "kid-off-grammar-space": mint(self.key, kid="pool a"),
            "kid-off-grammar-empty": mint(self.key, kid=""),
            "issuer-mismatch": mint(self.key, iss="someone-else"),
            "audience-mismatch": mint(self.key, aud="other-resolver@other.internal"),
            "ttl-exceeds-3600": mint(self.key, exp=DEFAULT_NOW + 3601),
            "expired-beyond-leeway": mint(self.key, exp=DEFAULT_NOW - 31),
            "iat-far-future": mint(self.key, iat=DEFAULT_NOW + 120),
            "sub-empty": mint(self.key, sub=""),
            "wrong-signing-key": mint(self.key, signer=self.other_key),
            "kid-points-at-other-key": mint(self.key, kid=OTHER_KEY_KID),
            "not-three-segments": "only.two",
            "padded-base64": "aGVhZGVy.eGFtcGxl.c2ln",
            "payload-not-object": "e30.WyJhcnJheSJd.c2ln",
        }
        for name, token in cases.items():
            with self.subTest(name=name):
                reason, _ = self.verify(token)
                self.assertEqual(CREDENTIAL_INVALID, reason, name)

    def test_typ_absent_is_accepted(self) -> None:
        token = mint(self.key, typ=None)
        self.assertEqual("ok", self.verify(token)[0])

    def test_off_grammar_kid_never_reaches_the_key_provider(self) -> None:
        # OSEP 450: rejected locally with no fetch — zero provider calls.
        self.verify(mint(self.key, kid="a/b"))
        self.verify(mint(self.key, kid="a?b"))
        self.verify(mint(self.key, kid="..%2f"))
        self.verify(mint(self.key, kid=":" + KID))
        self.assertEqual([], self.provider.calls)

    def test_unknown_kid_maps_to_invalid(self) -> None:
        token = mint(self.key, kid="pool-z:v9")
        self.assertEqual(CREDENTIAL_INVALID, self.verify(token)[0])
        self.assertEqual(["pool-z:v9"], self.provider.calls)

    def test_sidecar_sandbox_kid_is_valid(self) -> None:
        token = mint(self.key, kid="sbx_01HZYEXAMPLE")
        self.assertEqual("ok", self.verify(token)[0])

    # -- time-window rules (OSEP 385): exp-iat <= 3600, now <= exp+30,
    #    iat <= now+30 — checked at both leeway boundaries.

    def test_ttl_exactly_3600_is_accepted(self) -> None:
        token = mint(self.key, exp=DEFAULT_NOW + 3600)
        self.assertEqual("ok", self.verify(token)[0])

    def test_expiry_leeway_boundary(self) -> None:
        token = mint(self.key, exp=DEFAULT_NOW - 30)
        self.assertEqual("ok", self.verify(token)[0])
        self.clock.now = DEFAULT_NOW + 0.5
        self.assertEqual(CREDENTIAL_INVALID, self.verify(token)[0])

    def test_iat_leeway_boundary(self) -> None:
        future = mint(self.key, iat=DEFAULT_NOW + 30, exp=DEFAULT_NOW + 3630)
        self.assertEqual("ok", self.verify(future)[0])
        too_far = mint(self.key, iat=DEFAULT_NOW + 31, exp=DEFAULT_NOW + 3631)
        self.assertEqual(CREDENTIAL_INVALID, self.verify(too_far)[0])

    # -- subject binding (OSEP 357, 384): signed sub is authoritative.

    def test_subject_mismatch_maps_to_its_own_reason(self) -> None:
        token = mint(self.key, sub="sbx-someone-else")
        reason, detail = self.verify(token, subject=SUBJECT)
        self.assertEqual(SUBJECT_MISMATCH, reason)
        self.assertIn("subject", detail)

    def test_subject_mismatch_even_before_signature_trust(self) -> None:
        # A token signed by the wrong key AND claiming another subject is
        # rejected either way; the local binding check runs before any
        # key fetch, so no provider lookup happens for untrusted input.
        token = mint(self.other_key, sub="sbx-someone-else", kid="pool-z:v9")
        self.assertEqual(SUBJECT_MISMATCH, self.verify(token)[0])
        self.assertEqual([], self.provider.calls)

    # -- deliberate omissions (OSEP 389): no nbf, no role claims, no
    #    jti dedup; unknown payload claims ignored.

    def test_unknown_payload_claims_are_ignored(self) -> None:
        token = mint(self.key, payload_extra={"role": "admin", "tenant": "x", "scope": "all"})
        self.assertEqual("ok", self.verify(token)[0])

    def test_nbf_is_not_enforced(self) -> None:
        token = mint(self.key, payload_extra={"nbf": DEFAULT_NOW + 10_000})
        self.assertEqual("ok", self.verify(token)[0])

    def test_jti_is_not_deduplicated(self) -> None:
        token = mint(self.key)
        self.assertEqual("ok", self.verify(token)[0])
        self.assertEqual("ok", self.verify(token)[0])
        self.assertEqual("ok", self.verify(token)[0])

    def test_missing_jti_is_not_rejected(self) -> None:
        token = mint(self.key, jti=None)
        verified = self.verifier.verify(token, subject=SUBJECT)
        self.assertIsNone(verified.jti)

    # -- structural strictness on attacker-controlled JSON.

    def test_duplicate_payload_member_rejected(self) -> None:
        body = (
            '{"iss": "opensandbox", '
            '"aud": "credential-resolver@resolver.internal", '
            f'"sub": "{SUBJECT}", "iat": {DEFAULT_NOW}, '
            f'"exp": {DEFAULT_NOW + 3600}, "exp": {DEFAULT_NOW + 7200}, '
            '"jti": "abc"}'
        )
        header_b64 = b64url(json.dumps({"alg": "EdDSA", "kid": KID, "typ": "JWT"}).encode())
        payload_b64 = b64url(body.encode())
        signing_input = f"{header_b64}.{payload_b64}".encode()
        token = f"{header_b64}.{payload_b64}.{b64url(self.key.sign(signing_input))}"
        self.assertEqual(CREDENTIAL_INVALID, self.verify(token)[0])

    def test_fractional_and_boolean_times_rejected(self) -> None:
        float_iat = mint(self.key, iat=float(DEFAULT_NOW))
        self.assertEqual(CREDENTIAL_INVALID, self.verify(float_iat)[0])
        bool_exp = mint(self.key, exp=True)
        self.assertEqual(CREDENTIAL_INVALID, self.verify(bool_exp)[0])

    def test_aud_as_list_rejected(self) -> None:
        token = mint(self.key, aud=["credential-resolver@resolver.internal"])
        self.assertEqual(CREDENTIAL_INVALID, self.verify(token)[0])


class VerifierConfigTest(unittest.TestCase):
    def test_empty_issuer_or_audience_rejected(self) -> None:
        provider = StaticKeyProvider({})
        for kwargs in ({"expected_issuer": "", "expected_aud": "a"}, {"expected_issuer": "i", "expected_aud": ""}):
            with self.subTest(kwargs=kwargs):
                with self.assertRaises(ValueError):
                    CredentialVerifier(key_provider=provider, **kwargs)


if __name__ == "__main__":
    unittest.main()
