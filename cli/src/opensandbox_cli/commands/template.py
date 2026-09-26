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

"""Template commands: create, get, list, delete."""

from __future__ import annotations

from typing import Literal, cast

import click
from opensandbox.models.templates import (
    CreateTemplateRequest,
    TemplateFilter,
    TemplateReadiness,
)
from pydantic import ValidationError

from opensandbox_cli.utils import (
    KEY_VALUE,
    handle_errors,
    load_json_object,
    output_option,
    prepare_output,
    validation_message,
)


@click.group("template", invoke_without_command=True)
@click.pass_context
def template_group(ctx: click.Context) -> None:
    """🏗 Manage sandbox templates (golden-image builds)."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


@template_group.command("create")
@click.option(
    "--image",
    "-i",
    default=None,
    help="Source OCI image reference the golden image is built from. Required unless --file is used.",
)
@click.option(
    "--publish",
    "-p",
    default=None,
    help="S3-compatible publish target for the built artifacts (e.g. s3://bucket/publish). Required unless --file is used.",
)
@click.option(
    "--file",
    "-f",
    "request_file",
    type=click.Path(exists=True, dir_okay=False),
    default=None,
    help=(
        "JSON request file in the public CreateTemplateRequest wire format "
        "(camelCase keys: image, publish, resourceLimits, entrypoint, env, metadata, "
        "format, readiness.probe, readiness.warmupSeconds). Mutually exclusive with "
        "request-building flags; -o still applies."
    ),
)
@click.option(
    "--resource",
    "resources_kv",
    multiple=True,
    type=KEY_VALUE,
    help="Guest machine sizing (e.g. cpu=1 memory=512Mi disk=2Gi). Repeatable.",
)
@click.option(
    "--entrypoint",
    "entrypoint",
    multiple=True,
    help="Guest business command argv item. Repeat to build the full entrypoint.",
)
@click.option(
    "--env",
    "-e",
    "envs",
    multiple=True,
    type=KEY_VALUE,
    help="Environment variable baked into the golden image (KEY=VALUE). Repeatable.",
)
@click.option("--metadata", "-m", "metadata_kv", multiple=True, type=KEY_VALUE, help="Metadata (KEY=VALUE). Repeatable.")
@click.option(
    "--format",
    "image_format",
    type=click.Choice(["native", "overlaybd"]),
    default=None,
    help="Storage encoding of the produced snapshots. Defaults to the server default (overlaybd).",
)
@click.option(
    "--readiness-probe",
    default=None,
    help="Build readiness probe checked first (e.g. tcp://127.0.0.1:44772 or cmd://<command>).",
)
@click.option(
    "--warmup-seconds",
    type=click.IntRange(min=0),
    default=None,
    help="Fallback warmup window in seconds used when no probe is set.",
)
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def template_create(
    obj,
    image: str | None,
    publish: str | None,
    resources_kv: tuple[tuple[str, str], ...],
    entrypoint: tuple[str, ...],
    envs: tuple[tuple[str, str], ...],
    metadata_kv: tuple[tuple[str, str], ...],
    image_format: str | None,
    readiness_probe: str | None,
    warmup_seconds: int | None,
    request_file: str | None,
    output_format: str | None,
) -> None:
    """Create a template and start its asynchronous golden-image build."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")

    if request_file is not None:
        file_conflicts: list[str] = []
        if image is not None:
            file_conflicts.append("--image")
        if publish is not None:
            file_conflicts.append("--publish")
        if resources_kv:
            file_conflicts.append("--resource")
        if entrypoint:
            file_conflicts.append("--entrypoint")
        if envs:
            file_conflicts.append("--env")
        if metadata_kv:
            file_conflicts.append("--metadata")
        if image_format is not None:
            file_conflicts.append("--format")
        if readiness_probe is not None:
            file_conflicts.append("--readiness-probe")
        if warmup_seconds is not None:
            file_conflicts.append("--warmup-seconds")
        if file_conflicts:
            raise click.ClickException(
                f"--file cannot be combined with: {', '.join(file_conflicts)}."
            )
        data = load_json_object(request_file)
        try:
            request = CreateTemplateRequest.model_validate(data)
        except ValidationError as exc:
            raise click.ClickException(
                f"Invalid request file '{request_file}': {validation_message(exc)}"
            ) from exc
    else:
        if image is None or publish is None:
            missing = [
                name
                for name, value in (("--image", image), ("--publish", publish))
                if not value
            ]
            raise click.ClickException(
                f"Missing required options: {', '.join(missing)} (or pass --file)."
            )
        readiness = None
        if readiness_probe is not None or warmup_seconds is not None:
            readiness = TemplateReadiness(probe=readiness_probe, warmupSeconds=warmup_seconds)

        request = CreateTemplateRequest(
            image=image,
            publish=publish,
            resourceLimits=dict(resources_kv) if resources_kv else None,
            entrypoint=list(entrypoint) if entrypoint else None,
            env=dict(envs) if envs else None,
            metadata=dict(metadata_kv) if metadata_kv else None,
            format=cast(Literal["native", "overlaybd"] | None, image_format),
            readiness=readiness,
        )
    mgr = obj.get_manager()

    with obj.output.spinner("Creating template..."):
        info = mgr.create_template(request)
    details = {
        "template_id": info.template_id,
        "image": info.image,
        "status": info.status.phase,
    }
    if request_file is not None:
        details["request_file"] = request_file
    obj.output.success_panel(
        details,
        title="Template Created",
    )


