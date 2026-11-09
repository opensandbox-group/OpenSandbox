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

"""Strict compact-JWS parsing for the resolver caller credential.

Only the parsing envelope lives here; header, claim, and signature rules
are in :mod:`opensandbox_resolver_auth.verifier`. Anything that does not
match the compact serialization exactly — segment count, unpadded
base64url, duplicate JSON members, non-finite numbers — raises
:class:`VerificationError` with reason ``credential_invalid``.
"""

from __future__ import annotations

import base64
import json
from dataclasses import dataclass
from typing import Any

from .errors import CREDENTIAL_INVALID, VerificationError

__all__ = ["ParsedJws", "parse_compact_jws", "decode_base64url"]

_B64URL_ALPHABET = frozenset(
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
)


def decode_base64url(value: str) -> bytes:
    """Decode one unpadded base64url segment (RFC 7515), strictly.

    Literal ``=`` padding, non-alphabet characters, and truncated
    quanta raise ``ValueError``.
    """
    if not value or not _B64URL_ALPHABET.issuperset(value):
        raise ValueError("not unpadded base64url")
    if len(value) % 4 == 1:
        # A trailing single character cannot form a valid base64 quantum;
        # the segment is truncated, not merely unpadded.
        raise ValueError("truncated base64url")
    # The alphabet check above has already excluded literal "="
    # characters, so re-padding for the standard decoder is unambiguous.
    padded = value + "=" * ((-len(value)) % 4)
    return base64.b64decode(padded, altchars=b"-_", validate=True)


@dataclass(frozen=True)
class ParsedJws:
    """Decoded compact JWS parts, unverified."""

    header: dict[str, Any]
    payload: dict[str, Any]
    # ASCII bytes of ``header_b64 + "." + payload_b64`` — the exact input
    # the Ed25519 signature covers.
    signing_input: bytes
    signature: bytes


def _reject_constant(value: str) -> Any:
    raise ValueError(f"non-finite JSON number: {value}")


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON member: {key}")
        result[key] = value
    return result


def _decode_segment(segment: str, what: str) -> bytes:
    try:
        return decode_base64url(segment)
    except ValueError as error:
        raise VerificationError(CREDENTIAL_INVALID, f"malformed {what} segment") from error


def _decode_json(raw: bytes, what: str) -> dict[str, Any]:
    try:
        value = json.loads(
            raw,
            object_pairs_hook=_unique_object,
            parse_constant=_reject_constant,
        )
    except (
        UnicodeDecodeError,
        json.JSONDecodeError,
        ValueError,
        RecursionError,
    ) as error:
        raise VerificationError(CREDENTIAL_INVALID, f"malformed {what} JSON") from error
    if type(value) is not dict:
        raise VerificationError(CREDENTIAL_INVALID, f"{what} is not a JSON object")
    return value


def parse_compact_jws(token: object) -> ParsedJws:
    """Parse one compact JWS into unverified parts.

    ``token`` must be the three-segment base64url serialization; every
    deviation raises :class:`VerificationError`.
    """
    if not isinstance(token, str) or not token:
        raise VerificationError(CREDENTIAL_INVALID, "empty credential")
    parts = token.split(".")
    if len(parts) != 3:
        raise VerificationError(CREDENTIAL_INVALID, "credential is not a compact JWS")
    header_b64, payload_b64, signature_b64 = parts
    header = _decode_json(_decode_segment(header_b64, "header"), "header")
    payload = _decode_json(_decode_segment(payload_b64, "payload"), "payload")
    signature = _decode_segment(signature_b64, "signature")
    signing_input = f"{header_b64}.{payload_b64}".encode("ascii")
    return ParsedJws(
        header=header,
        payload=payload,
        signing_input=signing_input,
        signature=signature,
    )
