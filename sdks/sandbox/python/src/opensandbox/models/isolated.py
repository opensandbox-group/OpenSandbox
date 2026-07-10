#
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
#
"""
Isolated session data models.

Pydantic models for namespace-isolated execution sessions (OSEP-0013).
"""

from datetime import datetime

from pydantic import BaseModel, ConfigDict, Field


class IsolatedWorkspaceSpec(BaseModel):
    """Workspace configuration for an isolated session."""

    path: str = Field(description="Host path to bind as workspace")
    mode: str | None = Field(
        default=None,
        description="Bind mode: 'rw' (read-write), 'overlay' (copy-on-write), or 'ro' (read-only). None = server default.",
    )

    model_config = ConfigDict(populate_by_name=True)


class EnvPassthroughSpec(BaseModel):
    """Environment variable passthrough configuration."""

    mode: str = Field(
        default="deny",
        description="Passthrough mode: 'allow' (allowlist) or 'deny' (denylist)",
    )
    keys: list[str] = Field(
        default_factory=list,
        description="Environment variable keys to allow or deny",
    )

    model_config = ConfigDict(populate_by_name=True)


class BindMount(BaseModel):
    """An explicit source-to-destination bind mount into the namespace."""

    source: str = Field(description="Host path to bind-mount into the namespace")
    dest: str | None = Field(
        default=None,
        description="Mount destination inside the namespace. Defaults to source when omitted.",
    )
    readonly: bool | None = Field(
        default=None,
        description="Mount read-only (--ro-bind) when true; read-write (--bind) otherwise.",
    )

    model_config = ConfigDict(populate_by_name=True)


class CreateIsolatedSessionRequest(BaseModel):
    """Request to create an isolated bash session."""

    workspace: IsolatedWorkspaceSpec = Field(
        description="Workspace bind configuration"
    )
    profile: str | None = Field(
        default=None,
        description="Isolation profile: 'strict' or 'balanced'. None = server default.",
    )
    extra_writable: list[str] | None = Field(
        default=None,
        description="Additional paths to bind read-write inside the namespace",
    )
    binds: list[BindMount] | None = Field(
        default=None,
        description="Additional host paths bind-mounted with explicit source-to-destination mapping",
    )
    share_net: bool | None = Field(
        default=None,
        description="Share host network namespace (default: profile-dependent)",
    )
    env_passthrough: EnvPassthroughSpec | None = Field(
        default=None,
        description="Environment variable passthrough configuration",
    )
    uid: int | None = Field(
        default=None,
        description="Run as this Unix UID inside the namespace",
    )
    gid: int | None = Field(
        default=None,
        description="Run as this Unix GID inside the namespace",
    )
    uid_mode: str | None = Field(
        default=None,
        description="How user identity is established inside the namespace: 'setpriv' (default) or 'userns'.",
    )
    idle_timeout_seconds: int | None = Field(
        default=None,
        description="Auto-destroy session after this many idle seconds (0 = no timeout)",
    )

    model_config = ConfigDict(populate_by_name=True)


class IsolatedSessionInfo(BaseModel):
    """Response after creating an isolated session."""

    session_id: str = Field(description="Unique session identifier")
    created_at: datetime = Field(description="Session creation timestamp")

    model_config = ConfigDict(populate_by_name=True)


class IsolatedSessionState(BaseModel):
    """Current state of an isolated session."""

    status: str = Field(description="Session status: 'active' or 'destroyed'")
    created_at: datetime | None = Field(
        default=None, description="Session creation timestamp"
    )
    last_run_at: datetime | None = Field(
        default=None, description="Timestamp of last code execution"
    )
    idle_remaining_seconds: int | None = Field(
        default=None, description="Seconds until idle auto-destroy (null if no timeout)"
    )

    model_config = ConfigDict(populate_by_name=True)


class IsolatedSessionSummary(BaseModel):
    """Summary of an isolated session as returned by list."""

    session_id: str = Field(description="Unique session identifier")
    status: str = Field(
        description="Session status: 'active', 'dead', or 'destroyed'"
    )
    created_at: datetime = Field(description="Session creation timestamp")
    last_run_at: datetime = Field(description="Timestamp of last code execution")
    idle_remaining_seconds: int | None = Field(
        default=None, description="Seconds until idle auto-destroy (null if no timeout)"
    )

    model_config = ConfigDict(populate_by_name=True)


class IsolatedRunOpts(BaseModel):
    """Options for running code in an isolated session."""

    envs: dict[str, str] | None = Field(
        default=None,
        description="Environment variables to inject for this run",
    )
    timeout_seconds: int | None = Field(
        default=None,
        description="Maximum execution time in seconds",
    )

    model_config = ConfigDict(populate_by_name=True)


class IsolatedCapabilities(BaseModel):
    """Isolator capabilities reported by execd."""

    available: bool = Field(
        default=False, description="Whether isolation is available"
    )
    isolator: str | None = Field(
        default=None, description="Isolator name (e.g. 'bubblewrap')"
    )
    version: str | None = Field(
        default=None, description="Isolator version string"
    )
    message: str | None = Field(
        default=None, description="Diagnostic message when unavailable"
    )
    commit_supported: bool = Field(
        default=False, description="Whether commit is supported"
    )
    diff_supported: bool = Field(
        default=False, description="Whether diff is supported"
    )

    model_config = ConfigDict(populate_by_name=True)
