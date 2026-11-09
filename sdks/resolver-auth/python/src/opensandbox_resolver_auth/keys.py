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

"""Signing-key providers for the credential verifier."""

from __future__ import annotations

from collections.abc import Mapping

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

from .errors import UnknownSigningKeyError

__all__ = ["KeyProvider", "StaticKeyProvider"]


class KeyProvider:
    """Maps a ``kid`` to its Ed25519 public key.

    Implementations raise :class:`UnknownSigningKeyError` when no key can
    be established for the kid — an absent key is a denial, never a
    pass-through. The mapping must be thread-safe when the verifier is
    shared across request handlers.
    """

    def get(self, kid: str) -> Ed25519PublicKey:  # pragma: no cover - interface
        raise UnknownSigningKeyError(kid)


class StaticKeyProvider(KeyProvider):
    """In-process kid→key map for tests and single-key deployments."""

    def __init__(self, keys: Mapping[str, Ed25519PublicKey]) -> None:
        self._keys = dict(keys)

    def get(self, kid: str) -> Ed25519PublicKey:
        try:
            return self._keys[kid]
        except KeyError as error:
            raise UnknownSigningKeyError(kid) from error