@template_group.command("get")
@click.argument("template_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def template_get(obj, template_id: str, output_format: str | None) -> None:
    """Get template details including its latest build status."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    mgr = obj.get_manager()
    with obj.output.spinner("Fetching template..."):
        info = mgr.get_template(template_id)
    d = info.model_dump(mode="json")

    if obj.output.fmt in ("json", "yaml"):
        obj.output.print_dict(d, title="Template Info")
        return

    status_val = d.get("status")
    if isinstance(status_val, dict):
        d["status"] = status_val.get("phase", str(status_val))
        if status_val.get("message"):
            d["status_message"] = status_val["message"]
    readiness_val = d.get("readiness")
    if isinstance(readiness_val, dict):
        parts = []
        if readiness_val.get("probe"):
            parts.append(readiness_val["probe"])
        if readiness_val.get("warmup_seconds") is not None:
            parts.append(f"warmup={readiness_val['warmup_seconds']}s")
        d["readiness"] = " ".join(parts) if parts else None
    obj.output.print_dict(d, title="Template Info")


@template_group.command("list")
@click.option("--metadata", "-m", "metadata_kv", multiple=True, type=KEY_VALUE, help="Metadata filter (KEY=VALUE). Repeatable.")
@click.option("--page", type=click.IntRange(min=1), default=None, help="Page number (1-indexed).")
@click.option("--page-size", type=click.IntRange(min=1), default=None, help="Items per page.")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def template_list(
    obj,
    metadata_kv: tuple[tuple[str, str], ...],
    page: int | None,
    page_size: int | None,
    output_format: str | None,
) -> None:
    """List templates."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    mgr = obj.get_manager()
    filt = TemplateFilter(
        metadata=dict(metadata_kv) if metadata_kv else None,
        page=page,
        pageSize=page_size,
    )
    with obj.output.spinner("Fetching templates..."):
        result = mgr.list_templates(filt)
    if not result.template_infos:
        if obj.output.fmt in ("json", "yaml"):
            obj.output.print_dict(
                {
                    "items": [],
                    "pagination": result.pagination.model_dump(mode="json"),
                },
                title="Templates",
            )
        else:
            obj.output.info("No templates found.")
        return

    raw_rows = [info.model_dump(mode="json") for info in result.template_infos]

    if obj.output.fmt in ("json", "yaml"):
        obj.output.print_dict(
            {
                "items": raw_rows,
                "pagination": result.pagination.model_dump(mode="json"),
            },
            title="Templates",
        )
        return

    rows = []
    for d in raw_rows:
        flat = dict(d)
        status_val = flat.get("status")
        if isinstance(status_val, dict):
            flat["status"] = status_val.get("phase", str(status_val))
        rows.append(flat)

    obj.output.print_rows(
        rows,
        columns=["template_id", "status", "image", "format", "created_at", "updated_at"],
        title="Templates",
    )


@template_group.command("delete")
@click.argument("template_id")
@output_option("table", "json", "yaml")
@click.pass_obj
@handle_errors
def template_delete(obj, template_id: str, output_format: str | None) -> None:
    """Delete a template by ID. Running sandboxes are unaffected."""
    prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
    mgr = obj.get_manager()
    with obj.output.spinner(f"Deleting template {template_id}..."):
        mgr.delete_template(template_id)
    obj.output.success(f"Template deleted: {template_id}")
