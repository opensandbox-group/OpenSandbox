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

"""Tests for OSEP-0011 signing module."""

from __future__ import annotations

import hashlib
import struct

import pytest

from opensandbox_server.services.signing import (
    MAX_EXPIRES_B36_LEN,
    MAX_UINT64,
    build_canonical_bytes,
    compute_hex8,
    compute_signature,
    decode_expires_b36,
    encode_expires_b36,
)

# ============================================================
# encode_expires_b36
# ============================================================


class TestEncodeExpiresB36:
    def test_zero_returns_literal_zero(self) -> None:
        assert encode_expires_b36(0) == "0"

    def test_one_returns_one(self) -> None:
        assert encode_expires_b36(1) == "1"

    def test_35_returns_z(self) -> None:
        assert encode_expires_b36(35) == "z"

    def test_36_returns_10(self) -> None:
        assert encode_expires_b36(36) == "10"

    def test_2000000000_returns_x2qxvk(self) -> None:
        assert encode_expires_b36(2000000000) == "x2qxvk"

    def test_max_uint64(self) -> None:
        encoded = encode_expires_b36(MAX_UINT64)
        assert encoded == "3w5e11264sgsf"
        assert len(encoded) == MAX_EXPIRES_B36_LEN

    def test_no_leading_zeros(self) -> None:
        for n in [0, 1, 36, 1000, 2**63]:
            s = encode_expires_b36(n)
            assert s == s.lstrip("0") or "0"

    def test_negative_raises_value_error(self) -> None:
        with pytest.raises(ValueError, match="non-negative"):
            encode_expires_b36(-1)

    def test_overflow_raises_value_error(self) -> None:
        with pytest.raises(ValueError, match="exceeds uint64"):
            encode_expires_b36(MAX_UINT64 + 1)


# ============================================================
# decode_expires_b36
# ============================================================


class TestDecodeExpiresB36:
    def test_zero(self) -> None:
        assert decode_expires_b36("0") == 0

    def test_one(self) -> None:
        assert decode_expires_b36("1") == 1

    def test_z(self) -> None:
        assert decode_expires_b36("z") == 35

    def test_10(self) -> None:
        assert decode_expires_b36("10") == 36

    def test_x2qxvk(self) -> None:
        assert decode_expires_b36("x2qxvk") == 2000000000

    def test_max_uint64(self) -> None:
        assert decode_expires_b36("3w5e11264sgsf") == MAX_UINT64

    def test_roundtrip(self) -> None:
        for val in [0, 1, 36, 100, 2000000000, 2**32, 2**63, MAX_UINT64]:
            assert decode_expires_b36(encode_expires_b36(val)) == val

    def test_empty_raises(self) -> None:
        with pytest.raises(ValueError, match="empty"):
            decode_expires_b36("")

    def test_too_long_raises(self) -> None:
        with pytest.raises(ValueError, match="too long"):
            decode_expires_b36("0" * (MAX_EXPIRES_B36_LEN + 1))

    def test_leading_zeros_raises(self) -> None:
        with pytest.raises(ValueError, match="leading zeros"):
            decode_expires_b36("01")

    def test_invalid_characters_raises(self) -> None:
        with pytest.raises(ValueError, match="invalid characters"):
            decode_expires_b36("x2qxvk!")

    def test_uppercase_raises(self) -> None:
        with pytest.raises(ValueError, match="invalid characters"):
            decode_expires_b36("X2QXvk")

    def test_uint64_overflow_raises(self) -> None:
        with pytest.raises(ValueError, match="overflows uint64"):
            decode_expires_b36("3w5e11264sgsg")  # MAX_UINT64 + 1 in base36


# ============================================================
# build_canonical_bytes
# ============================================================


class TestBuildCanonicalBytes:
    def test_basic_format(self) -> None:
        result = build_canonical_bytes("my-sandbox", 8080, "x2qxvk")
        assert result == b"v1\nshort\nmy-sandbox\n8080\nx2qxvk\n"

    def test_sandbox_id_with_hyphens(self) -> None:
        result = build_canonical_bytes("sbx-abc-123", 44772, "abc123")
        assert result == b"v1\nshort\nsbx-abc-123\n44772\nabc123\n"

    def test_port_1(self) -> None:
        result = build_canonical_bytes("s", 1, "0")
        assert result == b"v1\nshort\ns\n1\n0\n"

    def test_port_65535(self) -> None:
        result = build_canonical_bytes("s", 65535, "0")
        assert result == b"v1\nshort\ns\n65535\n0\n"

    def test_port_zero_raises(self) -> None:
        with pytest.raises(ValueError, match="port must be 1-65535"):
            build_canonical_bytes("s", 0, "0")

    def test_port_65536_raises(self) -> None:
        with pytest.raises(ValueError, match="port must be 1-65535"):
            build_canonical_bytes("s", 65536, "0")

    def test_expires_b36_empty_string(self) -> None:
        """Empty string is syntactically allowed by build_canonical_bytes
        (validation is the caller's responsibility via decode_expires_b36)."""
        result = build_canonical_bytes("s", 80, "")
        assert result == b"v1\nshort\ns\n80\n\n"


