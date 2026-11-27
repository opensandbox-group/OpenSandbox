# pyright: reportAttributeAccessIssue=false
# protobuf-generated modules expose dynamic attributes.

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

"""FastPath v2 gRPC client with sync and loop-scoped grpc.aio async paths; typed gRPC statuses classify errors."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
import inspect
from pathlib import Path
from threading import Lock
from typing import Optional
from weakref import WeakKeyDictionary

import grpc
import grpc.aio

from opensandbox_server.services.fast_sandbox.auth import (
    AsyncCredentialInterceptor,
    CredentialInterceptor,
    TokenSource,
)

from opensandbox_server.services.fast_sandbox.generated import (
    fastpath_pb2 as fastpath_pb2,
)
from opensandbox_server.services.fast_sandbox.generated import (
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


class FastPathResourceExhausted(FastPathError):
    """gRPC RESOURCE_EXHAUSTED may indicate runtime admission failure, not only pool capacity."""


class FastPathFailedPrecondition(FastPathError):
    """The referenced sandbox is not in a state that allows the operation."""


class FastPathHandlerError(FastPathError):
    """A relayed Action Handler answered with a non-2xx HTTP status.

    Per the FastPath v2 relay contract the handler's HTTP status and body are
    propagated as the RPC *result*, not as an RPC error: the embedded status
    (e.g. 409 exists / 412 revision or incarnation mismatch / 404 unknown
    subject / 400 validation NACK) is the authoritative failure surface.
    """

    def __init__(self, status_code: int, body: bytes):
        self.status_code = status_code
        self.body = body
        preview = body.decode("utf-8", errors="replace").strip()
        super().__init__(
            f"HTTP_{status_code}",
            preview or f"handler returned HTTP {status_code}",
        )


@dataclass
class _AioLoopState:
    connect_lock: asyncio.Lock = field(default_factory=asyncio.Lock)
    channel: Optional[grpc.aio.Channel] = None
    stub: Optional[fastpath_pb2_grpc.FastPathServiceStub] = None
    roots: Optional[bytes] = None
    ca_stat: Optional[tuple[Optional[str], int, int, int]] = None
    active_calls: dict[grpc.aio.Channel, int] = field(default_factory=dict)
    retired_channels: set[grpc.aio.Channel] = field(default_factory=set)


class FastPathClient:
    """Synchronous gRPC client for the fast-sandbox FastPathService v2 API."""

    def __init__(
        self,
        endpoint: str = DEFAULT_FASTPATH_ENDPOINT,
        timeout_seconds: float = 30.0,
        *,
        auth_enabled: Optional[bool] = None,
        token_source: Optional[TokenSource] = None,
        tls_ca_file: Optional[str] = None,
        tls_server_name: Optional[str] = None,
    ) -> None:
        if auth_enabled is None:
            auth_enabled = token_source is not None or tls_ca_file is not None or tls_server_name is not None
        self._auth_enabled = auth_enabled
        self._token_source = token_source
        self._tls_ca_file = tls_ca_file
        self._tls_server_name = tls_server_name
        self._roots: Optional[bytes] = None
        self._ca_stat: Optional[tuple[Optional[str], int, int, int]] = None
        self._endpoint = endpoint
        self._timeout_seconds = timeout_seconds
        self._channel: Optional[grpc.Channel] = None
        self._stub: Optional[fastpath_pb2_grpc.FastPathServiceStub] = None
        self._active_calls: dict[grpc.Channel, int] = {}
        self._retired_channels: set[grpc.Channel] = set()
        self._connect_lock = Lock()
        # Per-loop grpc.aio channel state so async FastAPI routes avoid the
        # default ThreadPoolExecutor bottleneck while staying safe across
        # concurrent or sequential event loops sharing the same client.
        self._aio_states_lock = Lock()
        self._aio_states: WeakKeyDictionary[asyncio.AbstractEventLoop, _AioLoopState] = (
            WeakKeyDictionary()
        )

    def __enter__(self) -> "FastPathClient":
        self.connect()
        return self

    def __exit__(self, *exc_info) -> None:
        self.close()

    def _load_tls_roots_for(
        self,
        *,
        has_channel: bool,
        cached_stat: Optional[tuple[Optional[str], int, int, int]],
        cached_roots: Optional[bytes],
    ) -> tuple[Optional[bytes], Optional[tuple[Optional[str], int, int, int]]]:
        if not self._tls_ca_file:
            return None, None
        path = Path(self._tls_ca_file)
        st = path.stat()
        stat_key = (self._tls_ca_file, st.st_mtime_ns, st.st_size, st.st_ino)
        if has_channel and stat_key == cached_stat:
            return cached_roots, stat_key
        roots = path.read_bytes()
        if b"-----BEGIN CERTIFICATE-----" not in roots:
            raise ValueError("invalid CA bundle")
        return roots, stat_key

    def _load_tls_roots(self) -> tuple[Optional[bytes], Optional[tuple[Optional[str], int, int, int]]]:
        return self._load_tls_roots_for(
            has_channel=self._channel is not None,
            cached_stat=self._ca_stat,
            cached_roots=self._roots,
        )

    def connect(self) -> None:
        if not self._auth_enabled:
            if self._stub is not None and self._channel is not None:
                return
            with self._connect_lock:
                if self._channel is None:
                    channel = grpc.insecure_channel(self._endpoint)
                    self._stub = fastpath_pb2_grpc.FastPathServiceStub(channel)
                    self._channel = channel
            return
        if self._token_source is None:
            raise FastPathUnavailable("credentials-missing", "FastPath caller TokenRequest source is required")
        try:
            roots, stat_key = self._load_tls_roots()
        except (OSError, ValueError):
            raise FastPathUnavailable("tls-trust-unavailable", "FastPath TLS CA bundle cannot be loaded") from None
        if self._stub is not None and self._channel is not None and roots == self._roots:
            self._ca_stat = stat_key
            return
        with self._connect_lock:
            try:
                roots, stat_key = self._load_tls_roots()
            except (OSError, ValueError):
                raise FastPathUnavailable("tls-trust-unavailable", "FastPath TLS CA bundle cannot be loaded") from None
            if self._channel is None or roots != self._roots:
                try:
                    self._token_source.token()
                except Exception:
                    raise FastPathUnavailable(
                        "credentials-unavailable",
                        "FastPath TokenRequest preflight failed; check Kubernetes credentials, API TLS and caller RBAC",
                    ) from None
                options = [("grpc.ssl_target_name_override", self._tls_server_name)] if self._tls_server_name else []
                channel = grpc.intercept_channel(
                    grpc.secure_channel(self._endpoint, grpc.ssl_channel_credentials(roots), options=options),
                    CredentialInterceptor(self._token_source),
                )
                old_channel = self._channel
                self._stub = fastpath_pb2_grpc.FastPathServiceStub(channel)
                self._channel = channel
                self._roots = roots
                self._ca_stat = stat_key
                if old_channel is not None and old_channel is not channel:
                    # Retire connections authenticated by removed trust roots once in-flight RPCs drain.
                    if self._active_calls.get(old_channel, 0) > 0:
                        self._retired_channels.add(old_channel)
                    else:
                        self._active_calls.pop(old_channel, None)
                        old_channel.close()
            else:
                self._ca_stat = stat_key

    def close(self) -> None:
        with self._connect_lock:
            to_close = list(self._retired_channels)
            self._retired_channels.clear()
            self._active_calls.clear()
            if self._channel is not None:
                to_close.append(self._channel)
                self._channel = None
                self._stub = None
                self._roots = None
                self._ca_stat = None
        for channel in to_close:
            channel.close()

    def _get_aio_state(self, *, create: bool = True) -> Optional[_AioLoopState]:
        loop = asyncio.get_running_loop()
        with self._aio_states_lock:
            state = self._aio_states.get(loop)
            if state is None and create:
                state = _AioLoopState()
                self._aio_states[loop] = state
            return state

    async def _aload_tls_roots_for(
        self,
        *,
        has_channel: bool,
        cached_stat: Optional[tuple[Optional[str], int, int, int]],
        cached_roots: Optional[bytes],
    ) -> tuple[Optional[bytes], Optional[tuple[Optional[str], int, int, int]]]:
        if not self._tls_ca_file:
            return None, None
        path = Path(self._tls_ca_file)
        st = path.stat()
        stat_key = (self._tls_ca_file, st.st_mtime_ns, st.st_size, st.st_ino)
        if has_channel and stat_key == cached_stat:
            return cached_roots, stat_key
        roots = await asyncio.to_thread(path.read_bytes)
        if b"-----BEGIN CERTIFICATE-----" not in roots:
            raise ValueError("invalid CA bundle")
        return roots, stat_key

    async def aconnect(self) -> None:
        state = self._get_aio_state()
        assert state is not None
        if not self._auth_enabled:
            if state.stub is not None and state.channel is not None:
                return
            async with state.connect_lock:
                if state.channel is None:
                    channel = grpc.aio.insecure_channel(self._endpoint)
                    state.stub = fastpath_pb2_grpc.FastPathServiceStub(channel)
                    state.channel = channel
            return
        if self._token_source is None:
            raise FastPathUnavailable("credentials-missing", "FastPath caller TokenRequest source is required")
        if state.stub is not None and state.channel is not None:
            if not self._tls_ca_file:
                if state.ca_stat is None and state.roots is None:
                    return
            else:
                try:
                    st = Path(self._tls_ca_file).stat()
                except OSError:
                    raise FastPathUnavailable(
                        "tls-trust-unavailable", "FastPath TLS CA bundle cannot be loaded"
                    ) from None
                stat_key = (self._tls_ca_file, st.st_mtime_ns, st.st_size, st.st_ino)
                if stat_key == state.ca_stat:
                    return
        channel_to_close: Optional[grpc.aio.Channel] = None
        async with state.connect_lock:
            try:
                roots, stat_key = await self._aload_tls_roots_for(
                    has_channel=state.channel is not None,
                    cached_stat=state.ca_stat,
                    cached_roots=state.roots,
                )
            except (OSError, ValueError):
                raise FastPathUnavailable("tls-trust-unavailable", "FastPath TLS CA bundle cannot be loaded") from None
            if state.channel is None or roots != state.roots:
                try:
                    atoken = getattr(self._token_source, "atoken", None)
                    if inspect.iscoroutinefunction(atoken):
                        await atoken()
                    else:
                        await asyncio.to_thread(self._token_source.token)
                except Exception:
                    raise FastPathUnavailable(
                        "credentials-unavailable",
                        "FastPath TokenRequest preflight failed; check Kubernetes credentials, API TLS and caller RBAC",
                    ) from None
                options = [("grpc.ssl_target_name_override", self._tls_server_name)] if self._tls_server_name else []
                channel = grpc.aio.secure_channel(
                    self._endpoint,
                    grpc.ssl_channel_credentials(roots),
                    options=options,
                    interceptors=[AsyncCredentialInterceptor(self._token_source)],
                )
                old_channel = state.channel
                state.stub = fastpath_pb2_grpc.FastPathServiceStub(channel)
                state.channel = channel
                state.roots = roots
                state.ca_stat = stat_key
                if old_channel is not None and old_channel is not channel:
                    if state.active_calls.get(old_channel, 0) > 0:
                        state.retired_channels.add(old_channel)
                    else:
                        state.active_calls.pop(old_channel, None)
                        channel_to_close = old_channel
            else:
                state.ca_stat = stat_key
        if channel_to_close is not None:
            await channel_to_close.close()

    async def aclose(self) -> None:
        state = self._get_aio_state(create=False)
        if state is None:
            return
        async with state.connect_lock:
            to_close = list(state.retired_channels)
            state.retired_channels.clear()
            state.active_calls.clear()
            if state.channel is not None:
                to_close.append(state.channel)
                state.channel = None
                state.stub = None
                state.roots = None
                state.ca_stat = None
        for channel in to_close:
            await channel.close()

    # -- lifecycle ---------------------------------------------------------

    async def create_sandbox_async(
        self,
        request: fastpath_pb2.CreateSandboxRequest,
        *,
        wait_timeout_millis: Optional[int] = None,
    ) -> fastpath_pb2.CreateSandboxResponse:
        """Async CreateSandbox over grpc.aio (avoids ThreadPoolExecutor saturation)."""
        deadline = (
            self._rpc_timeout(wait_timeout_millis)
            if wait_timeout_millis is not None
            else self._timeout_seconds
        )
        return await self._acall(
            lambda stub: stub.CreateSandbox(request, timeout=deadline)
        )

    async def create_rootfs_sandbox_async(
        self,
        request: fastpath_pb2.CreateRootfsSandboxRequest,
        *,
        wait_timeout_millis: int,
    ) -> fastpath_pb2.CreateSandboxResponse:
        return await self._acall(
            lambda stub: stub.CreateRootfsSandbox(
                request, timeout=self._rpc_timeout(wait_timeout_millis)
            )
        )

    async def get_sandbox_async(
        self,
        namespace: str,
        sandbox_name: str,
        *,
        expected_uid: str = "",
        expected_generation: int = 0,
    ) -> fastpath_pb2.GetSandboxResponse:
        """Async GetSandbox over grpc.aio; raises FastPathNotFound on gRPC NotFound."""
        reference = namespaced_reference(namespace, sandbox_name, expected_uid=expected_uid)
        request = fastpath_pb2.GetSandboxRequest(
            sandbox=reference,
            expected_generation=expected_generation,
        )
        return await self._acall(
            lambda stub: stub.GetSandbox(request, timeout=self._timeout_seconds)
        )

    async def get_pool_async(self, namespace: str, pool_name: str) -> fastpath_pb2.PoolInfo:
        """Async GetPool over grpc.aio; raises FastPathNotFound when absent."""
        request = fastpath_pb2.GetPoolRequest(namespace=namespace, pool_name=pool_name)
        return await self._acall(
            lambda stub: stub.GetPool(request, timeout=self._timeout_seconds)
        )

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

    def create_rootfs_sandbox(
        self,
        request: fastpath_pb2.CreateRootfsSandboxRequest,
        *,
        wait_timeout_millis: int,
    ) -> fastpath_pb2.CreateSandboxResponse:
        return self._call(lambda: self._require_stub().CreateRootfsSandbox(
            request, timeout=self._rpc_timeout(wait_timeout_millis),
        ))

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

    def pause_sandbox(
        self,
        namespace: str,
        sandbox_name: str,
        *,
        expected_uid: str = "",
        expected_generation: int = 0,
        request_id: str = "",
    ) -> fastpath_pb2.PauseSandboxResponse:
        """Persist the pause intent; completion (PAUSED) is observed via get_sandbox.

        ``expected_generation`` fences against concurrent state changes
        (0 = no fence); rejection surfaces as FailedPrecondition.
        """
        request = fastpath_pb2.PauseSandboxRequest(
            sandbox=namespaced_reference(namespace, sandbox_name, expected_uid=expected_uid),
            expected_generation=expected_generation,
            request_id=request_id,
        )
        return self._call(
            lambda: self._require_stub().PauseSandbox(request, timeout=self._timeout_seconds)
        )

    def resume_sandbox(
        self,
        namespace: str,
        sandbox_name: str,
        *,
        expected_uid: str = "",
        expected_checkpoint_id: str = "",
        request_id: str = "",
    ) -> fastpath_pb2.ResumeSandboxResponse:
        """Persist the resume intent; completion (READY) is observed via get_sandbox."""
        request = fastpath_pb2.ResumeSandboxRequest(
            sandbox=namespaced_reference(namespace, sandbox_name, expected_uid=expected_uid),
            expected_checkpoint_id=expected_checkpoint_id,
            request_id=request_id,
        )
        return self._call(
            lambda: self._require_stub().ResumeSandbox(request, timeout=self._timeout_seconds)
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

    def replace_action_bindings(
        self,
        namespace: str,
        sandbox_name: str,
        bindings: list[dict],
        *,
        expected_uid: str,
        expected_generation: int,
    ) -> fastpath_pb2.UpdateSandboxResponse:
        request = fastpath_pb2.UpdateSandboxRequest(
            sandbox=namespaced_reference(namespace, sandbox_name, expected_uid=expected_uid),
            expected_generation=expected_generation,
            action_bindings=fastpath_pb2.ReplaceActionBindings(
                items=[
                    fastpath_pb2.ActionBinding(handler=b["handler"], input=b["input"])
                    for b in bindings
                ]
            ),
        )
        return self._call(
            lambda: self._require_stub().UpdateSandbox(request, timeout=self._timeout_seconds)
        )

    # -- snapshots -----------------------------------------------------------

    def create_sandbox_snapshot(
        self,
        request: fastpath_pb2.CreateSandboxSnapshotRequest,
    ) -> fastpath_pb2.CreateSandboxSnapshotResponse:
        """Submit a snapshot of a running sandbox; completion is observed via
        get_sandbox_snapshot (the intent is durable and idempotent by request_id)."""
        return self._call(
            lambda: self._require_stub().CreateSandboxSnapshot(
                request, timeout=self._timeout_seconds
            )
        )

    def get_sandbox_snapshot(
        self,
        namespace: str,
        snapshot_name: str,
        *,
        expected_uid: str = "",
    ) -> fastpath_pb2.GetSandboxSnapshotResponse:
        """Get a sandbox snapshot; raises FastPathNotFound on gRPC NotFound."""
        request = fastpath_pb2.GetSandboxSnapshotRequest(
            snapshot=fastpath_pb2.NamespacedName(namespace=namespace, name=snapshot_name),
            expected_uid=expected_uid,
        )
        return self._call(
            lambda: self._require_stub().GetSandboxSnapshot(
                request, timeout=self._timeout_seconds
            )
        )

    def delete_sandbox_snapshot(
        self,
        namespace: str,
        snapshot_name: str,
        *,
        expected_uid: str = "",
    ) -> None:
        """Delete a sandbox snapshot and its published artifacts."""
        request = fastpath_pb2.DeleteSandboxSnapshotRequest(
            snapshot=namespaced_reference(namespace, snapshot_name, expected_uid=expected_uid)
        )
        self._call(
            lambda: self._require_stub().DeleteSandboxSnapshot(
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

    # -- handler relay -----------------------------------------------------

    def invoke_handler_action(
        self,
        namespace: str,
        sandbox_name: str,
        *,
        expected_uid: str,
        handler: str,
        path: str,
        method: str = "POST",
        payload: Optional[bytes] = None,
    ) -> fastpath_pb2.HandlerActionResult:
        """Relay an opaque payload to a Pool-declared Action Handler and
        return its HTTP status/body verbatim on 2xx.

        A 4xx/5xx handler outcome is the RPC result (not an RPC error), so it
        raises FastPathHandlerError carrying the embedded status and body;
        relay failures (unknown sandbox, undeclared handler, unreachable
        Fastlet) raise the typed FastPathError subclasses as usual.
        """
        request = fastpath_pb2.InvokeHandlerActionRequest(
            sandbox=namespaced_reference(namespace, sandbox_name, expected_uid=expected_uid),
            handler=handler,
            method=method,
            path=path,
            payload=payload or b"",
        )
        response = self._call(
            lambda: self._require_stub().InvokeHandlerAction(
                request, timeout=self._timeout_seconds
            )
        )
        result = response.result
        status_code = int(result.status) if result is not None else 0
        if not 200 <= status_code <= 299:
            body = bytes(result.body) if result is not None else b""
            raise FastPathHandlerError(status_code, body)
        return result

    # -- pools -------------------------------------------------------------

    def get_pool(self, namespace: str, pool_name: str) -> fastpath_pb2.PoolInfo:
        """Get a SandboxPool; raises FastPathNotFound when absent."""
        request = fastpath_pb2.GetPoolRequest(namespace=namespace, pool_name=pool_name)
        return self._call(
            lambda: self._require_stub().GetPool(request, timeout=self._timeout_seconds)
        )

    def list_pools(self, namespace: str) -> fastpath_pb2.ListPoolsResponse:
        request = fastpath_pb2.ListPoolsRequest(namespace=namespace)
        return self._call(
            lambda: self._require_stub().ListPools(request, timeout=self._timeout_seconds)
        )

    # -- internals ---------------------------------------------------------

    def _rpc_timeout(self, server_wait_millis: int) -> float:
        """Keep the client deadline beyond FastPath's server-side readiness wait."""
        return max(self._timeout_seconds, server_wait_millis / 1000 + 5.0)

    def _require_stub(self) -> fastpath_pb2_grpc.FastPathServiceStub:
        self.connect()
        if self._stub is None:  # pragma: no cover - connect() always sets it
            raise FastPathUnavailable("channel-not-open", "FastPath client is not connected")
        return self._stub

    def _call(self, call):
        self.connect()
        with self._connect_lock:
            channel = self._channel
            if channel is not None:
                self._active_calls[channel] = self._active_calls.get(channel, 0) + 1
        try:
            return call()
        except grpc.RpcError as exc:
            raise _to_fastpath_error(exc) from exc
        finally:
            if channel is not None:
                should_close = False
                with self._connect_lock:
                    remaining = self._active_calls.get(channel, 1) - 1
                    if remaining <= 0:
                        self._active_calls.pop(channel, None)
                        if channel in self._retired_channels:
                            self._retired_channels.discard(channel)
                            should_close = True
                    else:
                        self._active_calls[channel] = remaining
                if should_close:
                    channel.close()

    async def _acall(self, call):
        await self.aconnect()
        state = self._get_aio_state(create=False)
        channel = state.channel if state is not None else None
        stub = state.stub if state is not None else None
        if state is None or stub is None:  # pragma: no cover - aconnect() always sets it
            raise FastPathUnavailable("channel-not-open", "FastPath async client is not connected")
        if channel is not None:
            state.active_calls[channel] = state.active_calls.get(channel, 0) + 1
        try:
            return await call(stub)
        except grpc.RpcError as exc:
            raise _to_fastpath_error(exc) from exc
        finally:
            if channel is not None:
                should_close = False
                remaining = state.active_calls.get(channel, 1) - 1
                if remaining <= 0:
                    state.active_calls.pop(channel, None)
                    if channel in state.retired_channels:
                        state.retired_channels.discard(channel)
                        should_close = True
                else:
                    state.active_calls[channel] = remaining
                if should_close:
                    await channel.close()


