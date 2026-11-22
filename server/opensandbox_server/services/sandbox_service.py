# Copyright 2025 The OpenSandbox Authors
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

from abc import ABC, abstractmethod
import socket
from typing import Optional
from uuid import uuid4

from fastapi import HTTPException

from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    CreateSandboxResponse,
    Endpoint,
    ListSandboxesRequest,
    ListSandboxesResponse,
    PatchSandboxMetadataRequest,
    RenewSandboxExpirationRequest,
    RenewSandboxExpirationResponse,
    Sandbox,
)
from opensandbox_server.services.diagnostics import DiagnosticContext, DiagnosticResult
from opensandbox_server.services.validators import (
    RESERVED_METADATA_LABEL_PREFIXES,
    ensure_valid_port,
)


class SandboxService(ABC):

    def set_tenant_provider(self, provider: object) -> None:
        """Inject tenant provider (no-op for non-K8s implementations)."""

    def close(self) -> None:
        """Release service-owned background readers and connections."""

    @staticmethod
    def generate_sandbox_id() -> str:
        """Generate a unique RFC4122 UUID4 sandbox identifier (with hyphens)."""
        return str(uuid4())

    @staticmethod
    def _resolve_bind_ip(family: int = socket.AF_INET) -> str:
        """Resolve an outward-facing address; discovery failure falls back to loopback."""
        try:
            target = ("2001:4860:4860::8888", 80, 0, 0) if family == socket.AF_INET6 else ("8.8.8.8", 80)
            with socket.socket(family, socket.SOCK_DGRAM) as sock:
                sock.connect(target)
                ip = sock.getsockname()[0]
                if ip:
                    if family == socket.AF_INET or not ip.startswith("fe80"):
                        return ip
        except OSError:
            if family == socket.AF_INET6:
                return SandboxService._resolve_bind_ip(socket.AF_INET)

        try:
            family_name = socket.AF_INET6 if family == socket.AF_INET6 else socket.AF_INET
            hostname = socket.gethostname()
            infos = socket.getaddrinfo(hostname, None, family_name, socket.SOCK_DGRAM)
            if infos:
                addr = infos[0][4][0]
                if addr:
                    return addr
        except OSError:
            pass

        return "::1" if family == socket.AF_INET6 else "127.0.0.1"

    @staticmethod
    def validate_port(port: int) -> None:
        """Validate that the port is within 1-65535; raises HTTPException(400) otherwise."""
        ensure_valid_port(port)

    @abstractmethod
    async def create_sandbox(self, request: CreateSandboxRequest) -> CreateSandboxResponse:
        """Create from a validated request; HTTPException reports backend failure."""
        pass

    @abstractmethod
    def list_sandboxes(self, request: ListSandboxesRequest) -> ListSandboxesResponse:
        pass

    @abstractmethod
    def get_sandbox(self, sandbox_id: str) -> Sandbox:
        """Raise HTTPException when the sandbox is not found."""
        pass

    @abstractmethod
    def delete_sandbox(self, sandbox_id: str) -> None:
        """Request deletion (HTTPException on failure); return does not universally confirm runtime termination."""
        pass

    @abstractmethod
    def pause_sandbox(self, sandbox_id: str) -> None:
        """Raise HTTPException when the sandbox is missing or cannot be paused."""
        pass

    @abstractmethod
    def resume_sandbox(self, sandbox_id: str) -> None:
        """Raise HTTPException when the sandbox is missing or cannot be resumed."""
        pass

    @abstractmethod
    def renew_expiration(
        self,
        sandbox_id: str,
        request: RenewSandboxExpirationRequest,
    ) -> RenewSandboxExpirationResponse:
        """Set the requested absolute expiration; HTTPException reports a missing sandbox or renewal failure."""
        pass

    @abstractmethod
    def patch_sandbox_metadata(self, sandbox_id: str, patch: PatchSandboxMetadataRequest) -> Sandbox:
        """Patch sandbox metadata via JSON Merge Patch (RFC 7396). Non-null adds/replaces, null deletes, absent keeps."""
        pass

    @staticmethod
    def _is_system_label(key: str) -> bool:
        return key.startswith(RESERVED_METADATA_LABEL_PREFIXES)

    @staticmethod
    def _validate_metadata_patch(patch: dict) -> None:
        """Reject a metadata patch that touches reserved keys or adds invalid labels.

        Only the incoming values are validated, not the labels already present.
        """
        from fastapi import HTTPException
        from opensandbox_server.services.constants import SandboxErrorCodes
        from opensandbox_server.services.validators import ensure_metadata_labels

        for key in patch:
            if SandboxService._is_system_label(key):
                raise HTTPException(
                    status_code=400,
                    detail={
                        "code": SandboxErrorCodes.INVALID_METADATA_LABEL,
                        "message": f"Metadata key '{key}' is reserved (system-managed label prefix).",
                    },
                )

        patch_additions = {k: str(v) for k, v in patch.items() if v is not None}
        if patch_additions:
            ensure_metadata_labels(patch_additions)

    @staticmethod
    def _apply_metadata_patch(labels: dict, patch: dict) -> dict:
        """Apply JSON Merge Patch to labels: validate, separate user metadata, merge, rebuild."""
        SandboxService._validate_metadata_patch(patch)

        current_metadata = {
            k: v for k, v in labels.items() if not SandboxService._is_system_label(k)
        }

        for key, value in patch.items():
            if value is None:
                current_metadata.pop(key, None)
            else:
                current_metadata[key] = str(value)

        new_labels = {k: v for k, v in labels.items() if SandboxService._is_system_label(k)}
        for k, v in current_metadata.items():
            new_labels[k] = str(v)
        return new_labels

    def get_sandbox_diagnostic_context(self, sandbox_id: str) -> DiagnosticContext:
        """Return backend context; extensions may override this capability."""
        raise HTTPException(
            status_code=501,
            detail={
                "code": "DIAGNOSTICS_NOT_IMPLEMENTED",
                "message": "Diagnostic context is not implemented by this backend.",
            },
        )

    @abstractmethod
    def get_sandbox_log_diagnostics(
        self,
        sandbox_id: str,
        scope: str,
    ) -> DiagnosticResult:
        """Collect stable log diagnostics using runtime-specific policy."""
        pass

    @abstractmethod
    def get_sandbox_event_diagnostics(
        self,
        sandbox_id: str,
        scope: str,
    ) -> DiagnosticResult:
        """Collect stable event diagnostics using runtime-specific policy."""
        pass

    @abstractmethod
    def get_sandbox_logs(
        self,
        sandbox_id: str,
        tail: int = 100,
    ) -> str:
        """Return the trailing tail log lines as plain text."""
        pass

    @abstractmethod
    def get_sandbox_events(self, sandbox_id: str, limit: int = 50) -> str:
        """Return at most limit events as plain text."""
        pass

    @abstractmethod
    def get_endpoint(self, sandbox_id: str, port: int, resolve_internal: bool = False,
                     expires: Optional[int] = None,
                     use_proxy_host: bool = False) -> Endpoint:
        """
        Get sandbox access endpoint.

        Args:
            sandbox_id: Unique sandbox identifier
            port: Port number where the service is listening inside the sandbox
            resolve_internal: If True, bypass public routing for a server-reachable endpoint.
            expires: Unix epoch seconds for a signed route token. When provided, the
                endpoint is wrapped in a cryptographically signed route per OSEP-0011.
                Requires ingress gateway mode with secure_access keys configured.
            use_proxy_host: Runtime-specific hint. Docker consumes it to build
                host-mapped endpoints from the server-local proxy host when the
                server-side proxy targets the host-mapped port. Other runtimes
                currently ignore it.

        Returns:
            Endpoint: Public endpoint URL

        Raises:
            HTTPException: If sandbox not found, endpoint not available,
                or signed routes are not supported by the runtime/configuration.
        """
        pass
