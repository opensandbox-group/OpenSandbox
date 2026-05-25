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
Synchronous Sandbox client implementation.
"""

import logging
import time
from collections.abc import Callable
from datetime import datetime, timedelta, timezone
from typing import Any

from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.constants import DEFAULT_EGRESS_PORT, DEFAULT_EXECD_PORT
from opensandbox.exceptions import (
    InvalidArgumentException,
    SandboxException,
    SandboxInternalException,
    SandboxReadyTimeoutException,
)
from opensandbox.models.diagnostics import DiagnosticContent
from opensandbox.models.sandboxes import (
    CreateSnapshotRequest,
    NetworkPolicy,
    NetworkRule,
    PlatformSpec,
    SandboxEndpoint,
    SandboxImageSpec,
    SandboxInfo,
    SandboxMetrics,
    SandboxRenewResponse,
    SnapshotInfo,
    Volume,
)
from opensandbox.sync.adapters.factory import AdapterFactorySync
from opensandbox.sync.services import (
    CommandsSync,
    DiagnosticsSync,
    EgressSync,
    FilesystemSync,
    HealthSync,
    MetricsSync,
    SandboxesSync,
)

logger = logging.getLogger(__name__)


class SandboxSync:
    """
    Main synchronous entrypoint for the Open Sandbox SDK.

    This class mirrors the async :class:`opensandbox.sandbox.Sandbox` API, but all
    operations are **blocking** and executed in the current thread.

    Key Features:

    - **Secure Isolation**: Complete Linux OS access in isolated containers
    - **File System Operations**: Create, read, update, delete files and directories
    - **Multi-language Execution**: Support for Python, Java, Bash, and other languages
    - **Real-time Command Execution**: Streaming output via SSE (Server-Sent Events)
    - **Resource Management**: CPU, memory, and storage constraints
    - **Lifecycle Management**: Create, pause, resume, terminate operations
    - **Health Monitoring**: Readiness polling and status tracking

    Notes:

    - **Blocking**: Do not call these methods directly from an asyncio event loop thread.
      If you need non-blocking behavior, prefer the async :class:`~opensandbox.sandbox.Sandbox`.
    - **Resource cleanup**: :meth:`close` closes *local* HTTP resources only. It does **not**
      terminate the remote sandbox instance. Call :meth:`kill` to stop the remote sandbox.

    Usage Example:

    ```python
    from datetime import timedelta
    from opensandbox.models.sandboxes import SandboxImageSpec
    from opensandbox.models.execd import RunCommandOpts
    from opensandbox.sync.sandbox import SandboxSync

    # Create a sandbox (blocking)
    sandbox = SandboxSync.create(
        "python:3.11",
        resource={"cpu": "1", "memory": "500Mi"},
        timeout=timedelta(minutes=30),
    )

    # Use the sandbox
    sandbox.files.write_file("script.py", "print('Hello World')")
    result = sandbox.commands.run("python script.py")

    # Always clean up resources
    sandbox.kill()   # terminate remote sandbox
    sandbox.close()  # close local HTTP resources

    # Or use a context manager for automatic close():
    with SandboxSync.create("python:3.11") as sandbox:
        # Note on lifecycle:
        # - Exiting the context manager will call `sandbox.close()` (local HTTP resources only).
        # - You must still call `sandbox.kill()` to terminate the remote sandbox instance.
        sandbox.commands.run("python -c \"print('hi')\"")
        sandbox.kill()
    ```
    """

    def __init__(
        self,
        sandbox_id: str,
        sandbox_service: SandboxesSync,
        filesystem_service: FilesystemSync,
        command_service: CommandsSync,
        health_service: HealthSync,
        metrics_service: MetricsSync,
        egress_service: EgressSync,
        connection_config: ConnectionConfigSync,
        diagnostics_service: DiagnosticsSync | None = None,
        custom_health_check: Callable[["SandboxSync"], bool] | None = None,
    ) -> None:
        """
        Internal constructor for SandboxSync. Use :meth:`create` or :meth:`connect` instead.
        """
        self.id = sandbox_id
        self._sandbox_service = sandbox_service
        self._filesystem_service = filesystem_service
        self._command_service = command_service
        self._health_service = health_service
        self._metrics_service = metrics_service
        self._egress_service = egress_service
        self._connection_config = connection_config
        self._diagnostics_service = diagnostics_service or AdapterFactorySync(
            connection_config
        ).create_diagnostics_service()
        self._custom_health_check = custom_health_check

    @property
    def files(self) -> FilesystemSync:
        """
        Provides access to file system operations within the sandbox.

        Allows writing, reading, listing, and deleting files and directories.
        """
        return self._filesystem_service

    @property
    def commands(self) -> CommandsSync:
        """
        Provides access to command execution operations.

        Supports both one-shot command execution and SSE streaming output.
        """
        return self._command_service

    @property
    def metrics(self) -> MetricsSync:
        """
        Provides access to sandbox metrics and monitoring.

        Allows retrieving resource usage statistics (CPU, memory) and other performance metrics.
        """
        return self._metrics_service

    @property
    def diagnostics(self) -> DiagnosticsSync:
        """
        Provides access to sandbox diagnostic log and event descriptors.
        """
        return self._diagnostics_service

    @property
    def connection_config(self) -> ConnectionConfigSync:
        """Provides access to the connection configuration (including shared transport)."""
        return self._connection_config

    def get_info(self) -> SandboxInfo:
        """
        Get the current status of this sandbox.

        Returns:
            Current sandbox status including state and metadata

        Raises:
            SandboxException: if status cannot be retrieved
        """
        return self._sandbox_service.get_sandbox_info(self.id)

    def get_endpoint(self, port: int) -> SandboxEndpoint:
        """
        Get a specific network endpoint for this sandbox.

        Args:
            port: The port number to get the endpoint for

        Returns:
            Endpoint information including connection details

        Raises:
            SandboxException: if endpoint cannot be retrieved
        """
        return self._sandbox_service.get_sandbox_endpoint(
            self.id, port, self.connection_config.use_server_proxy
        )

    def get_signed_endpoint(self, port: int, expires: int) -> SandboxEndpoint:
        """
        Get a signed endpoint URL with an OSEP-0011 route token.

        Args:
            port: The port number to get the endpoint for
            expires: Unix epoch seconds for the signed route token expiry

        Returns:
            Endpoint information with a signed URL

        Raises:
            SandboxException: if endpoint cannot be retrieved
        """
        return self._sandbox_service.get_signed_sandbox_endpoint(
            self.id, port, expires, self.connection_config.use_server_proxy
        )

    def get_metrics(self) -> SandboxMetrics:
        """
        Get the current resource usage metrics for this sandbox.

        Returns:
            Current sandbox metrics including CPU, memory, and I/O statistics

        Raises:
            SandboxException: if metrics cannot be retrieved
        """
        return self._metrics_service.get_metrics(self.id)

    def get_diagnostic_logs(self, scope: str) -> DiagnosticContent:
        """
        Get diagnostic log content for this sandbox.

        Args:
            scope: Required diagnostic scope such as "container", "lifecycle", or "all".
        """
        return self._diagnostics_service.get_logs(self.id, scope)

    def get_diagnostic_events(self, scope: str) -> DiagnosticContent:
        """
        Get diagnostic event content for this sandbox.

        Args:
            scope: Required diagnostic scope such as "runtime", "lifecycle", or "all".
        """
        return self._diagnostics_service.get_events(self.id, scope)

    def renew(self, timeout: timedelta) -> SandboxRenewResponse:
        """
        Renew the sandbox expiration time to delay automatic termination.

        The new expiration time will be set to the current time plus the provided duration.

        Args:
            timeout: Duration to add to the current time to set the new expiration

        Returns:
            Renew response including the new expiration time.

        Raises:
            SandboxException: if the operation fails
        """
        # Use timezone-aware UTC datetime to avoid cross-timezone ambiguity.
        new_expiration = datetime.now(timezone.utc) + timeout
        logger.info(
            "Renewing sandbox %s timeout, estimated expiration: %s",
            self.id,
            new_expiration,
        )
        return self._sandbox_service.renew_sandbox_expiration(self.id, new_expiration)

    def patch_metadata(self, patch: dict[str, str | None]) -> SandboxInfo:
        """
        Patch sandbox metadata.

        String values add or replace keys; None deletes keys.
        """
        return self._sandbox_service.patch_sandbox_metadata(self.id, patch)

    def create_snapshot(self, name: str | None = None) -> SnapshotInfo:
        """Create a persistent snapshot from this sandbox (blocking)."""
        return self._sandbox_service.create_snapshot(
            self.id, CreateSnapshotRequest(name=name)
        )

    def get_egress_policy(self) -> NetworkPolicy:
        """
        Get current egress policy for this sandbox.
        """
        return self._egress_service.get_policy()

    def patch_egress_rules(self, rules: list[NetworkRule]) -> None:
        """
        Patch egress rules for this sandbox using sidecar merge semantics.

        Rules in this patch payload take priority over existing rules with the
        same target. Existing rules for other targets remain unchanged. Within a
        single patch payload, the first rule for a target wins.

        This operation does not replace the entire policy and does not change
        the current defaultAction.
        """
        self._egress_service.patch_rules(rules)

    def pause(self) -> None:
        """
        Pause the sandbox while preserving its state.

        The sandbox will transition to PAUSED state and can be resumed later.
        All running processes will be suspended.

        Raises:
            SandboxException: if pause operation fails
        """
        logger.info("Pausing sandbox: %s", self.id)
        self._sandbox_service.pause_sandbox(self.id)


    def kill(self) -> None:
        """
        Send a termination signal to the remote sandbox instance.

        This is an irreversible operation that stops the sandbox immediately.

        Note: This method does NOT close the local resources. Use :meth:`close` or
        the sync context manager to clean up local resources.

        Raises:
            SandboxException: if termination fails
        """
        logger.info("Killing sandbox: %s", self.id)
        self._sandbox_service.kill_sandbox(self.id)

    def close(self) -> None:
        """
        Close local resources associated with this sandbox.

        This method closes HTTP client resources and other local resources.
        It does NOT terminate the remote sandbox instance. Call :meth:`kill` first
        if you want to terminate the remote sandbox.

        Note: This method logs errors but does not raise exceptions to avoid
        issues in context manager cleanup.
        """
        try:
            self._connection_config.close_transport_if_owned()
            logger.debug("Closed resources for sandbox %s", self.id)
        except Exception as e:
            logger.warning("Error closing resources for sandbox %s: %s", self.id, e, exc_info=True)

    def is_healthy(self) -> bool:
        """
        Check if the sandbox is healthy and responsive.

        Returns:
            True if sandbox is healthy, False otherwise
        """
        if self._custom_health_check:
            return self._custom_health_check(self)
        try:
            return self._health_service.ping(self.id)
        except Exception:
            return False

    def check_ready(self, timeout: timedelta, polling_interval: timedelta) -> None:
        """
        Wait for the sandbox to pass health checks with polling.

        Args:
            timeout: Maximum time to wait for health check to pass
            polling_interval: Time between health check attempts

        Raises:
            SandboxReadyTimeoutException: if health check doesn't pass within timeout
            SandboxException: if health check fails
        """
        logger.info(
            "Waiting for sandbox %s to pass health check (timeout: %ss)",
            self.id,
            timeout.total_seconds(),
        )

        deadline = time.time() + timeout.total_seconds()
        attempt = 0
        last_exception: Exception | None = None

        while time.time() < deadline:
            attempt += 1
            logger.debug("Health check attempt #%s for sandbox %s", attempt, self.id)
            try:
                if self.is_healthy():
                    logger.info(
                        "Sandbox %s passed health check after %s attempts",
                        self.id,
                        attempt,
                    )
                    return
                last_exception = None
            except Exception as e:
                last_exception = e

            time.sleep(polling_interval.total_seconds())

        error_detail = (
            f"Last error: {last_exception}"
            if last_exception
            else "Health check returned false continuously"
        )
        connection_detail = (
            f"ConnectionConfig(domain={self.connection_config.get_domain()}, "
            f"use_server_proxy={self.connection_config.use_server_proxy})"
        )
        if self.connection_config.use_server_proxy:
            hint = (
                "Hint: server proxy mode is enabled. Check server-to-sandbox connectivity "
                "and server API key/auth configuration."
            )
        else:
            hint = (
                "Hint: direct sandbox endpoint access is enabled. If the SDK cannot directly "
                "reach sandbox network/ports, set ConnectionConfigSync(use_server_proxy=True). "
                "For Docker bridge deployments where server runs in a container, also configure "
                "server [docker].host_ip to a host-reachable address."
            )
        final_message = (
            f"Sandbox health check timed out after {timeout.total_seconds()}s "
            f"({attempt} attempts). {error_detail}. {connection_detail}. {hint}"
        )
        logger.error(final_message)
        raise SandboxReadyTimeoutException(final_message)

    @classmethod
    def create(
        cls,
        image: SandboxImageSpec | str | None = None,
        *,
        snapshot_id: str | None = None,
        timeout: timedelta | None = timedelta(minutes=10),
        ready_timeout: timedelta = timedelta(seconds=30),
        env: dict[str, str] | None = None,
        metadata: dict[str, str] | None = None,
        resource: dict[str, str] | None = None,
        platform: PlatformSpec | None = None,
        network_policy: NetworkPolicy | None = None,
        extensions: dict[str, str] | None = None,
        secure_access: bool = False,
        entrypoint: list[str] | None = None,
        volumes: list[Volume] | None = None,
        connection_config: ConnectionConfigSync | None = None,
        health_check: Callable[["SandboxSync"], bool] | None = None,
        health_check_polling_interval: timedelta = timedelta(milliseconds=200),
        skip_health_check: bool = False,
    ) -> "SandboxSync":
        """
        Create a new sandbox instance with the specified configuration (blocking).

        Args:
            image: Container image specification including image reference and optional auth
            timeout: Maximum sandbox lifetime. Pass None to require explicit cleanup.
            ready_timeout: Maximum time to wait for sandbox to become ready
            env: Environment variables for the sandbox
            metadata: Custom metadata for the sandbox
            resource: Resource limits (CPU, memory, etc.)
            network_policy: Optional outbound network policy (egress).
            extensions: Opaque extension parameters passed through to the server as-is.
                Prefer namespaced keys (e.g. ``storage.id``).
            secure_access: Whether to enable secured access for sandbox endpoints.
            entrypoint: Command to run as entrypoint
            volumes: Optional list of volumes to mount in the sandbox.
            connection_config: Connection configuration
            health_check: Custom sync health check function
            health_check_polling_interval: Time between health check attempts
            skip_health_check: If True, do NOT wait for sandbox readiness/health; returned instance may not be ready yet.

        Returns:
            Fully configured and ready SandboxSync instance

        Raises:
            SandboxException: if sandbox creation or initialization fails
        """
        if (image is None) == (snapshot_id is None):
            raise InvalidArgumentException(
                "Exactly one of image or snapshot_id must be specified"
            )

        config = (connection_config or ConnectionConfigSync()).with_transport_if_missing()
        entrypoint = entrypoint or ["tail", "-f", "/dev/null"]
        env = env or {}
        metadata = metadata or {}
        resource = resource or {"cpu": "1", "memory": "2Gi"}
        extensions = extensions or {}

        if isinstance(image, str):
            image = SandboxImageSpec(image=image)

        startup_source = image.image if image is not None else snapshot_id
        timeout_log = "manual-cleanup" if timeout is None else f"{timeout.total_seconds()}s"
        logger.info(
            "Creating sandbox with startup source: %s (timeout: %s)",
            startup_source,
            timeout_log,
        )
        factory = AdapterFactorySync(config)
        sandbox_id: str | None = None
        sandbox_service: SandboxesSync | None = None

        try:
            sandbox_service = factory.create_sandbox_service()
            response = sandbox_service.create_sandbox(
                spec=image,
                entrypoint=entrypoint,
                env=env,
                metadata=metadata,
                timeout=timeout,
                resource=resource,
                network_policy=network_policy,
                extensions=extensions,
                volumes=volumes,
                platform=platform,
                secure_access=secure_access,
                snapshot_id=snapshot_id,
            )
            sandbox_id = response.id
            execd_endpoint = sandbox_service.get_sandbox_endpoint(
                response.id, DEFAULT_EXECD_PORT, config.use_server_proxy
            )
            egress_endpoint = sandbox_service.get_sandbox_endpoint(
                response.id, DEFAULT_EGRESS_PORT, config.use_server_proxy
            )

            sandbox = cls(
                sandbox_id=response.id,
                sandbox_service=sandbox_service,
                filesystem_service=factory.create_filesystem_service(execd_endpoint),
                command_service=factory.create_command_service(execd_endpoint),
                health_service=factory.create_health_service(execd_endpoint),
                metrics_service=factory.create_metrics_service(execd_endpoint),
                egress_service=factory.create_egress_service(egress_endpoint),
                diagnostics_service=factory.create_diagnostics_service(),
                connection_config=config,
                custom_health_check=health_check,
            )

            if not skip_health_check:
                sandbox.check_ready(ready_timeout, health_check_polling_interval)
                logger.info("Sandbox %s is ready", sandbox.id)
            else:
                logger.info(
                    "Sandbox %s created (skip_health_check=true, sandbox may not be ready yet)",
                    sandbox.id,
                )

            return sandbox
        except Exception as e:
            if sandbox_id and sandbox_service:
                try:
                    logger.warning(
                        "Sandbox creation failed during initialization. Attempting to terminate zombie sandbox: %s",
                        sandbox_id,
                    )
                    sandbox_service.kill_sandbox(sandbox_id)
                except Exception:
                    pass
            config.close_transport_if_owned()
            if isinstance(e, SandboxException):
                raise
            raise SandboxInternalException(f"Internal exception when creating sandbox: {e}") from e

    @classmethod
    def connect(
        cls,
        sandbox_id: str,
        connection_config: ConnectionConfigSync | None = None,
        health_check: Callable[["SandboxSync"], bool] | None = None,
        connect_timeout: timedelta = timedelta(seconds=30),
        health_check_polling_interval: timedelta = timedelta(milliseconds=200),
        skip_health_check: bool = False,
    ) -> "SandboxSync":
        """
        Connect to an existing sandbox instance by ID (blocking).

        Args:
            sandbox_id: ID of the existing sandbox
            connection_config: Connection configuration
            health_check: Custom sync health check function
            connect_timeout: Max time to wait for sandbox readiness/health after connecting.
            health_check_polling_interval: Polling interval used while waiting for readiness/health.
            skip_health_check: If True, do NOT wait for readiness/health; returned instance may not be ready yet.

        Returns:
            Connected SandboxSync instance

        Raises:
            InvalidArgumentException: if required configuration is missing
            SandboxException: if sandbox connection fails
        """
        if not sandbox_id:
            raise InvalidArgumentException("Sandbox ID must be specified")
        # Accept any string identifier.
        sandbox_id = str(sandbox_id)

        config = (connection_config or ConnectionConfigSync()).with_transport_if_missing()
        logger.info("Connecting to sandbox: %s", sandbox_id)
        factory = AdapterFactorySync(config)

        try:
            sandbox_service = factory.create_sandbox_service()
            execd_endpoint = sandbox_service.get_sandbox_endpoint(
                sandbox_id, DEFAULT_EXECD_PORT, config.use_server_proxy
            )
            egress_endpoint = sandbox_service.get_sandbox_endpoint(
                sandbox_id, DEFAULT_EGRESS_PORT, config.use_server_proxy
            )

            sandbox = cls(
                sandbox_id=sandbox_id,
                sandbox_service=sandbox_service,
                filesystem_service=factory.create_filesystem_service(execd_endpoint),
                command_service=factory.create_command_service(execd_endpoint),
                health_service=factory.create_health_service(execd_endpoint),
                metrics_service=factory.create_metrics_service(execd_endpoint),
                egress_service=factory.create_egress_service(egress_endpoint),
                diagnostics_service=factory.create_diagnostics_service(),
                connection_config=config,
                custom_health_check=health_check,
            )

            if not skip_health_check:
                sandbox.check_ready(connect_timeout, health_check_polling_interval)
            else:
                logger.info(
                    "Connected to sandbox %s (skip_health_check=true, sandbox may not be ready yet)",
                    sandbox_id,
                )

            logger.info("Connected to sandbox %s", sandbox_id)
            return sandbox
        except Exception as e:
            config.close_transport_if_owned()
            if isinstance(e, SandboxException):
                raise
            raise SandboxInternalException(f"Failed to connect to sandbox: {e}") from e

    @classmethod
    def resume(
            cls,
            sandbox_id: str,
            connection_config: ConnectionConfigSync | None = None,
            health_check: Callable[["SandboxSync"], bool] | None = None,
            resume_timeout: timedelta = timedelta(seconds=30),
            health_check_polling_interval: timedelta = timedelta(milliseconds=200),
            skip_health_check: bool = False,
    ) -> "SandboxSync":
        """
        Resume a paused sandbox by ID and return a new, usable SandboxSync instance.

        This method performs the server-side resume operation, then re-resolves the execd endpoint
        (which may change across pause/resume on some backends), rebuilds service adapters, and
        optionally waits for readiness/health.

        Args:
            sandbox_id: ID of the paused sandbox to resume.
            connection_config: Connection configuration (shared transport, headers, timeouts).
            health_check: Optional custom sync health check function (falls back to ping).
            resume_timeout: Max time to wait for sandbox readiness/health after resuming.
            health_check_polling_interval: Polling interval used while waiting for readiness/health.
            skip_health_check: If True, do NOT wait for readiness/health; returned instance may not be ready yet.
        """
        if not sandbox_id:
            raise InvalidArgumentException("Sandbox ID must be specified")

        # Accept any string identifier.
        sandbox_id = str(sandbox_id)

        config = (connection_config or ConnectionConfigSync()).with_transport_if_missing()

        logger.info("Resuming sandbox: %s", sandbox_id)
        factory = AdapterFactorySync(config)

        try:
            sandbox_service = factory.create_sandbox_service()
            sandbox_service.resume_sandbox(sandbox_id)

            execd_endpoint = sandbox_service.get_sandbox_endpoint(
                sandbox_id, DEFAULT_EXECD_PORT, config.use_server_proxy
            )
            egress_endpoint = sandbox_service.get_sandbox_endpoint(
                sandbox_id, DEFAULT_EGRESS_PORT, config.use_server_proxy
            )

            sandbox = cls(
                sandbox_id=sandbox_id,
                sandbox_service=sandbox_service,
                filesystem_service=factory.create_filesystem_service(execd_endpoint),
                command_service=factory.create_command_service(execd_endpoint),
                health_service=factory.create_health_service(execd_endpoint),
                metrics_service=factory.create_metrics_service(execd_endpoint),
                egress_service=factory.create_egress_service(egress_endpoint),
                diagnostics_service=factory.create_diagnostics_service(),
                connection_config=config,
                custom_health_check=health_check,
            )

            if not skip_health_check:
                sandbox.check_ready(resume_timeout, health_check_polling_interval)
            else:
                logger.info(
                    "Resumed sandbox %s (skip_health_check=true, sandbox may not be ready yet)",
                    sandbox_id,
                )

            return sandbox
        except Exception as e:
            config.close_transport_if_owned()
            if isinstance(e, SandboxException):
                raise
            raise SandboxInternalException(f"Failed to resume sandbox: {e}") from e


    def __enter__(self) -> "SandboxSync":
        """Sync context manager entry."""
        return self

    def __exit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> None:
        """Sync context manager exit."""
        self.close()
