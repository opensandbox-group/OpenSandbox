# Copyright 2026 The OpenSandbox Authors.
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
from opensandbox.adapters.converter import MetricsModelConverter
from opensandbox.api.execd.models import Metrics
from opensandbox.models.sandboxes import (
    CredentialProxyConfig,
    NetworkPolicy,
    PlatformSpec,
    SandboxFilter,
    SandboxImageAuth,
    SandboxImageSpec,
    SandboxLifecycle,
    SandboxMetrics,
    SandboxState,
    Volume,
)
from pydantic import BaseModel, ValidationError

from opensandbox_cli.client import ClientContext
from opensandbox_cli.utils import (
    DURATION,
    KEY_VALUE,
    handle_errors,
    load_json_object,
    output_option,
    parse_nullable_duration,
    prepare_output,
    validation_message,
)


@click.group("sandbox", invoke_without_command=True)
@click.pass_context
def sandbox_group(ctx: click.Context) -> None:
    """📦 Manage sandbox lifecycle."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


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
    "--template",
    default=None,
    help="Template ID (tpl_...) of a Succeeded template to create the sandbox from. Mutually exclusive with --image and --snapshot-id.",
)
@click.option(
    "--snapshot-id",
    "snapshot_id",
    default=None,
    help="Snapshot ID to restore the sandbox from. Mutually exclusive with --image and --template.",
)
@click.option(
    "--file",
    "-f",
    "request_file",
    type=click.Path(exists=True, dir_okay=False),
    default=None,
    help=(
        "JSON request file in the public CreateSandboxRequest wire format "
        "(camelCase keys: image/templateId/snapshotId, timeout in seconds, resourceLimits, "
        "networkPolicy, volumes, ...). Mutually exclusive with request-building flags; "
        "--skip-health-check, --ready-timeout and -o still apply."
    ),
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
@click.option(
    "--credential-proxy",
    is_flag=True,
    default=False,
    help="Enable Credential Vault transparent proxy support. Requires --network-policy-file.",
)
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
    template: str | None,
    snapshot_id: str | None,
    request_file: str | None,
    timeout_raw: str | None,
    envs: tuple[tuple[str, str], ...],
    metadata_kv: tuple[tuple[str, str], ...],
    extensions_kv: tuple[tuple[str, str], ...],
    resources_kv: tuple[tuple[str, str], ...],
    entrypoint: tuple[str, ...],
    network_policy_file: str | None,
    credential_proxy: bool,
    volumes_file: str | None,
    skip_health_check: bool,
    ready_timeout: timedelta | None,
    output_format: str | None,
) -> None:
    """Create a new sandbox from an image, a template, a snapshot, or a request file."""
    from opensandbox.sync.sandbox import SandboxSync

    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")

    if request_file is not None:
        file_conflicts: list[str] = []
        if image is not None:
            file_conflicts.append("--image")
        if image_auth_username or image_auth_password:
            file_conflicts.append("--image-auth-username/--image-auth-password")
        if template:
            file_conflicts.append("--template")
        if snapshot_id:
            file_conflicts.append("--snapshot-id")
        if timeout_raw is not None:
            file_conflicts.append("--timeout")
        if envs:
            file_conflicts.append("--env")
        if metadata_kv:
            file_conflicts.append("--metadata")
        if extensions_kv:
            file_conflicts.append("--extension")
        if resources_kv:
            file_conflicts.append("--resource")
        if entrypoint:
            file_conflicts.append("--entrypoint")
        if network_policy_file:
            file_conflicts.append("--network-policy-file")
        if credential_proxy:
            file_conflicts.append("--credential-proxy")
        if volumes_file:
            file_conflicts.append("--volumes-file")
        if file_conflicts:
            raise click.ClickException(
                f"--file cannot be combined with: {', '.join(file_conflicts)}."
            )
        file_req = _parse_sandbox_request_file(request_file)
        if (
            file_req["image"] is None
            and file_req["template"] is None
            and file_req["snapshot_id"] is None
        ):
            raise click.ClickException(
                f"Request file '{request_file}' must set one of 'image', 'templateId', or 'snapshotId'."
            )
        image_spec: SandboxImageSpec | str | None = file_req["image"]
        template = file_req["template"]
        snapshot_id = file_req["snapshot_id"]
        timeout = file_req["timeout"]
        timeout_is_set = file_req["timeout_is_set"]
        env = file_req["env"]
        metadata = file_req["metadata"]
        extensions = file_req["extensions"]
        resource = file_req["resource"]
        resource_requests = file_req["resource_requests"]
        entrypoint_argv = file_req["entrypoint"]
        platform = file_req["platform"]
        network_policy = file_req["network_policy"]
        credential_proxy_config = file_req["credential_proxy"]
        volumes = file_req["volumes"]
        secure_access = file_req["secure_access"]
        lifecycle = file_req["lifecycle"]
    else:
        if template and snapshot_id:
            raise click.ClickException("--template and --snapshot-id are mutually exclusive.")

        if template:
            conflicts: list[str] = []
            if image is not None:
                conflicts.append("--image")
            if image_auth_username or image_auth_password:
                conflicts.append("--image-auth-username/--image-auth-password")
            if envs:
                conflicts.append("--env")
            if resources_kv:
                conflicts.append("--resource")
            if entrypoint:
                conflicts.append("--entrypoint")
            if volumes_file:
                conflicts.append("--volumes-file")
            if credential_proxy:
                conflicts.append("--credential-proxy")
            if conflicts:
                raise click.ClickException(
                    "Template mode fixes the workload shape on the server; "
                    f"{', '.join(conflicts)} cannot be combined with --template."
                )
        elif snapshot_id:
            if image is not None:
                raise click.ClickException("--snapshot-id and --image are mutually exclusive.")
            if image_auth_username or image_auth_password:
                raise click.ClickException(
                    "--snapshot-id cannot be combined with image auth options."
                )
        else:
            if image is None:
                image = obj.resolved_config.get("default_image")
            if not image:
                raise click.ClickException(
                    "Sandbox image is required. Pass --image, use --file, or set defaults.image in the CLI config."
                )

        if bool(image_auth_username) != bool(image_auth_password):
            raise click.ClickException(
                "Pass both --image-auth-username and --image-auth-password together."
            )
        if credential_proxy and not network_policy_file:
            raise click.ClickException(
                "--credential-proxy requires --network-policy-file because Credential Vault injection needs egress policy."
            )

        timeout_is_set = False
        if timeout_raw is not None:
            timeout = parse_nullable_duration(timeout_raw)
            timeout_is_set = True
        else:
            timeout = None
            default_timeout = obj.resolved_config.get("default_timeout")
            if default_timeout:
                timeout = parse_nullable_duration(default_timeout)
                timeout_is_set = True

        image_spec = image
        if image_auth_username and image_auth_password:
            image_spec = SandboxImageSpec(
                image=image,
                auth=SandboxImageAuth(
                    username=image_auth_username,
                    password=image_auth_password,
                ),
            )
        env = dict(envs) if envs else None
        metadata = dict(metadata_kv) if metadata_kv else None
        extensions = dict(extensions_kv) if extensions_kv else None
        resource = dict(resources_kv) if resources_kv else None
        resource_requests = None
        entrypoint_argv = list(entrypoint) if entrypoint else None
        platform = None
        network_policy = (
            _load_network_policy(network_policy_file) if network_policy_file else None
        )
        credential_proxy_config = (
            CredentialProxyConfig(enabled=True) if credential_proxy else None
        )
        volumes = None
        if volumes_file:
            with open(volumes_file) as f:
                raw_volumes = json.load(f)
            if not isinstance(raw_volumes, list):
                raise click.ClickException(
                    f"Volumes file must contain a JSON array, got {type(raw_volumes).__name__}."
                )
            volumes = [Volume(**item) for item in raw_volumes]
        secure_access = False
        lifecycle = None

    if template and not timeout_is_set:
        raise click.ClickException(
            "--timeout is required when creating a sandbox from a template (e.g. --timeout 30m)."
        )

    with obj.output.spinner("Creating sandbox..."):
        if template:
            if timeout is None:
                raise click.ClickException(
                    "--timeout none (manual cleanup) is not supported in template mode."
                )
            template_kwargs: dict = {}
            if ready_timeout is not None:
                template_kwargs["ready_timeout"] = ready_timeout
            if metadata is not None:
                template_kwargs["metadata"] = metadata
            if extensions is not None:
                template_kwargs["extensions"] = extensions
            if network_policy is not None:
                template_kwargs["network_policy"] = network_policy
            sandbox = SandboxSync.create_from_template(
                template,
                timeout=timeout,
                connection_config=obj.connection_config,
                skip_health_check=skip_health_check,
                **template_kwargs,
            )
        else:
            kwargs: dict = {
                "connection_config": obj.connection_config,
                "skip_health_check": skip_health_check,
            }
            if snapshot_id:
                kwargs["snapshot_id"] = snapshot_id
            if timeout_is_set:
                kwargs["timeout"] = timeout
            if ready_timeout is not None:
                kwargs["ready_timeout"] = ready_timeout
            if env is not None:
                kwargs["env"] = env
            if metadata is not None:
                kwargs["metadata"] = metadata
            if extensions is not None:
                kwargs["extensions"] = extensions
            if resource is not None:
                kwargs["resource"] = resource
            if resource_requests is not None:
                kwargs["resource_requests"] = resource_requests
            if entrypoint_argv is not None:
                kwargs["entrypoint"] = entrypoint_argv
            if platform is not None:
                kwargs["platform"] = platform
            if network_policy is not None:
                kwargs["network_policy"] = network_policy
            if credential_proxy_config is not None:
                kwargs["credential_proxy"] = credential_proxy_config
            if volumes is not None:
                kwargs["volumes"] = volumes
            if secure_access:
                kwargs["secure_access"] = True
            if lifecycle is not None:
                kwargs["lifecycle"] = lifecycle

            sandbox = SandboxSync.create(image_spec, **kwargs)

    details: dict[str, Any] = {
        "id": sandbox.id,
        "status": "created",
        "timeout": _describe_create_timeout(timeout_is_set, timeout),
    }
    if request_file is not None:
        details["request_file"] = request_file
    if template:
        details["template"] = template
    elif snapshot_id:
        details["snapshot_id"] = snapshot_id
    else:
        details["image"] = (
            image_spec.image if isinstance(image_spec, SandboxImageSpec) else image_spec
        )
    obj.output.success_panel(
        details,
        title="Sandbox Created",
    )


def _load_network_policy(path: str) -> NetworkPolicy:
    with open(path) as f:
        return NetworkPolicy(**json.load(f))


_SANDBOX_REQUEST_FILE_FIELDS = (
    "image",
    "templateId",
    "snapshotId",
    "platform",
    "timeout",
    "resourceLimits",
    "resourceRequests",
    "env",
    "metadata",
    "lifecycle",
    "entrypoint",
    "networkPolicy",
    "credentialProxy",
    "secureAccess",
    "volumes",
    "extensions",
)

# Fields rejected alongside templateId: template mode fixes the workload shape
# on the server (metadata, extensions, networkPolicy and timeout stay allowed).
_TEMPLATE_MODE_FORBIDDEN_FILE_FIELDS = (
    "resourceLimits",
    "resourceRequests",
    "env",
    "entrypoint",
    "volumes",
    "platform",
    "credentialProxy",
    "secureAccess",
    "lifecycle",
)


def _file_str_dict(data: dict, key: str, path: str) -> dict[str, str] | None:
    """Extract a string-to-string object field from a request file."""
    value = data.get(key)
    if value is None:
        return None
    if not isinstance(value, dict) or not all(
        isinstance(k, str) and isinstance(v, str) for k, v in value.items()
    ):
        raise click.ClickException(
            f"Request file field '{key}' in '{path}' must be an object with string values."
        )
    return dict(value)


def _file_model(data: dict, key: str, path: str, model_type: type[BaseModel]) -> Any:
    """Extract a model field from a request file, wrapping validation errors."""
    value = data.get(key)
    if value is None:
        return None
    if not isinstance(value, dict):
        raise click.ClickException(
            f"Request file field '{key}' in '{path}' must be an object."
        )
    try:
        return model_type.model_validate(value)
    except ValidationError as exc:
        raise click.ClickException(
            f"Invalid '{key}' in request file '{path}': {validation_message(exc)}"
        ) from exc


def _file_image_spec(value: object, path: str) -> SandboxImageSpec | str:
    """Parse the request file 'image' field (string or {uri, auth} object)."""
    if isinstance(value, str):
        if not value.strip():
            raise click.ClickException(
                f"Request file field 'image' in '{path}' must not be blank."
            )
        return value
    if isinstance(value, dict):
        uri = value.get("uri")
        if not isinstance(uri, str) or not uri.strip():
            raise click.ClickException(
                f"Request file field 'image' in '{path}' must be a string or an object "
                "with a non-empty 'uri'."
            )
        auth = value.get("auth")
        if auth is None:
            return uri
        if not isinstance(auth, dict):
            raise click.ClickException(
                f"Request file field 'image.auth' in '{path}' must be an object."
            )
        try:
            return SandboxImageSpec(image=uri, auth=SandboxImageAuth(**auth))
        except ValidationError as exc:
            raise click.ClickException(
                f"Invalid 'image.auth' in request file '{path}': {validation_message(exc)}"
            ) from exc
    raise click.ClickException(
        f"Request file field 'image' in '{path}' must be a string or an object with 'uri'."
    )


def _parse_sandbox_request_file(path: str) -> dict[str, Any]:
    """Parse a CreateSandboxRequest wire-format JSON file into create variables."""
    data = load_json_object(path)

    unknown = sorted(set(data) - set(_SANDBOX_REQUEST_FILE_FIELDS))
    if unknown:
        raise click.ClickException(
            f"Request file '{path}' has unsupported fields: {', '.join(unknown)}. "
            f"Supported fields: {', '.join(_SANDBOX_REQUEST_FILE_FIELDS)}."
        )

    image = data.get("image")
    template_id = data.get("templateId")
    snapshot_id = data.get("snapshotId")
    sources = [
        name
        for name, value in (
            ("image", image),
            ("templateId", template_id),
            ("snapshotId", snapshot_id),
        )
        if value is not None
    ]
    if len(sources) > 1:
        raise click.ClickException(
            f"Request file '{path}' must set only one of 'image', 'templateId', "
            f"'snapshotId'; got {', '.join(sources)}."
        )

    req: dict[str, Any] = {
        "image": None,
        "template": None,
        "snapshot_id": None,
        "timeout": None,
        "timeout_is_set": False,
        "env": None,
        "metadata": None,
        "extensions": None,
        "resource": None,
        "resource_requests": None,
        "entrypoint": None,
        "platform": None,
        "network_policy": None,
        "credential_proxy": None,
        "volumes": None,
        "secure_access": False,
        "lifecycle": None,
    }

    if image is not None:
        req["image"] = _file_image_spec(image, path)

    if template_id is not None:
        if not isinstance(template_id, str) or not template_id.strip():
            raise click.ClickException(
                f"Request file field 'templateId' in '{path}' must be a non-empty string."
            )
        req["template"] = template_id

    if snapshot_id is not None:
        if not isinstance(snapshot_id, str) or not snapshot_id.strip():
            raise click.ClickException(
                f"Request file field 'snapshotId' in '{path}' must be a non-empty string."
            )
        req["snapshot_id"] = snapshot_id

    if "timeout" in data:
        raw_timeout = data["timeout"]
        if raw_timeout is None:
            req["timeout"] = None
            req["timeout_is_set"] = True
        elif isinstance(raw_timeout, int) and not isinstance(raw_timeout, bool):
            if raw_timeout <= 0:
                raise click.ClickException(
                    f"Request file field 'timeout' in '{path}' must be a positive integer "
                    "of seconds, or null for manual cleanup."
                )
            req["timeout"] = timedelta(seconds=raw_timeout)
            req["timeout_is_set"] = True
        else:
            raise click.ClickException(
                f"Request file field 'timeout' in '{path}' must be a positive integer of "
                "seconds, or null for manual cleanup."
            )

    if req["template"] is not None:
        fixed_fields = [
            f"'{name}'"
            for name in _TEMPLATE_MODE_FORBIDDEN_FILE_FIELDS
            if data.get(name) is not None
        ]
        if fixed_fields:
            raise click.ClickException(
                "Template mode fixes the workload shape on the server; "
                f"request file cannot combine 'templateId' with {', '.join(fixed_fields)}."
            )

    req["env"] = _file_str_dict(data, "env", path)
    req["metadata"] = _file_str_dict(data, "metadata", path)
    req["extensions"] = _file_str_dict(data, "extensions", path)
    req["resource"] = _file_str_dict(data, "resourceLimits", path)
    req["resource_requests"] = _file_str_dict(data, "resourceRequests", path)

    entrypoint = data.get("entrypoint")
    if entrypoint is not None:
        if (
            not isinstance(entrypoint, list)
            or not entrypoint
            or not all(isinstance(item, str) for item in entrypoint)
        ):
            raise click.ClickException(
                f"Request file field 'entrypoint' in '{path}' must be a non-empty array "
                "of strings."
            )
        req["entrypoint"] = list(entrypoint)

    secure_access = data.get("secureAccess")
    if secure_access is not None:
        if not isinstance(secure_access, bool):
            raise click.ClickException(
                f"Request file field 'secureAccess' in '{path}' must be a boolean."
            )
        req["secure_access"] = secure_access

    req["platform"] = _file_model(data, "platform", path, PlatformSpec)
    req["network_policy"] = _file_model(data, "networkPolicy", path, NetworkPolicy)
    req["lifecycle"] = _file_model(data, "lifecycle", path, SandboxLifecycle)

    credential_proxy = data.get("credentialProxy")
    if credential_proxy is not None:
        if isinstance(credential_proxy, bool):
            req["credential_proxy"] = CredentialProxyConfig(enabled=credential_proxy)
        elif isinstance(credential_proxy, dict):
            try:
                req["credential_proxy"] = CredentialProxyConfig.model_validate(
                    credential_proxy
                )
            except ValidationError as exc:
                raise click.ClickException(
                    f"Invalid 'credentialProxy' in request file '{path}': "
                    f"{validation_message(exc)}"
                ) from exc
        else:
            raise click.ClickException(
                f"Request file field 'credentialProxy' in '{path}' must be a boolean "
                "or an object."
            )

    if (
        req["credential_proxy"] is not None
        and req["credential_proxy"].enabled
        and req["network_policy"] is None
    ):
        raise click.ClickException(
            f"Request file field 'credentialProxy' in '{path}' requires 'networkPolicy' "
            "because Credential Vault injection needs egress policy."
        )

    volumes = data.get("volumes")
    if volumes is not None:
        if not isinstance(volumes, list):
            raise click.ClickException(
                f"Request file field 'volumes' in '{path}' must be an array."
            )
        parsed_volumes: list[Volume] = []
        for item in volumes:
            if not isinstance(item, dict):
                raise click.ClickException(
                    f"Request file field 'volumes' in '{path}' must contain volume objects."
                )
            try:
                parsed_volumes.append(Volume.model_validate(item))
            except ValidationError as exc:
                raise click.ClickException(
                    f"Invalid 'volumes' entry in request file '{path}': "
                    f"{validation_message(exc)}"
                ) from exc
        req["volumes"] = parsed_volumes

    return req


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

    if obj.output.fmt in ("json", "yaml"):
        obj.output.print_dict(
            {
                "items": raw_rows,
                "pagination": result.pagination.model_dump(mode="json"),
            },
            title="Sandboxes",
        )
        return

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


@sandbox_group.command("get")
@click.argument("sandbox_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def sandbox_get(obj: ClientContext, sandbox_id: str, output_format: str | None) -> None:
    """Get sandbox details."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    mgr = obj.get_manager()
    info = mgr.get_sandbox_info(sandbox_id)
    d = info.model_dump(mode="json")

    if obj.output.fmt in ("json", "yaml"):
        obj.output.print_dict(d, title="Sandbox Info")
        return

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
        with obj.output.spinner(f"Killing sandbox {sid}..."):
            mgr.kill_sandbox(sid)
        rows.append({"sandbox_id": sid, "status": "terminated"})
    obj.output.print_rows(rows, columns=["sandbox_id", "status"], title="Sandboxes")


@sandbox_group.command("pause")
@click.argument("sandbox_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def sandbox_pause(obj: ClientContext, sandbox_id: str, output_format: str | None) -> None:
    """Pause a running sandbox."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    mgr = obj.get_manager()
    with obj.output.spinner("Pausing sandbox..."):
        mgr.pause_sandbox(sandbox_id)
    obj.output.success(f"Pause request accepted: {sandbox_id}")


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
    mgr = obj.get_manager()
    with obj.output.spinner("Renewing sandbox..."):
        resp = mgr.renew_sandbox(sandbox_id, timeout)
    obj.output.success_panel(
        {"sandbox_id": sandbox_id, "expires_at": str(resp.expires_at)},
        title="Sandbox Renewed",
    )


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
    if isinstance(decoded, dict) and "cpu_used_pct" in decoded:
        return MetricsModelConverter.to_sandbox_metrics(Metrics.from_dict(decoded)), None
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

    obj.output.print_model(metric)
