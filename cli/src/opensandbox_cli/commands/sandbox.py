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

"""Sandbox lifecycle commands: create, list, get, kill, pause, resume, renew, endpoint, health, metrics."""

from __future__ import annotations

import json
from datetime import datetime, timedelta, timezone
from typing import Any

import click
from opensandbox.models.sandboxes import (
    NetworkPolicy,
    SandboxFilter,
    SandboxImageAuth,
    SandboxImageSpec,
    SandboxMetrics,
    SandboxState,
    Volume,
)

from opensandbox_cli.client import ClientContext
from opensandbox_cli.utils import (
    DURATION,
    KEY_VALUE,
    handle_errors,
    output_option,
    parse_nullable_duration,
    prepare_output,
)


@click.group("sandbox", invoke_without_command=True)
@click.pass_context
def sandbox_group(ctx: click.Context) -> None:
    """📦 Manage sandbox lifecycle."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


# Alias: osb sb ...
sandbox_group.name = "sandbox"

_SANDBOX_STATE_CANONICAL = {
    state.lower(): state for state in SandboxState.values()
}


def _normalize_sandbox_states(states: tuple[str, ...]) -> list[str] | None:
    """Normalize case-insensitive CLI state filters to SDK canonical values."""
    if not states:
        return None

    normalized: list[str] = []
    for state in states:
        canonical = _SANDBOX_STATE_CANONICAL.get(state.strip().lower())
        if canonical is None:
            choices = ", ".join(sorted(SandboxState.values()))
            raise click.ClickException(
                f"Invalid sandbox state '{state}'. Valid values: {choices}"
            )
        normalized.append(canonical)
    return normalized


# ---- create ---------------------------------------------------------------

@sandbox_group.command("create")
@click.option("--image", "-i", required=False, help="Container image (e.g. python:3.11). Defaults to config value if set.")
@click.option(
    "--image-auth-username",
    default=None,
    help="Registry username for pulling a private image.",
)
@click.option(
    "--image-auth-password",
    default=None,
    help="Registry password or token for pulling a private image.",
)
@click.option(
    "--timeout",
    "-t",
    "timeout_raw",
    default=None,
    help="Sandbox lifetime (e.g. 10m, 1h), 'none' for manual cleanup, or omit to use defaults.timeout / SDK default TTL.",
)
@click.option("--env", "-e", "envs", multiple=True, type=KEY_VALUE, help="Environment variable (KEY=VALUE). Repeatable.")
@click.option("--metadata", "-m", "metadata_kv", multiple=True, type=KEY_VALUE, help="Metadata (KEY=VALUE). Repeatable.")
@click.option("--extension", "extensions_kv", multiple=True, type=KEY_VALUE, help="Extension parameter (KEY=VALUE). Repeatable.")
@click.option("--resource", "resources_kv", multiple=True, type=KEY_VALUE, help="Resource limit (e.g. cpu=1 memory=2Gi). Repeatable.")
@click.option(
    "--entrypoint",
    "entrypoint",
    multiple=True,
    help="Entrypoint argv item. Repeat to build the full entrypoint.",
)
@click.option("--network-policy-file", type=click.Path(exists=True), default=None, help="Network policy JSON file.")
@click.option("--volumes-file", type=click.Path(exists=True), default=None, help="Volumes JSON file (list of volume objects).")
@click.option("--skip-health-check", is_flag=True, default=False, help="Skip waiting for sandbox readiness.")
@click.option("--ready-timeout", type=DURATION, default=None, help="Max wait time for sandbox readiness (e.g. 30s).")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def sandbox_create(
    obj: ClientContext,
    image: str | None,
    image_auth_username: str | None,
    image_auth_password: str | None,
    timeout_raw: str | None,
    envs: tuple[tuple[str, str], ...],
    metadata_kv: tuple[tuple[str, str], ...],
    extensions_kv: tuple[tuple[str, str], ...],
    resources_kv: tuple[tuple[str, str], ...],
    entrypoint: tuple[str, ...],
    network_policy_file: str | None,
    volumes_file: str | None,
    skip_health_check: bool,
    ready_timeout: timedelta | None,
    output_format: str | None,
) -> None:
    """Create a new sandbox."""
    from opensandbox.sync.sandbox import SandboxSync
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")

    if image is None:
        image = obj.resolved_config.get("default_image")
    if not image:
        raise click.ClickException(
            "Sandbox image is required. Pass --image or set defaults.image in the CLI config."
        )

    if bool(image_auth_username) != bool(image_auth_password):
        raise click.ClickException(
            "Pass both --image-auth-username and --image-auth-password together."
        )

    timeout: timedelta | None
    timeout_is_set = False
    if timeout_raw is not None:
        timeout = parse_nullable_duration(timeout_raw)
        timeout_is_set = True
    else:
        timeout = None

    if timeout_raw is None:
        default_timeout = obj.resolved_config.get("default_timeout")
        if default_timeout:
            timeout = parse_nullable_duration(default_timeout)
            timeout_is_set = True

    image_spec: SandboxImageSpec | str = image
    if image_auth_username and image_auth_password:
        image_spec = SandboxImageSpec(
            image=image,
            auth=SandboxImageAuth(
                username=image_auth_username,
                password=image_auth_password,
            ),
        )

    kwargs: dict = {
        "connection_config": obj.connection_config,
        "skip_health_check": skip_health_check,
    }
    if timeout_is_set:
        kwargs["timeout"] = timeout
    if ready_timeout is not None:
        kwargs["ready_timeout"] = ready_timeout
    if envs:
        kwargs["env"] = dict(envs)
    if metadata_kv:
        kwargs["metadata"] = dict(metadata_kv)
    if extensions_kv:
        kwargs["extensions"] = dict(extensions_kv)
    if resources_kv:
        kwargs["resource"] = dict(resources_kv)
    if entrypoint:
        kwargs["entrypoint"] = list(entrypoint)
    if network_policy_file:
        with open(network_policy_file) as f:
            kwargs["network_policy"] = NetworkPolicy(**json.load(f))
    if volumes_file:
        with open(volumes_file) as f:
            raw_volumes = json.load(f)
        if not isinstance(raw_volumes, list):
            raise click.ClickException(
                f"Volumes file must contain a JSON array, got {type(raw_volumes).__name__}."
            )
        kwargs["volumes"] = [Volume(**item) for item in raw_volumes]

    with obj.output.spinner("Creating sandbox..."):
        sandbox = SandboxSync.create(image_spec, **kwargs)
    obj.output.success_panel(
        {
            "id": sandbox.id,
            "image": image,
            "status": "created",
            "timeout": _describe_create_timeout(timeout_is_set, timeout),
        },
        title="Sandbox Created",
    )


# ---- list -----------------------------------------------------------------

@sandbox_group.command("list")
@click.option("--state", "-s", "states", multiple=True, help="Filter by state (Pending, Running, Paused, ...). Repeatable.")
@click.option("--metadata", "-m", "metadata_kv", multiple=True, type=KEY_VALUE, help="Metadata filter (KEY=VALUE). Repeatable.")
@click.option("--page", type=click.IntRange(min=1), default=None, help="Page number (1-indexed).")
@click.option("--page-size", type=click.IntRange(min=1), default=None, help="Items per page.")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def sandbox_list(
    obj: ClientContext,
    states: tuple[str, ...],
    metadata_kv: tuple[tuple[str, str], ...],
    page: int | None,
    page_size: int | None,
    output_format: str | None,
) -> None:
    """List sandboxes."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    mgr = obj.get_manager()
    filt = SandboxFilter(
        states=_normalize_sandbox_states(states),
        metadata=dict(metadata_kv) if metadata_kv else None,
        page=page,
        page_size=page_size,
    )
    with obj.output.spinner("Fetching sandboxes..."):
        result = mgr.list_sandbox_infos(filt)
    if not result.sandbox_infos:
        if obj.output.fmt in ("json", "yaml"):
            obj.output.print_dict(
                {
                    "items": [],
                    "pagination": result.pagination.model_dump(mode="json"),
                },
                title="Sandboxes",
            )
        else:
            obj.output.info("No sandboxes found.")
        return

    raw_rows = [info.model_dump(mode="json") for info in result.sandbox_infos]

    # For machine-readable formats, preserve the original structure
    if obj.output.fmt in ("json", "yaml"):
        obj.output.print_dict(
            {
                "items": raw_rows,
                "pagination": result.pagination.model_dump(mode="json"),
            },
            title="Sandboxes",
        )
        return

    # Flatten nested status/image objects for clean table display
    rows = []
    for d in raw_rows:
        flat = dict(d)
        status_val = flat.get("status")
        if isinstance(status_val, dict):
            flat["status"] = status_val.get("state", str(status_val))
        image_val = flat.get("image")
        if isinstance(image_val, dict):
            flat["image"] = image_val.get("image", str(image_val))
        rows.append(flat)

    obj.output.print_rows(
        rows,
        columns=["id", "status", "image", "created_at", "expires_at"],
        title="Sandboxes",
    )


