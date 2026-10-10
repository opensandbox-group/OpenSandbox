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

from __future__ import annotations

import json
from typing import Any, Literal

from itsdangerous import BadSignature, URLSafeTimedSerializer

from app.config import Settings

Role = Literal["tenant", "admin"]


def _serializer(settings: Settings) -> URLSafeTimedSerializer:
    return URLSafeTimedSerializer(settings.bff_session_secret, salt="opensandbox-console")


def encode_session(settings: Settings, payload: dict[str, Any]) -> str:
    return _serializer(settings).dumps(payload)


def decode_session(settings: Settings, token: str) -> dict[str, Any] | None:
    try:
        data = _serializer(settings).loads(token, max_age=settings.bff_session_max_age_seconds)
    except BadSignature:
        return None
    if not isinstance(data, dict):
        return None
    return data


def tenant_session(tenant: str, namespace: str) -> dict[str, Any]:
    return {"role": "tenant", "tenant": tenant, "namespace": namespace}


def admin_session() -> dict[str, Any]:
    return {"role": "admin"}
