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
Connection configuration for OpenSandbox operations.
"""

import os
from datetime import timedelta

import httpx  # type: ignore[reportMissingImports]
from pydantic import (  # type: ignore[reportMissingImports]
    BaseModel,
    ConfigDict,
    Field,
    PrivateAttr,
    field_validator,
)


class ConnectionConfig(BaseModel):
    """
    Sandbox operations connection configuration.

    Transport lifecycle:
    - If `transport` is NOT provided, the SDK creates a default `httpx.AsyncHTTPTransport`
      per Sandbox/Manager instance. In this case, `Sandbox.close()` / `SandboxManager.close()`
      will close the transport.
    - If `transport` IS provided by the user, the SDK will NOT close it; the user owns it.

    Note:
    - Async transports are generally expected to be used within a single asyncio event loop.
      If your test runner creates multiple event loops (common in pytest-asyncio defaults),
      avoid sharing a single `ConnectionConfig(transport=...)` instance across loops.
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
    debug: bool = Field(
        default=False, description="Enable debug logging for HTTP requests"
    )
    user_agent: str = Field(
        default="OpenSandbox-Python-SDK/0.1.9", description="User agent string"
    )
    headers: dict[str, str] = Field(
        default_factory=dict, description="User defined headers"
    )
    transport: httpx.AsyncBaseTransport | None = Field(
        default=None,
        description=(
            "Shared httpx transport instance used by all HTTP clients within a "
            "Sandbox/Manager instance. Pass a custom transport (e.g. AsyncHTTPTransport "
            "with custom settings) to control connection pooling, proxies, retries, etc."
        ),
    )
    use_server_proxy: bool = Field(
        default=False,
        description=(
            "Using sandbox server as proxy for process execd requests"
            "It's useful when client sdk can't access the created sandbox directly"
        ),
    )

    # Environment variable names
    _ENV_API_KEY = "OPEN_SANDBOX_API_KEY"
    _ENV_DOMAIN = "OPEN_SANDBOX_DOMAIN"
    _DEFAULT_DOMAIN = "localhost:8080"
    _API_VERSION = "v1"

    def model_post_init(self, __context: object) -> None:
        # If the user explicitly provided `transport`, the SDK must not close it.
        self._owns_transport = "transport" not in self.model_fields_set

    def with_transport_if_missing(self) -> "ConnectionConfig":
        """
        Ensure a transport exists for this SDK resource.

        If `transport` is missing, return a copy with a default transport and
        mark it as SDK-owned. If present, return self unchanged.
        """
        if self.transport is not None:
            return self
        transport = httpx.AsyncHTTPTransport(
            limits=httpx.Limits(
                max_connections=100,
                max_keepalive_connections=20,
                keepalive_expiry=30.0,
            ),
        )
        config = self.model_copy(update={"transport": transport})
        config._owns_transport = True
        return config

    async def close_transport_if_owned(self) -> None:
        """Close the transport only if it was created by default_factory."""
        if self.transport is None or not self._owns_transport:
            return
        try:
            await self.transport.aclose()
        except Exception:
            # Avoid raising during cleanup paths
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
        """
        Get API key from config or environment variable.
        Returns:
            API key string (may be empty if not configured)
        Note: An empty API key may cause authentication failures.
        Consider checking if the key is set before making API calls.
        """
        return self.api_key or os.getenv(self._ENV_API_KEY, "")

    def get_domain(self) -> str:
        """Get domain from config or environment variable."""
        return self.domain or os.getenv(self._ENV_DOMAIN, self._DEFAULT_DOMAIN)

    def get_base_url(self) -> str:
        """Get the full base URL for API requests."""
        domain = self.get_domain()
        # Allow domain to override protocol if it explicitly starts with a scheme
        if domain.startswith("http://") or domain.startswith(
            "https://"
        ):
            return f"{domain}/{self._API_VERSION}"
        return f"{self.protocol}://{domain}/{self._API_VERSION}"
