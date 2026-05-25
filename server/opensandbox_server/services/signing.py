# Copyright 2026 Alibaba Group Holding Ltd.
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

"""expires_b36 encoding/decoding and OSEP-0011 route signing.

Canonical format::

    v1\\nshort\\n{sandbox_id}\\n{port}\\n{expires_b36}\\n

Signature scheme (OSEP-0011)::

    inner     = BE32(len(secret)) || secret || BE32(len(canonical)) || canonical
    digest    = SHA256(inner)
    hex8      = hex(digest)[0:8]
    signature = hex8 + key_id                           # 9 chars
    route     = {sandbox_id}-{port}-{expires_b36}-{signature}

expires_b36 is base36-encoded uint64 epoch seconds (lowercase, no leading zeros).
"""

from __future__ import annotations

import hashlib
import struct
from typing import Final

_BASE36_CHARS: Final[str] = "0123456789abcdefghijklmnopqrstuvwxyz"
_BASE36_CHAR_SET: Final[set[str]] = set(_BASE36_CHARS)

MAX_EXPIRES_B36_LEN: Final[int] = 13
MAX_UINT64: Final[int] = 2**64 - 1

_CANONICAL_TEMPLATE: Final[str] = "v1\nshort\n{sandbox_id}\n{port}\n{expires_b36}\n"


def encode_expires_b36(expires_sec: int) -> str:
    """Encode a Unix epoch timestamp to base36 per OSEP-0011.

    Args:
        expires_sec: Non-negative Unix epoch seconds (uint64).

    Returns:
        Base36 lowercase string, no leading zeros.
        Returns ``"0"`` when *expires_sec* is 0.

    Raises:
        ValueError: If *expires_sec* is negative or exceeds uint64 range.
    """
    if expires_sec < 0:
        raise ValueError(f"expires_sec must be non-negative, got {expires_sec}")
    if expires_sec > MAX_UINT64:
        raise ValueError(f"expires_sec exceeds uint64 range: {expires_sec}")
    if expires_sec == 0:
        return "0"

    n = expires_sec
    chars: list[str] = []
    while n > 0:
        n, r = divmod(n, 36)
        chars.append(_BASE36_CHARS[r])
    return "".join(reversed(chars))


def decode_expires_b36(s: str) -> int:
    """Decode a base36 string to a Unix timestamp per OSEP-0011.

    Args:
        s: Base36 string (``[0-9a-z]{1,13}``, no leading zeros).

    Returns:
        Decoded integer.

    Raises:
        ValueError: If *s* is empty, contains invalid characters,
            exceeds 13 characters, has leading zeros, or overflows uint64.
    """
    if not s:
        raise ValueError("expires_b36 string must not be empty")
    if len(s) > MAX_EXPIRES_B36_LEN:
        raise ValueError(
            f"expires_b36 string too long: {len(s)} > {MAX_EXPIRES_B36_LEN}"
        )
    if not _is_valid_base36(s):
        invalid = [c for c in s if c not in _BASE36_CHAR_SET]
        raise ValueError(
            f"expires_b36 contains invalid characters: {invalid!r}"
        )
    if len(s) > 1 and s[0] == "0":
        raise ValueError(f"expires_b36 must not have leading zeros: {s!r}")

    val = int(s, 36)
    if val > MAX_UINT64:
        raise ValueError(f"expires_b36 value overflows uint64: {s!r}")
    return val


def _is_valid_base36(s: str) -> bool:
    return all(c in _BASE36_CHAR_SET for c in s)


def build_canonical_bytes(sandbox_id: str, port: int, expires_b36: str) -> bytes:
    """Build the canonical byte string for route signing.

    Format: ``v1\\nshort\\n{sandbox_id}\\n{port}\\n{expires_b36}\\n``

    Args:
        sandbox_id: Sandbox identifier (may contain hyphens).
        port: Port number (1--65535).
        expires_b36: Base36-encoded expiration (from :func:`encode_expires_b36`).

    Returns:
        UTF-8 encoded canonical bytes.

    Raises:
        ValueError: If *port* is out of range.
    """
    if not 1 <= port <= 65535:
        raise ValueError(f"port must be 1-65535, got {port}")
    text = _CANONICAL_TEMPLATE.format(
        sandbox_id=sandbox_id,
        port=port,
        expires_b36=expires_b36,
    )
    return text.encode("utf-8")


def _be32(x: int) -> bytes:
    return struct.pack(">I", x)


def compute_hex8(secret_bytes: bytes, canonical_bytes: bytes) -> str:
    """Compute the hex8 prefix of the SHA256 digest per OSEP-0011.

    ``inner = BE32(len(secret)) || secret || BE32(len(canonical)) || canonical``

    Args:
        secret_bytes: Signing key raw bytes.
        canonical_bytes: Canonical byte string from :func:`build_canonical_bytes`.

    Returns:
        First 8 lowercase hex characters of the digest.
    """
    inner = (
        _be32(len(secret_bytes))
        + secret_bytes
        + _be32(len(canonical_bytes))
        + canonical_bytes
    )
    digest = hashlib.sha256(inner).digest()
    return digest.hex()[:8]


def compute_signature(
    secret_bytes: bytes,
    key_id: str,
    canonical_bytes: bytes,
) -> str:
    """Compute the full OSEP-0011 signature: ``hex8 + key_id`` (9 chars).

    Args:
        secret_bytes: Signing key raw bytes.
        key_id: Single character ``[0-9a-z]`` key identifier.
        canonical_bytes: Canonical byte string from :func:`build_canonical_bytes`.

    Returns:
        9-character signature string.
    """
    return compute_hex8(secret_bytes, canonical_bytes) + key_id


__all__ = [
    "encode_expires_b36",
    "decode_expires_b36",
    "build_canonical_bytes",
    "compute_hex8",
    "compute_signature",
    "MAX_EXPIRES_B36_LEN",
    "MAX_UINT64",
]
