# pyright: reportAttributeAccessIssue=false
# protobuf-generated modules expose dynamic attributes.

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

"""FastPath gRPC client for the fast-sandbox Fast-Path Server (FastPath v2).

Wraps the generated `fastpath.v2.FastPathService` stubs with typed helpers and
normalized error handling. OpenSandbox must not infer NotFound from error
strings: only gRPC `codes.NotFound` maps to the public HTTP 404 contract.

The client is deliberately **synchronous**: the lifecycle service methods are
called from FastAPI thread-pool handlers and background renew workers
(`asyncio.to_thread`), where an aio channel bound to another event loop would
be unsafe. This matches the sync-call pattern of the Docker and Kubernetes
backends.

"""

from __future__ import annotations

from threading import Lock
from typing import Optional

import grpc

from opensandbox_server.services.fleets.generated import (
    fastpath_pb2 as fastpath_pb2,
)
from opensandbox_server.services.fleets.generated import (
    fastpath_pb2_grpc as fastpath_pb2_grpc,
)

DEFAULT_FASTPATH_ENDPOINT = "fast-sandbox-fastpath.opensandbox.svc:9090"


class FastPathError(Exception):
    """Base error for fast-sandbox FastPath communication failures."""

    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code
        self.message = message

    def __str__(self) -> str:  # pragma: no cover - trivial formatting
        return f"FastPathError(code={self.code}, message={self.message})"


class FastPathNotFound(FastPathError):
    """The referenced fast-sandbox resource does not exist (gRPC NotFound)."""


class FastPathUnavailable(FastPathError):
    """The FastPath server is unreachable or the request timed out."""


class FastPathInvalidArgument(FastPathError):
    """The FastPath server rejected the request (gRPC InvalidArgument)."""


class FastPathConflict(FastPathError):
    """The FastPath request conflicts with existing durable state."""