# ---- get ------------------------------------------------------------------

@sandbox_group.command("get")
@click.argument("sandbox_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def sandbox_get(obj: ClientContext, sandbox_id: str, output_format: str | None) -> None:
    """Get sandbox details."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox_id = obj.resolve_sandbox_id(sandbox_id)
    mgr = obj.get_manager()
    info = mgr.get_sandbox_info(sandbox_id)
    d = info.model_dump(mode="json")

    # For machine-readable formats, preserve the original structure
    if obj.output.fmt in ("json", "yaml"):
        obj.output.print_dict(d, title="Sandbox Info")
        return

    # Flatten nested objects for clean table display
    status_val = d.get("status")
    if isinstance(status_val, dict):
        d["status"] = status_val.get("state", str(status_val))
        if status_val.get("reason"):
            d["status_reason"] = status_val["reason"]
        if status_val.get("message"):
            d["status_message"] = status_val["message"]
    image_val = d.get("image")
    if isinstance(image_val, dict):
        d["image"] = image_val.get("image", str(image_val))
    obj.output.print_dict(d, title="Sandbox Info")


# ---- kill -----------------------------------------------------------------

@sandbox_group.command("kill")
@click.argument("sandbox_ids", nargs=-1, required=True)
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def sandbox_kill(
    obj: ClientContext, sandbox_ids: tuple[str, ...], output_format: str | None
) -> None:
    """Terminate one or more sandboxes."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    mgr = obj.get_manager()
    rows: list[dict[str, str]] = []
    for sid in sandbox_ids:
        resolved = obj.resolve_sandbox_id(sid)
        with obj.output.spinner(f"Killing sandbox {resolved}..."):
            mgr.kill_sandbox(resolved)
        rows.append({"sandbox_id": resolved, "status": "terminated"})
    obj.output.print_rows(rows, columns=["sandbox_id", "status"], title="Sandboxes")


# ---- pause ----------------------------------------------------------------

@sandbox_group.command("pause")
@click.argument("sandbox_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def sandbox_pause(obj: ClientContext, sandbox_id: str, output_format: str | None) -> None:
    """Pause a running sandbox."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox_id = obj.resolve_sandbox_id(sandbox_id)
    mgr = obj.get_manager()
    with obj.output.spinner("Pausing sandbox..."):
        mgr.pause_sandbox(sandbox_id)
    obj.output.success(f"Sandbox paused: {sandbox_id}")


# ---- resume ---------------------------------------------------------------

@sandbox_group.command("resume")
@click.argument("sandbox_id")
@click.option("--skip-health-check", is_flag=True, default=False, help="Skip waiting for sandbox readiness after resume.")
@click.option("--resume-timeout", type=DURATION, default=None, help="Max wait time for sandbox readiness after resume (e.g. 30s).")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def sandbox_resume(
    obj: ClientContext,
    sandbox_id: str,
    skip_health_check: bool,
    resume_timeout: timedelta | None,
    output_format: str | None,
) -> None:
    """Resume a paused sandbox."""
    from opensandbox.sync.sandbox import SandboxSync
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")

    sandbox_id = obj.resolve_sandbox_id(sandbox_id)
    sandbox = None
    try:
        kwargs = {
            "connection_config": obj.connection_config,
            "skip_health_check": skip_health_check,
        }
        if resume_timeout is not None:
            kwargs["resume_timeout"] = resume_timeout

        with obj.output.spinner("Resuming sandbox..."):
            sandbox = SandboxSync.resume(sandbox_id, **kwargs)
        obj.output.success(f"Sandbox resumed: {sandbox_id}")
    finally:
        if sandbox is not None:
            sandbox.close()


# ---- renew ----------------------------------------------------------------

@sandbox_group.command("renew")
@click.argument("sandbox_id")
@click.option("--timeout", "-t", required=True, type=DURATION, help="New TTL duration (e.g. 30m, 2h).")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def sandbox_renew(
    obj: ClientContext,
    sandbox_id: str,
    timeout: timedelta,
    output_format: str | None,
) -> None:
    """Renew sandbox expiration."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox_id = obj.resolve_sandbox_id(sandbox_id)
    mgr = obj.get_manager()
    with obj.output.spinner("Renewing sandbox..."):
        resp = mgr.renew_sandbox(sandbox_id, timeout)
    obj.output.success_panel(
        {"sandbox_id": sandbox_id, "expires_at": str(resp.expires_at)},
        title="Sandbox Renewed",
    )


# ---- endpoint -------------------------------------------------------------

@sandbox_group.command("endpoint")
@click.argument("sandbox_id")
@click.option("--port", "-p", required=True, type=click.IntRange(min=1, max=65535), help="Port number.")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def sandbox_endpoint(
    obj: ClientContext, sandbox_id: str, port: int, output_format: str | None
) -> None:
    """Get the public endpoint for a sandbox port."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        ep = sandbox.get_endpoint(port)
        obj.output.print_model(ep, title="Sandbox Endpoint")
    finally:
        sandbox.close()


# ---- health ---------------------------------------------------------------

@sandbox_group.command("health")
@click.argument("sandbox_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def sandbox_health(
    obj: ClientContext, sandbox_id: str, output_format: str | None
) -> None:
    """Check sandbox health."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        healthy = sandbox.is_healthy()
        if obj.output.fmt == "table":
            if healthy:
                obj.output.success(f"Sandbox {sandbox_id} is healthy")
            else:
                obj.output.error(f"Sandbox {sandbox_id} is unhealthy")
        else:
            obj.output.print_dict(
                {"sandbox_id": sandbox_id, "healthy": healthy},
                title="Health Check",
            )
    finally:
        sandbox.close()


# ---- metrics --------------------------------------------------------------

@sandbox_group.command("metrics")
@click.argument("sandbox_id")
@click.option("--watch", is_flag=True, default=False, help="Stream metrics updates in real time.")
@output_option("table", "json", "yaml", "raw")
@click.pass_obj
@handle_errors
def sandbox_metrics(
    obj: ClientContext,
    sandbox_id: str,
    watch: bool,
    output_format: str | None,
) -> None:
    """Get sandbox resource metrics."""
    fallback = "raw" if watch else "table"
    prepare_output(
        obj, output_format, allowed=("table", "json", "yaml", "raw"), fallback=fallback
    )
    sandbox = obj.connect_sandbox(sandbox_id)
    try:
        if watch:
            _watch_sandbox_metrics(obj, sandbox)
            return

        m = sandbox.get_metrics()
        obj.output.print_model(m, title="Sandbox Metrics")
    finally:
        sandbox.close()


def _watch_sandbox_metrics(obj: ClientContext, sandbox) -> None:  # type: ignore[no-untyped-def]
    """Stream sandbox metrics from the execd SSE endpoint."""
    client = getattr(sandbox.metrics, "_httpx_client", None)
    if client is None:
        raise click.ClickException("Streaming metrics are unavailable for this sandbox connection.")

    headers = {"Accept": "text/event-stream"}

    try:
        with client.stream("GET", "/metrics/watch", headers=headers) as response:
            response.raise_for_status()
            for line in response.iter_lines():
                metric, warning = _parse_metric_stream_line(line)
                if warning:
                    obj.output.warning(warning)
                    continue
                if metric is None:
                    continue
                _render_stream_metric(obj, metric)
    except KeyboardInterrupt:
        return


def _parse_metric_stream_line(line: str) -> tuple[SandboxMetrics | None, str | None]:
    """Parse one line from the metrics SSE stream."""
    stripped = line.strip()
    if not stripped or stripped.startswith((":","event:", "id:", "retry:")):
        return None, None

    payload = stripped[5:].strip() if stripped.startswith("data:") else stripped
    if not payload:
        return None, None

    decoded: Any = json.loads(payload)
    if isinstance(decoded, dict) and decoded.get("error"):
        return None, f"Metrics stream error: {decoded['error']}"
    return SandboxMetrics.model_validate(decoded), None


def _describe_create_timeout(
    timeout_is_set: bool, timeout: timedelta | None
) -> str:
    """Describe the sandbox timeout mode shown in create output."""
    if not timeout_is_set:
        return "sdk-default"
    if timeout is None:
        return "manual-cleanup"
    return str(timeout)


def _render_stream_metric(obj: ClientContext, metric: SandboxMetrics) -> None:
    """Render one streaming metrics sample."""
    if obj.output.fmt in ("table", "raw"):
        timestamp = datetime.fromtimestamp(metric.timestamp / 1000, tz=timezone.utc).isoformat()
        click.echo(
            " ".join(
                [
                    f"[{timestamp}]",
                    f"cpu={metric.cpu_used_percentage:.2f}%",
                    f"cores={metric.cpu_count:g}",
                    f"mem={metric.memory_used_in_mib:.2f}/{metric.memory_total_in_mib:.2f}MiB",
                ]
            )
        )
        return

    if obj.output.fmt == "json":
        click.echo(json.dumps(metric.model_dump(mode="json"), default=str))
        return

    if obj.output.fmt == "yaml":
        obj.output.print_model(metric)
        return