# ============================================================
# compute_hex8 / compute_signature
# ============================================================


class TestComputeHex8:
    def test_length(self) -> None:
        result = compute_hex8(b"secret", b"canonical")
        assert len(result) == 8

    def test_is_lowercase_hex(self) -> None:
        result = compute_hex8(b"secret", b"canonical")
        assert all(c in "0123456789abcdef" for c in result)

    def test_deterministic(self) -> None:
        secret = b"my-secret-key"
        canonical = b"v1\nshort\nsandbox-1\n8080\nx2qxvk\n"
        assert compute_hex8(secret, canonical) == compute_hex8(secret, canonical)

    def test_different_secret_produces_different_result(self) -> None:
        canonical = b"v1\nshort\nsandbox-1\n8080\nx2qxvk\n"
        assert compute_hex8(b"key-a", canonical) != compute_hex8(b"key-b", canonical)

    def test_different_canonical_produces_different_result(self) -> None:
        secret = b"my-secret"
        assert compute_hex8(secret, b"canonical-a") != compute_hex8(secret, b"canonical-b")

    def test_known_inner_structure(self) -> None:
        """Verify the inner byte layout matches the OSEP-0011 spec."""
        secret = b"ab"
        canonical = b"test"
        # inner = BE32(2) || b"ab" || BE32(4) || b"test"
        inner = struct.pack(">I", 2) + b"ab" + struct.pack(">I", 4) + b"test"
        expected_digest = hashlib.sha256(inner).hexdigest()[:8]
        assert compute_hex8(secret, canonical) == expected_digest

    def test_empty_secret(self) -> None:
        """Empty secret should still produce a valid hex8."""
        result = compute_hex8(b"", b"canonical")
        assert len(result) == 8
        assert all(c in "0123456789abcdef" for c in result)

    def test_empty_canonical(self) -> None:
        result = compute_hex8(b"secret", b"")
        assert len(result) == 8


class TestComputeSignature:
    def test_length(self) -> None:
        sig = compute_signature(b"secret", "a", b"canonical")
        assert len(sig) == 9

    def test_ends_with_key_id(self) -> None:
        sig = compute_signature(b"secret", "k", b"canonical")
        assert sig[-1] == "k"

    def test_hex8_is_first_eight_of_digest(self) -> None:
        """verify signature[:8] == compute_hex8(secret, canonical)."""
        secret, canonical = b"my-secret", b"v1\nshort\ns\n80\nabc\n"
        hex8 = compute_hex8(secret, canonical)
        sig = compute_signature(secret, "f", canonical)
        assert sig[:8] == hex8

    @pytest.mark.parametrize("key_id", ["0", "a", "z", "5"])
    def test_various_key_ids(self, key_id: str) -> None:
        sig = compute_signature(b"secret", key_id, b"canonical")
        assert sig == compute_hex8(b"secret", b"canonical") + key_id



# ============================================================
# Integration: end-to-end signing flow
# ============================================================


class TestEndToEndSigning:
    def test_sign_and_verify_flow(self) -> None:
        """Verify the full signing pipeline: expires_b36 -> canonical -> signature."""
        sandbox_id = "my-sandbox"
        port = 8080
        expires_sec = 2000000000
        secret = b"my-base64-decoded-secret!"
        key_id = "a"

        expires_b36 = encode_expires_b36(expires_sec)
        assert expires_b36 == "x2qxvk"

        canonical = build_canonical_bytes(sandbox_id, port, expires_b36)
        assert canonical == b"v1\nshort\nmy-sandbox\n8080\nx2qxvk\n"

        signature = compute_signature(secret, key_id, canonical)
        assert len(signature) == 9
        assert signature[-1] == key_id

    def test_different_secrets_produce_different_signatures(self) -> None:
        sandbox_id = "s"
        port = 80
        expires_b36 = "abc"

        canonical = build_canonical_bytes(sandbox_id, port, expires_b36)
        sig_a = compute_signature(b"secret-a", "a", canonical)
        sig_b = compute_signature(b"secret-b", "b", canonical)

        assert sig_a != sig_b