class FastPathClient:
    """Synchronous gRPC client for the fast-sandbox FastPathService v2 API."""

    def __init__(
        self,
        endpoint: str = DEFAULT_FASTPATH_ENDPOINT,
        timeout_seconds: float = 30.0,
    ) -> None:
        self._endpoint = endpoint
        self._timeout_seconds = timeout_seconds
        self._channel: Optional[grpc.Channel] = None
        self._stub: Optional[fastpath_pb2_grpc.FastPathServiceStub] = None
        self._connect_lock = Lock()

    def __enter__(self) -> "FastPathClient":
        self.connect()
        return self

    def __exit__(self, *exc_info) -> None:
        self.close()

    def connect(self) -> None:
        """Open the gRPC channel to the FastPath endpoint."""
        if self._channel is not None:
            return
        with self._connect_lock:
            if self._channel is None:
                channel = grpc.insecure_channel(self._endpoint)
                self._stub = fastpath_pb2_grpc.FastPathServiceStub(channel)
                self._channel = channel

    def close(self) -> None:
        """Close the gRPC channel if open."""
        with self._connect_lock:
            if self._channel is not None:
                self._channel.close()
                self._channel = None
                self._stub = None

    # -- lifecycle ---------------------------------------------------------

    def create_sandbox(
        self,
        request: fastpath_pb2.CreateSandboxRequest,
        *,
        wait_timeout_millis: Optional[int] = None,
    ) -> fastpath_pb2.CreateSandboxResponse:
        """Create a sandbox through FastPath v2 (CRD-first, idempotent by request_id)."""
        deadline = (
            self._rpc_timeout(wait_timeout_millis)
            if wait_timeout_millis is not None
            else self._timeout_seconds
        )
        return self._call(lambda: self._require_stub().CreateSandbox(request, timeout=deadline))

    def get_sandbox(
        self,
        namespace: str,
        sandbox_name: str,
        *,
        expected_uid: str = "",
        expected_generation: int = 0,
    ) -> fastpath_pb2.GetSandboxResponse:
        """Get a sandbox; raises FastPathNotFound on gRPC NotFound."""
        reference = namespaced_reference(namespace, sandbox_name, expected_uid=expected_uid)
        request = fastpath_pb2.GetSandboxRequest(
            sandbox=reference,
            expected_generation=expected_generation,
        )
        return self._call(
            lambda: self._require_stub().GetSandbox(request, timeout=self._timeout_seconds)
        )

    def delete_sandbox(self, namespace: str, sandbox_name: str, *, expected_uid: str = "") -> None:
        """Submit an async (finalizer-driven) sandbox deletion."""
        request = fastpath_pb2.DeleteRequest(
            sandbox=namespaced_reference(namespace, sandbox_name, expected_uid=expected_uid)
        )
        self._call(
            lambda: self._require_stub().DeleteSandbox(request, timeout=self._timeout_seconds)
        )

    def list_sandboxes(
        self,
        namespace: str,
        metadata: Optional[dict] = None,
        page_size: Optional[int] = None,
        page_token: Optional[str] = None,
    ) -> fastpath_pb2.ListSandboxesResponse:
        """List sandboxes in a namespace; metadata acts as an AND-filter."""
        request = fastpath_pb2.ListSandboxesRequest(namespace=namespace)
        if metadata:
            request.metadata.update(metadata)
        if page_size is not None:
            request.page_size = page_size
        if page_token:
            request.page_token = page_token
        return self._call(
            lambda: self._require_stub().ListSandboxes(request, timeout=self._timeout_seconds)
        )

    def update_expiration(
        self,
        namespace: str,
        sandbox_name: str,
        expires_at_unix_seconds: int,
        *,
        expected_uid: str = "",
        expected_generation: int = 0,
    ) -> fastpath_pb2.UpdateSandboxResponse:
        """Persist an absolute expiry on the Sandbox CRD."""
        request = fastpath_pb2.UpdateSandboxRequest(
            sandbox=namespaced_reference(namespace, sandbox_name, expected_uid=expected_uid),
            expected_generation=expected_generation,
        )
        request.expires_at_unix_seconds = expires_at_unix_seconds
        return self._call(
            lambda: self._require_stub().UpdateSandbox(request, timeout=self._timeout_seconds)
        )

    def update_metadata(
        self,
        namespace: str,
        sandbox_name: str,
        upsert: Optional[dict] = None,
        delete_keys: Optional[list[str]] = None,
        *,
        expected_uid: str = "",
        expected_generation: int = 0,
    ) -> fastpath_pb2.UpdateSandboxResponse:
        """Update metadata: upsert entries and delete keys in one call."""
        request = fastpath_pb2.UpdateSandboxRequest(
            sandbox=namespaced_reference(namespace, sandbox_name, expected_uid=expected_uid),
            expected_generation=expected_generation,
        )
        if upsert:
            request.metadata_upsert.update(upsert)
        if delete_keys:
            request.metadata_delete_keys.extend(delete_keys)
        return self._call(
            lambda: self._require_stub().UpdateSandbox(request, timeout=self._timeout_seconds)
        )

    def get_sandbox_diagnostics(
        self, namespace: str, sandbox_name: str, limit: int = 50
    ) -> fastpath_pb2.SandboxDiagnosticsResponse:
        """Return lifecycle diagnostics (events only, not process output)."""
        request = fastpath_pb2.SandboxDiagnosticsRequest(
            namespace=namespace, sandbox_name=sandbox_name, limit=limit
        )
        return self._call(
            lambda: self._require_stub().GetSandboxDiagnostics(
                request, timeout=self._timeout_seconds
            )
        )

    # -- readiness / endpoints --------------------------------------------

    def resolve_endpoint(
        self,
        reference: fastpath_pb2.SandboxReference,
        target: fastpath_pb2.EndpointTarget,
        *,
        access_mode: fastpath_pb2.EndpointAccessMode = (fastpath_pb2.CENTRAL_PROXY),
        expected_generation: int = 0,
    ) -> fastpath_pb2.ResolveEndpointResponse:
        """Resolve an authenticated proxy route for a component or raw port."""
        request = fastpath_pb2.ResolveEndpointRequest(
            sandbox=reference,
            target=target,
            access_mode=access_mode,
            expected_generation=expected_generation,
        )
        return self._call(
            lambda: self._require_stub().ResolveEndpoint(request, timeout=self._timeout_seconds)
        )

    # -- pools -------------------------------------------------------------

    def get_pool(self, namespace: str, pool_name: str) -> fastpath_pb2.PoolInfo:
        """Get a SandboxPool; raises FastPathNotFound when absent."""
        request = fastpath_pb2.GetPoolRequest(namespace=namespace, pool_name=pool_name)
        return self._call(
            lambda: self._require_stub().GetPool(request, timeout=self._timeout_seconds)
        )

    def list_pools(self, namespace: str) -> fastpath_pb2.ListPoolsResponse:
        """List SandboxPools in a namespace."""
        request = fastpath_pb2.ListPoolsRequest(namespace=namespace)
        return self._call(
            lambda: self._require_stub().ListPools(request, timeout=self._timeout_seconds)
        )

    # -- internals ---------------------------------------------------------

    def _rpc_timeout(self, server_wait_millis: int) -> float:
        """Keep the client deadline beyond FastPath's server-side readiness wait."""
        return max(self._timeout_seconds, server_wait_millis / 1000 + 5.0)

    def _require_stub(self) -> fastpath_pb2_grpc.FastPathServiceStub:
        if self._stub is None:
            self.connect()
        if self._stub is None:  # pragma: no cover - connect() always sets it
            raise FastPathUnavailable("channel-not-open", "FastPath client is not connected")
        return self._stub

    def _call(self, call):
        try:
            return call()
        except grpc.RpcError as exc:
            raise _to_fastpath_error(exc) from exc


def _to_fastpath_error(exc: grpc.RpcError) -> FastPathError:
    """Normalize a gRPC status to a typed FastPathError, without string matching."""
    code = exc.code()
    details = exc.details() or ""
    if code == grpc.StatusCode.NOT_FOUND:
        return FastPathNotFound(code.name, details)
    if code == grpc.StatusCode.INVALID_ARGUMENT:
        return FastPathInvalidArgument(code.name, details)
    if code in (grpc.StatusCode.ALREADY_EXISTS, grpc.StatusCode.ABORTED):
        return FastPathConflict(code.name, details)
    if code in (
        grpc.StatusCode.UNAVAILABLE,
        grpc.StatusCode.DEADLINE_EXCEEDED,
        grpc.StatusCode.CANCELLED,
    ):
        return FastPathUnavailable(code.name, details)
    return FastPathError(code.name, details)


def namespaced_reference(
    namespace: str, sandbox_name: str, *, expected_uid: str = ""
) -> fastpath_pb2.SandboxReference:
    """Build a SandboxReference by namespaced name (no UID cache required)."""
    return fastpath_pb2.SandboxReference(
        namespaced_name=fastpath_pb2.NamespacedName(namespace=namespace, name=sandbox_name),
        expected_uid=expected_uid,
    )


def component_target(component_name: str) -> fastpath_pb2.EndpointTarget:
    """Build an EndpointTarget for a named Pool Infra Component (e.g. execd)."""
    return fastpath_pb2.EndpointTarget(component_name=component_name)


def port_target(port: int) -> fastpath_pb2.EndpointTarget:
    """Build an EndpointTarget for a raw user port."""
    return fastpath_pb2.EndpointTarget(port=port)
