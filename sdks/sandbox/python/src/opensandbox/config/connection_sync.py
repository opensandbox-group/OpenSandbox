#
# Copyright 2025 Alibaba Group Holding Ltd.
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
"""
Synchronous connection configuration for OpenSandbox SDK.

This mirrors ConnectionConfig (async) but uses httpx sync transports.
"""

import os
from datetime import timedelta

import httpx
from pydantic import BaseModel, ConfigDict, Field, PrivateAttr, field_validator


class ConnectionConfigSync(BaseModel):
    """
    Synchronous connection configuration shared across all sync SDK HTTP clients.

    Ownership rules:
    - If `transport` is not provided, the SDK creates a default HTTPTransport per
      Sandbox/Manager instance and will close it.
    - If `transport` is provided, the SDK will NOT close it (user owns it).
    """

    model_config = ConfigDict(arbitrary_types_allowed=True)

    _owns_transport: bool = PrivateAttr(default=True)

    api_key: str | None = Field(
        default=None, description="API key for authentication with sandbox service"
    )
    domain: str | None = Field(
        default=None, description="Base domain for the sandbox management API"
    )
    protocol: str = Field(default="http", description="Protocol to use (http/https)")
    request_timeout: timedelta = Field(
        default=timedelta(seconds=30),
        description="Timeout for HTTP requests to the management API",
    )
    debug: bool = Field(default=False, description="Enable debug logging for HTTP requests")
    user_agent: str = Field(
        default="OpenSandbox-Python-SDK/0.1.9", description="User agent string"
    )
    headers: dict[str, str] = Field(default_factory=dict, description="User defined headers")

    transport: httpx.BaseTransport | None = Field(
        default=None,
        description=(
            "Shared httpx transport instance used by all HTTP clients within a "
            "Sandbox/Manager instance. Pass a custom transport (e.g. HTTPTransport "
            "with custom limits/proxies) to control connection pooling, proxies, retries, etc."
        ),
    )
    use_server_proxy: bool = Field(
        default=False,
        description=(
            "Using sandbox server as proxy for process execd requests"
            "It's useful when client sdk can't access the created sandbox directly"
        ),
    )

    _ENV_API_KEY = "OPEN_SANDBOX_API_KEY"
    _ENV_DOMAIN = "OPEN_SANDBOX_DOMAIN"
    _DEFAULT_DOMAIN = "localhost:8080"
    _API_VERSION = "v1"

    def model_post_init(self, __context: object) -> None:
        self._owns_transport = "transport" not in self.model_fields_set

    def with_transport_if_missing(self) -> "ConnectionConfigSync":
        """
        Ensure a transport exists for this SDK resource.

        If `transport` is missing, return a copy with a default transport and
        mark it as SDK-owned. If present, return self unchanged.
        """
        if self.transport is not None:
            return self
        transport = httpx.HTTPTransport(
            limits=httpx.Limits(
                max_connections=100,
                max_keepalive_connections=20,
                keepalive_expiry=30.0,
            ),
        )
        config = self.model_copy(update={"transport": transport})
        config._owns_transport = True
        return config

    def close_transport_if_owned(self) -> None:
        """Close the transport only if it was created by default_factory."""
        if self.transport is None or not self._owns_transport:
            return
        try:
            self.transport.close()
        except Exception:
            pass

    @field_validator("protocol")
    @classmethod
    def protocol_must_be_valid(cls, v: str) -> str:
        v = v.lower()
        if v not in ("http", "https"):
            raise ValueError("Protocol must be 'http' or 'https'")
        return v

    @field_validator("request_timeout")
    @classmethod
    def timeout_must_be_positive(cls, v: timedelta) -> timedelta:
        if v.total_seconds() <= 0:
            raise ValueError(f"Request timeout must be positive, got: {v}")
        return v

    def get_api_key(self) -> str:
        return self.api_key or os.getenv(self._ENV_API_KEY, "")

    def get_domain(self) -> str:
        return self.domain or os.getenv(self._ENV_DOMAIN, self._DEFAULT_DOMAIN)

    def get_base_url(self) -> str:
        domain = self.get_domain()
        if domain.startswith("http://") or domain.startswith("https://"):
            return f"{domain}/{self._API_VERSION}"
        return f"{self.protocol}://{domain}/{self._API_VERSION}"
