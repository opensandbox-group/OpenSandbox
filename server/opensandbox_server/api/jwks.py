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

"""Public JWKS endpoint for egress caller-authentication keys (OSEP-0025 D5).

``GET /jwks/{kid}`` serves one public Ed25519 JWK. The endpoint is public by
design (public keys only — the OIDC jwks.json posture) and is exempt from API
key auth; abuse is bounded by kid-grammar rejection and the service's lookup
budget (429). Route stays thin: all logic lives in services.jwks.
"""

from __future__ import annotations

from typing import Optional

from fastapi import APIRouter, HTTPException, Request

from opensandbox_server.services.jwks import RateLimited

router = APIRouter(tags=["JWKS"])


def _service(request: Request):
    service = getattr(request.app.state, "jwks_service", None)
    if service is None:
        raise HTTPException(status_code=501, detail="jwks publication is not configured")
    return service


@router.get(
    "/jwks/{kid}",
    responses={
        200: {"description": "The JWK for this kid (public key only)"},
        404: {"description": "Unknown kid (off-grammar kids included)"},
        429: {"description": "Lookup budget exhausted; retry later"},
        501: {"description": "No signing Secrets configured"},
    },
)
def get_jwk(kid: str, request: Request) -> dict:
    """Publish one egress caller-authentication public key."""
    try:
        jwk: Optional[dict] = _service(request).get(kid)
    except RateLimited:
        raise HTTPException(status_code=429, detail="jwks lookup budget exhausted") from None
    if jwk is None:
        raise HTTPException(status_code=404, detail="unknown kid")
    return jwk