def _to_fastpath_error(exc: grpc.RpcError) -> FastPathError:
    """Normalize a gRPC status to a typed FastPathError, without string matching."""
    code = exc.code()
    details = exc.details() or ""
    if code == grpc.StatusCode.NOT_FOUND:
        return FastPathNotFound(code.name, details)
    if code == grpc.StatusCode.INVALID_ARGUMENT:
        return FastPathInvalidArgument(code.name, details)
    if code == grpc.StatusCode.RESOURCE_EXHAUSTED:
        return FastPathResourceExhausted(code.name, details)
    if code == grpc.StatusCode.FAILED_PRECONDITION:
        return FastPathFailedPrecondition(code.name, details)
    if code in (grpc.StatusCode.ALREADY_EXISTS, grpc.StatusCode.ABORTED):
        return FastPathConflict(code.name, details)
    # Authentication/authorization rejections belong to the platform deployment
    # identity (not the end-user API caller) and are surfaced as backend dependency
    # unavailability until token refresh or policy rollout converges.
    if code in (
        grpc.StatusCode.UNAVAILABLE,
        grpc.StatusCode.DEADLINE_EXCEEDED,
        grpc.StatusCode.CANCELLED,
        grpc.StatusCode.UNAUTHENTICATED,
        grpc.StatusCode.PERMISSION_DENIED,
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
