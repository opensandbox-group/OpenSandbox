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

"""Compact-JWS parsing strictness tests."""

from __future__ import annotations

import unittest

from vectors import (
    DEFAULT_ISSUER,
    DEFAULT_KID,
    DEFAULT_SUBJECT,
    b64url,
    generate_signing_key,
    mint,
)

from opensandbox_resolver_auth import CREDENTIAL_INVALID, VerificationError
from opensandbox_resolver_auth.jws import parse_compact_jws


class ParseCompactJwsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.key = generate_signing_key()
        self.token = mint(self.key)

    def parse(self, token: object) -> tuple[str, str]:
        try:
            parse_compact_jws(token)
        except VerificationError as error:
            return error.reason, error.detail
        return "ok", ""

    def test_valid_token_round_trips(self) -> None:
        parsed = parse_compact_jws(self.token)
        self.assertEqual({"alg": "EdDSA", "kid": DEFAULT_KID, "typ": "JWT"}, parsed.header)
        self.assertEqual(DEFAULT_ISSUER, parsed.payload["iss"])
        self.assertEqual(DEFAULT_SUBJECT, parsed.payload["sub"])
        # signing_input must be the exact ASCII of the first two segments.
        header_b64, payload_b64, _ = self.token.split(".")
        self.assertEqual(
            f"{header_b64}.{payload_b64}".encode("ascii"), parsed.signing_input
        )
        self.assertEqual(64, len(parsed.signature))

    def test_rejects_non_string_input(self) -> None:
        for bad in (None, 42, b"bytes", ["a.b.c"]):
            self.assertEqual(CREDENTIAL_INVALID, self.parse(bad)[0], repr(bad))

    def test_rejects_wrong_segment_count(self) -> None:
        for bad in ("", "a", "a.b", "a.b.c.d", ".", "a..", ".."):
            self.assertEqual(CREDENTIAL_INVALID, self.parse(bad)[0], repr(bad))

    def test_rejects_padding_and_illegal_characters(self) -> None:
        header_b64, payload_b64, sig_b64 = self.token.split(".")
        padded = f"{header_b64}=.{payload_b64}.{sig_b64}"
        self.assertEqual(CREDENTIAL_INVALID, self.parse(padded)[0])
        # Standard-alphabet characters are illegal in base64url segments.
        standard_b64 = f"{header_b64}.{payload_b64[:-1]}+.{sig_b64}"
        self.assertEqual(CREDENTIAL_INVALID, self.parse(standard_b64)[0])
        standard_slash = f"{header_b64}.{payload_b64[:-1]}/{sig_b64}"
        self.assertEqual(CREDENTIAL_INVALID, self.parse(standard_slash)[0])

    def test_rejects_non_utf8_and_non_object_json(self) -> None:
        header_b64, payload_b64, sig_b64 = self.token.split(".")
        # 0xff alone is invalid UTF-8.
        invalid_utf8 = b64url(b"\xff\xfe")
        self.assertEqual(
            CREDENTIAL_INVALID, self.parse(f"{invalid_utf8}.{payload_b64}.{sig_b64}")[0]
        )
        array_payload = b64url(b'[1,2,3]')
        self.assertEqual(
            CREDENTIAL_INVALID, self.parse(f"{header_b64}.{array_payload}.{sig_b64}")[0]
        )

    def test_rejects_non_finite_numbers(self) -> None:
        header_b64, payload_b64, sig_b64 = self.token.split(".")
        nan_payload = b64url(b'{"iat": NaN}')
        self.assertEqual(
            CREDENTIAL_INVALID, self.parse(f"{header_b64}.{nan_payload}.{sig_b64}")[0]
        )


if __name__ == "__main__":
    unittest.main()
