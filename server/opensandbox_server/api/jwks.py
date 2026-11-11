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

"""Public JWKS endpoint for credential-vault resolver caller authentication.

``GET /credential-vault/jwks/{kid}`` serves one public Ed25519 JWK that
resolvers use to verify ``OpenSandbox-Egress-Credential`` headers. Public by
design (public keys only, the OIDC jwks.json posture), exempt from API key
auth; request rate limiting is the trusted ingress/API gateway's job. All
logic lives in services.jwks.
"""

from __future__ import annotations

from typing import Optional

from fastapi import APIRouter, HTTPException, Request

from opensandbox_server.services.jwks import JWKSBackendError

import logging

logger = logging.getLogger(__name__)

router = APIRouter(tags=["Credential Vault"])


def _service(request: Request):
    service = getattr(request.app.state, "jwks_service", None)
    if service is None:
        raise HTTPException(status_code=501, detail="jwks publication is not configured")
    return service


@router.get(
    "/credential-vault/jwks/{kid}",
    responses={
        200: {"description": "The JWK for this kid (public key only)"},
        404: {"description": "Unknown kid (off-grammar kids included)"},
        501: {"description": "JWKS publication is not configured on this server (no egress configuration)"},
        503: {"description": "Transient backend failure; retry later"},
    },
)
def get_jwk(kid: str, request: Request) -> dict:
    """Publish one egress caller-authentication public key."""
    try:
        jwk: Optional[dict] = _service(request).get(kid)
    except JWKSBackendError:
        # Generic public detail: the endpoint is unauthenticated, and the
        # exception text can carry Secret names and API error internals.
        logger.warning("jwks lookup failed", exc_info=True)
        raise HTTPException(status_code=503, detail="jwks backend temporarily unavailable") from None
    if jwk is None:
        raise HTTPException(status_code=404, detail="unknown kid")
    return jwk
