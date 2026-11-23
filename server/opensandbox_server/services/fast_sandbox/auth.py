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

import logging
import os
from pathlib import Path
import random
import time
from collections import namedtuple
from threading import Lock
from typing import Callable, Optional

import grpc
from kubernetes.client import AuthenticationV1TokenRequest, V1TokenRequestSpec

AUDIENCE = "fast-sandbox-fastpath"
DEFAULT_AUTH_NAMESPACE = "opensandbox-system"
DEFAULT_SERVER_CALLER_SA = "opensandbox-server"
_INCLUSTER_NAMESPACE_PATH = Path("/var/run/secrets/kubernetes.io/serviceaccount/namespace")

logger = logging.getLogger(__name__)


def resolve_auth_namespace() -> str:
    env_ns = os.getenv("POD_NAMESPACE", "").strip()
    if env_ns:
        return env_ns
    try:
        file_ns = _INCLUSTER_NAMESPACE_PATH.read_text(encoding="utf-8").strip()
        if file_ns:
            return file_ns
    except OSError:
        pass
    return DEFAULT_AUTH_NAMESPACE


class TokenSource:
    def __init__(
        self,
        api: Callable,
        namespace: Optional[str] = None,
        service_account: str = DEFAULT_SERVER_CALLER_SA,
    ):
        resolved_namespace = (
            resolve_auth_namespace() if namespace is None else namespace.strip()
        )
        if not resolved_namespace or not service_account:
            raise ValueError("FastPath caller namespace and ServiceAccount are required")
        self._api = api
        self._namespace = resolved_namespace
        self._service_account = service_account
        self._lock = Lock()
        self._token = ""
        self._expires = 0.0
        self._refresh = 0.0
        self._retry = 0.0
        self._backoff = 1.0
        self._now = time.time

    def token(self) -> str:
        with self._lock:
            now = self._now()
            if self._token and now < min(self._refresh, self._expires - 5):
                return self._token
            if now < self._retry:
                if self._token and now < self._expires - 5:
                    return self._token
                raise RuntimeError("FastPath TokenRequest retry pending; no usable credential")
            try:
                api = self._api()
                configuration = api.api_client.configuration
                if configuration.verify_ssl is not True or not configuration.host.startswith(
                    "https://"
                ):
                    raise ValueError("FastPath TokenRequest requires verified Kubernetes API TLS")
                issued = api.create_namespaced_service_account_token(
                    name=self._service_account,
                    namespace=self._namespace,
                    body=AuthenticationV1TokenRequest(
                        spec=V1TokenRequestSpec(
                            audiences=[AUDIENCE],
                            expiration_seconds=3600,
                        )
                    ),
                    _request_timeout=(3, 5),
                )
                now = self._now()
                expires = issued.status.expiration_timestamp.timestamp()
                if not issued.status.token or expires <= now + 5:
                    raise ValueError("TokenRequest returned an empty or expired credential")
                self._token = issued.status.token
                self._expires = expires
                margin = min(1200, (expires - now) / 2)
                self._refresh = expires - margin * random.uniform(0.9, 1.0)
                self._retry = 0
                self._backoff = 1
                return self._token
            except Exception as exc:
                self._retry = self._now() + self._backoff
                self._backoff = min(30, self._backoff * 2)
                status = getattr(exc, "status", None)
                logger.warning(
                    "FastPath TokenRequest refresh failed: namespace=%s service_account=%s error_type=%s status=%s",
                    self._namespace,
                    self._service_account,
                    type(exc).__name__,
                    status,
                )
                if self._token and self._now() < self._expires - 5:
                    return self._token
                # Kubernetes error bodies can contain credentials; do not propagate them.
                raise RuntimeError(
                    "FastPath TokenRequest failed; check Kubernetes credentials and token-minting RBAC"
                ) from None


_CallDetails = namedtuple(
    "_CallDetails", "method timeout metadata credentials wait_for_ready compression"
)


class _Details(_CallDetails, grpc.ClientCallDetails):
    pass


class CredentialInterceptor(grpc.UnaryUnaryClientInterceptor):
    def __init__(self, source: TokenSource):
        self._source = source

    def intercept_unary_unary(self, continuation, details, request):
        from opensandbox_server.services.fast_sandbox.fastpath_client import FastPathUnavailable

        try:
            token = self._source.token()
        except Exception:
            raise FastPathUnavailable(
                "credentials-unavailable", "FastPath caller credential unavailable"
            ) from None
        metadata = [(k, v) for k, v in (details.metadata or ()) if k.lower() != "authorization"]
        metadata.append(("authorization", f"Bearer {token}"))
        return continuation(
            _Details(
                details.method,
                details.timeout,
                metadata,
                details.credentials,
                details.wait_for_ready,
                details.compression,
            ),
            request,
        )
