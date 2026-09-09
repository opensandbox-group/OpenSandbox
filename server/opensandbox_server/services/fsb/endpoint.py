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

import base64
import hashlib
import hmac
import unicodedata

from fastapi import HTTPException

from opensandbox_server.api.schema import Endpoint
from opensandbox_server.config import (
    GATEWAY_ROUTE_MODE_HEADER,
    GATEWAY_ROUTE_MODE_URI,
    INGRESS_MODE_GATEWAY,
    IngressConfig,
)
from opensandbox_server.services.constants import (
    OPEN_SANDBOX_INGRESS_HEADER,
    SandboxErrorCodes,
)


def build_endpoint(
    ingress: IngressConfig | None,
    namespace: str,
    sandbox_id: str,
    port: int,
    expires: int | None = None,
) -> Endpoint:
    if not 1 <= port <= 65535 or any(
        not value or any(unicodedata.category(char) == "Cc" for char in value)
        for value in (namespace, sandbox_id)
    ):
        raise HTTPException(
            status_code=400,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": "Fsb endpoints require valid namespace, sandbox ID and port.",
            },
        )
    if expires is not None:
        raise HTTPException(
            status_code=400,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": "Fsb route scopes do not support the expires parameter.",
            },
        )
    if (
        ingress is None
        or ingress.mode != INGRESS_MODE_GATEWAY
        or ingress.gateway is None
        or ingress.gateway.route.mode not in (GATEWAY_ROUTE_MODE_HEADER, GATEWAY_ROUTE_MODE_URI)
        or ingress.secure_access is None
    ):
        raise HTTPException(
            status_code=400,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    "Fsb endpoints require an ingress gateway with header or uri routing "
                    "and secure_access signing keys."
                ),
            },
        )
    signing = ingress.secure_access
    canonical = f"opensandbox-fsb-route-v1\n{namespace}\n{sandbox_id}\n{port}\n".encode()
    mac = hmac.new(signing.get_active_secret_bytes(), canonical, hashlib.sha256).digest()[:16]
    encoded_namespace, encoded_sandbox, encoded_mac = (
        base64.urlsafe_b64encode(value).rstrip(b"=").decode("ascii")
        for value in (namespace.encode(), sandbox_id.encode(), mac)
    )
    scope = f"f1.{encoded_namespace}.{encoded_sandbox}.{port}.{signing.active_key}.{encoded_mac}"
    if ingress.gateway.route.mode == GATEWAY_ROUTE_MODE_HEADER:
        return Endpoint(
            endpoint=ingress.gateway.address,
            headers={OPEN_SANDBOX_INGRESS_HEADER: scope},
        )
    return Endpoint(endpoint=f"{ingress.gateway.address.rstrip('/')}/{scope}")
