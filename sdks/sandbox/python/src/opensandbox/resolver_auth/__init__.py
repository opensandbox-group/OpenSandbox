#
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
#
"""Credential-vault resolver caller authentication (verification side).

Primary entry point: :class:`CredentialVaultCallerAuth` — one instance per
resolver process, one :meth:`CredentialVaultCallerAuth.verify_caller` call
per resolve request.
"""

from .client import CredentialVaultCallerAuth
from .verifier import (
    CREDENTIAL_HEADER,
    DEFAULT_ISSUER,
    KID_GRAMMAR,
    LEEWAY_SECONDS,
    TTL_MAX_SECONDS,
    VerifyError,
    VerifierConfig,
    verify_credential,
)

__all__ = [
    "CREDENTIAL_HEADER",
    "CredentialVaultCallerAuth",
    "DEFAULT_ISSUER",
    "KID_GRAMMAR",
    "LEEWAY_SECONDS",
    "TTL_MAX_SECONDS",
    "VerifyError",
    "VerifierConfig",
    "verify_credential",
]
