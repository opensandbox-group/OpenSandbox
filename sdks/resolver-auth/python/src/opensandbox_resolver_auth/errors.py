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

"""Error types and the frozen 401 reason codes (OSEP-0025).

Resolvers map every verification failure to HTTP 401 — never 500 or 403 —
and echo ``reason`` in the JSON body so integration debugging (Stage 2a)
can distinguish the three failure classes.
"""

from __future__ import annotations

__all__ = [
    "CREDENTIAL_MISSING",
    "CREDENTIAL_INVALID",
    "SUBJECT_MISMATCH",
    "VerificationError",
    "UnknownSigningKeyError",
]

# No ``OpenSandbox-Egress-Credential`` header on the request.
CREDENTIAL_MISSING = "credential_missing"
# Header present but failed any format, signature, key, or time-window rule.
CREDENTIAL_INVALID = "credential_invalid"
# Verified token ``sub`` differs from the resolve document's ``subject``.
SUBJECT_MISMATCH = "subject_mismatch"


class VerificationError(Exception):
    """A caller credential is missing, invalid, or bound to another subject.

    ``reason`` is one of the three frozen reason codes and belongs in the
    401 response body; ``detail`` explains the failure and is intended for
    resolver-side logs only.
    """

    def __init__(self, reason: str, detail: str = "") -> None:
        super().__init__(detail or reason)
        self.reason = reason
        self.detail = detail


class UnknownSigningKeyError(Exception):
    """A ``kid`` has no verifiable key behind the configured JWKS endpoint.

    Raised by key providers; the verifier translates it into a
    ``credential_invalid`` verification failure.
    """
