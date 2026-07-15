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

"""SDK client factory stored in Click context."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import timedelta
from pathlib import Path
from typing import Any

import httpx
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.sync.manager import SandboxManagerSync
from opensandbox.sync.sandbox import SandboxSync

from opensandbox_cli.output import OutputFormatter


@dataclass
class ClientContext:
    """Shared context passed via ``ctx.obj`` to all Click commands."""

    resolved_config: dict[str, Any]
    config_path: Path
    cli_overrides: dict[str, Any]
    output: OutputFormatter = field(init=False)
    _connection_config: ConnectionConfigSync | None = field(
        default=None, init=False, repr=False
    )
    _manager: SandboxManagerSync | None = field(
        default=None, init=False, repr=False
    )
    _devops_client: httpx.Client | None = field(
        default=None, init=False, repr=False
    )

    def __post_init__(self) -> None:
        """Initialize a default human-readable formatter for the command context."""
        self.output = OutputFormatter(
            "table",
            color=self.resolved_config.get("color", True),
        )

    @property
    def connection_config(self) -> ConnectionConfigSync:
        if self._connection_config is None:
            cfg = self.resolved_config
            self._connection_config = ConnectionConfigSync(
                api_key=cfg.get("api_key"),
                domain=cfg.get("domain"),
                protocol=cfg.get("protocol", "http"),
                request_timeout=timedelta(seconds=cfg.get("request_timeout", 30)),
                use_server_proxy=cfg.get("use_server_proxy", False),
            )
        return self._connection_config

    def get_devops_client(self) -> httpx.Client:
        """Return a cached HTTP client for experimental diagnostics endpoints."""
        if self._devops_client is None:
            config = self.connection_config
            headers = dict(config.headers)
            headers.setdefault("Accept", "text/plain")
            headers.setdefault("User-Agent", config.user_agent)
            if config.api_key:
                headers["OPEN-SANDBOX-API-KEY"] = config.api_key

            self._devops_client = httpx.Client(
                base_url=config.get_base_url(),
                headers=headers,
                timeout=config.request_timeout.total_seconds(),
            )
        return self._devops_client

    def get_manager(self) -> SandboxManagerSync:
        """Return a lazily-created ``SandboxManagerSync``."""
        if self._manager is None:
            self._manager = SandboxManagerSync.create(self.connection_config)
        return self._manager

    def resolve_sandbox_id(self, sandbox_id: str) -> str:
        """Return the sandbox ID exactly as provided by the user."""
        return sandbox_id

    def connect_sandbox(
        self, sandbox_id: str, *, skip_health_check: bool = True
    ) -> SandboxSync:
        """Connect to an existing sandbox by ID."""
        sandbox_id = self.resolve_sandbox_id(sandbox_id)
        return SandboxSync.connect(
            sandbox_id,
            connection_config=self.connection_config,
            skip_health_check=skip_health_check,
        )

    def make_output(self, fmt: str) -> OutputFormatter:
        """Create and cache the current command-scoped output formatter."""
        formatter = OutputFormatter(
            fmt,
            color=self.resolved_config.get("color", True),
        )
        self.output = formatter
        return formatter

    def close(self) -> None:
        """Release resources."""
        if self._manager is not None:
            self._manager.close()
            self._manager = None
        if self._devops_client is not None:
            self._devops_client.close()
            self._devops_client = None
        if self._connection_config is not None:
            self._connection_config.close_transport_if_owned()
            self._connection_config = None
