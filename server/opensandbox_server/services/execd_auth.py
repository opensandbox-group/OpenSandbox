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

"""Stateless HMAC-SHA256 token derivation and SHA-256 verifier helpers for execd auth."""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import logging
import os
import re
import secrets
import threading
import time
from dataclasses import dataclass
from typing import Dict, Optional

from opensandbox_server.config import ExecdConfig
from opensandbox_server.services.constants import OPEN_SANDBOX_EXECD_ACCESS_TOKEN_HEADER

logger = logging.getLogger(__name__)

_KEY_ID_PATTERN = re.compile(r"^[0-9a-zA-Z_-]{1,16}$")
_RELOAD_CHECK_INTERVAL_SECONDS = 2.0
_MIN_MASTER_KEY_BYTES = 32
_NONCE_BYTES = 16
HASH_PREFIX = "sha256:"


def _derive_raw_token(master_key: bytes, nonce: str) -> str:
    mac = hmac.new(master_key, nonce.encode("utf-8"), hashlib.sha256).digest()
    return base64.urlsafe_b64encode(mac).rstrip(b"=").decode("ascii")


def _hash_raw_token(raw_token: str) -> str:
    return hashlib.sha256(raw_token.encode("utf-8")).hexdigest()


def _parse_master_key(raw: str, kid: str) -> bytes:
    text = raw.strip()
    if not text:
        raise ValueError(f"Empty master key for kid '{kid}'")
    try:
        key_bytes = base64.b64decode(text, validate=True)
    except Exception as exc:
        raise ValueError(f"Invalid base64 master key for kid '{kid}'") from exc
    if len(key_bytes) < _MIN_MASTER_KEY_BYTES:
        raise ValueError(
            f"Master key for kid '{kid}' must be at least {_MIN_MASTER_KEY_BYTES} bytes, "
            f"got {len(key_bytes)}"
        )
    return key_bytes


@dataclass(frozen=True)
class _MasterKeyRing:
    active_kid: str
    keys: Dict[str, bytes]
    mtime_ns: int
    ino: int


class ExecdTokenManager:
    """Derives per-sandbox execd tokens from versioned master keys.

    Master keys are loaded from a read-only file (typically mounted from a K8s
    Secret) and hot-reloaded when the file's mtime/inode changes. Only the
    one-way verifier ``sha256:<kid>:<nonce>:<verifier>`` is placed in Pod env;
    the raw token is recomputed on demand during ``GetEndpoint(44772)``.
    """

    def __init__(
        self,
        config: ExecdConfig,
        *,
        reload_check_interval: float = _RELOAD_CHECK_INTERVAL_SECONDS,
    ) -> None:
        if not config.auth_signing_key_file:
            raise ValueError("execd.auth_signing_key_file is required")
        self._key_file = config.auth_signing_key_file
        self._reload_check_interval = reload_check_interval
        self._lock = threading.Lock()
        self._last_check_monotonic = time.monotonic()
        self._ring = self._load_key_ring()

    def _load_key_ring(self) -> _MasterKeyRing:
        st = os.stat(self._key_file)
        with open(self._key_file, "r", encoding="utf-8") as fh:
            data = json.load(fh)

        if not isinstance(data, dict):
            raise ValueError("execd master key file must contain a JSON object")

        active_kid = data.get("active")
        raw_keys = data.get("keys")
        if not isinstance(active_kid, str) or not _KEY_ID_PATTERN.match(active_kid):
            raise ValueError(
                "execd master key file 'active' must match ^[0-9a-zA-Z_-]{1,16}$"
            )
        if not isinstance(raw_keys, dict) or not raw_keys:
            raise ValueError("execd master key file 'keys' must be a non-empty object")

        parsed: Dict[str, bytes] = {}
        for kid, raw_val in raw_keys.items():
            if not isinstance(kid, str) or not _KEY_ID_PATTERN.match(kid):
                raise ValueError(f"Invalid execd master key id '{kid}'")
            if not isinstance(raw_val, str):
                raise ValueError(f"Master key for kid '{kid}' must be a string")
            parsed[kid] = _parse_master_key(raw_val, kid)

        if active_kid not in parsed:
            raise ValueError(f"Active execd master key '{active_kid}' not found in 'keys'")

        return _MasterKeyRing(
            active_kid=active_kid,
            keys=parsed,
            mtime_ns=st.st_mtime_ns,
            ino=st.st_ino,
        )

    def _current_ring(self) -> _MasterKeyRing:
        now_mono = time.monotonic()
        if now_mono - self._last_check_monotonic < self._reload_check_interval:
            return self._ring

        with self._lock:
            now_mono = time.monotonic()
            if now_mono - self._last_check_monotonic < self._reload_check_interval:
                return self._ring
            self._last_check_monotonic = now_mono

            try:
                st = os.stat(self._key_file)
                if st.st_mtime_ns != self._ring.mtime_ns or st.st_ino != self._ring.ino:
                    new_ring = self._load_key_ring()
                    self._ring = new_ring
                    logger.info(
                        "Reloaded execd master key ring from %s (active=%s, count=%d)",
                        self._key_file,
                        new_ring.active_kid,
                        len(new_ring.keys),
                    )
            except Exception as exc:
                logger.warning(
                    "Failed to reload execd master key ring from %s; keeping previous keys: %s",
                    self._key_file,
                    exc,
                )
            return self._ring

    def mint_env_value(self, *, nonce: Optional[str] = None) -> str:
        """Mint ``sha256:<kid>:<nonce>:<verifier>`` for ``EXECD_ACCESS_TOKEN``."""
        ring = self._current_ring()
        kid = ring.active_kid
        effective_nonce = nonce if nonce is not None else secrets.token_urlsafe(_NONCE_BYTES)
        if not effective_nonce or ":" in effective_nonce:
            raise ValueError("nonce must be non-empty and must not contain ':'")
        raw_token = _derive_raw_token(ring.keys[kid], effective_nonce)
        verifier = _hash_raw_token(raw_token)
        return f"{HASH_PREFIX}{kid}:{effective_nonce}:{verifier}"

    def resolve_raw_token(self, env_value: str) -> str:
        """Derive the raw token from ``sha256:<kid>:<nonce>:<verifier>`` or return legacy plaintext.

        Rotation contract: operators must retain old ``kid`` entries in the key ring
        file until every sandbox minted under that ``kid`` has terminated; removing an
        in-use ``kid`` fails closed with ``ValueError``.
        """
        trimmed = env_value.strip()
        if not trimmed.startswith(HASH_PREFIX):
            return trimmed

        parts = trimmed.split(":")
        if len(parts) != 4 or not parts[1] or not parts[2] or not parts[3]:
            raise ValueError("Malformed sha256 EXECD_ACCESS_TOKEN format")

        _, kid, nonce, expected_verifier = parts
        ring = self._current_ring()
        master_key = ring.keys.get(kid)
        if master_key is None:
            raise ValueError(f"Unknown execd master key id '{kid}'")

        raw_token = _derive_raw_token(master_key, nonce)
        actual_verifier = _hash_raw_token(raw_token)
        if not hmac.compare_digest(actual_verifier, expected_verifier.lower()):
            raise ValueError(f"Derived execd token verifier mismatch for kid '{kid}'")
        return raw_token


def build_execd_auth_headers(token: str) -> Dict[str, str]:
    """Build endpoint headers carrying the execd access token."""
    return {OPEN_SANDBOX_EXECD_ACCESS_TOKEN_HEADER: token}


__all__ = [
    "HASH_PREFIX",
    "ExecdTokenManager",
    "build_execd_auth_headers",
]
