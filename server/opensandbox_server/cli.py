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

from __future__ import annotations

import argparse
import os
import shutil
import types
from importlib import resources
from pathlib import Path
from typing import Any, FrozenSet, Union, get_args, get_origin

import uvicorn
from pydantic import BaseModel

from opensandbox_server.config import (
    AgentSandboxRuntimeConfig,
    CONFIG_ENV_VAR,
    DEFAULT_CONFIG_PATH,
    DockerConfig,
    EgressConfig,
    IngressConfig,
    KubernetesRuntimeConfig,
    RenewIntentConfig,
    RuntimeConfig,
    ServerConfig,
    StorageConfig,
    load_config,
)
from opensandbox_server.logging_config import configure_logging


def _strip_optional(annotation: Any) -> Any:
    """Unwrap Optional / Union[..., None] to the inner type."""
    if annotation is None:
        return None
    origin = get_origin(annotation)
    args = get_args(annotation)
    if origin is Union or origin is types.UnionType:
        filtered = [a for a in args if a is not type(None)]
        if len(filtered) == 1:
            return filtered[0]
    return annotation


def _is_basemodel_type(annotation: Any) -> bool:
    inner = _strip_optional(annotation)
    return isinstance(inner, type) and issubclass(inner, BaseModel)

EXAMPLE_FILE_MAP = {
    "docker": "example.config.toml",
    "docker-zh": "example.config.zh.toml",
    "k8s": "example.config.k8s.toml",
    "k8s-zh": "example.config.k8s.zh.toml",
}


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Run the OpenSandbox server.",
        formatter_class=argparse.RawTextHelpFormatter,
    )
    parser.add_argument(
        "--config",
        help="Path to the server config TOML file (overrides SANDBOX_CONFIG_PATH).",
    )
    parser.add_argument(
        "--reload",
        action="store_true",
        help="Enable auto-reload (development only).",
    )

    subparsers = parser.add_subparsers(dest="command")

    init_parser = subparsers.add_parser(
        "init-config",
        help="Generate a config file from packaged examples or the schema skeleton.",
    )
    init_parser.add_argument(
        "path",
        nargs="?",
        default=str(DEFAULT_CONFIG_PATH),
        help="Destination path for the config file (default: ~/.sandbox.toml).",
    )
    init_parser.add_argument(
        "--example",
        choices=sorted(EXAMPLE_FILE_MAP),
        help=(
            "Packaged example to copy (docker, docker-zh, k8s, k8s-zh). "
            "Omit to render the full skeleton with placeholders."
        ),
    )
    init_parser.add_argument(
        "--force",
        action="store_true",
        help="Overwrite existing file when generating config.",
    )

    parser.epilog = (
        "Subcommands:\n"
        "  init-config [path] [--example {docker,docker-zh,k8s,k8s-zh}] [--force]\n"
        "    Generate a config file. Without --example it renders the full skeleton (placeholders only).\n"
        "    --example    Copy a packaged example config.\n"
        "    --force      Overwrite destination if it exists.\n"
    )
    return parser


def copy_example_config(
    destination: str | Path | None = None, *, force: bool = False, kind: str = "default"
) -> Path:
    """Copy a packaged example config template to the target path."""
    if kind not in EXAMPLE_FILE_MAP:
        supported = ", ".join(EXAMPLE_FILE_MAP)
        raise ValueError(f"Unsupported example kind '{kind}'. Choices: {supported}")

    filename = EXAMPLE_FILE_MAP[kind]
    dest_path = Path(destination or DEFAULT_CONFIG_PATH).expanduser()
    dest_path.parent.mkdir(parents=True, exist_ok=True)
    if dest_path.exists() and not force:
        raise FileExistsError(f"Config file already exists at {dest_path}. Use --force to overwrite.")

    example_resource = resources.files("opensandbox_server.examples").joinpath(filename)
    if not example_resource.is_file():
        raise FileNotFoundError(f"Missing packaged example config template: {filename}")

    with resources.as_file(example_resource) as src_path:
        shutil.copyfile(src_path, dest_path)
    return dest_path


def render_full_config(destination: str | Path | None = None, *, force: bool = False) -> Path:
    """
    Render the most complete config skeleton from config models with comments.

    No defaults are prefilled; everything is emitted as placeholders so users
    must explicitly set values. Field comments come from pydantic Field
    descriptions to stay in sync with the schema.
    """

    def _placeholder_for_field(field) -> str:
        """Return a placeholder TOML value that is intentionally empty."""
        ann = field.annotation
        if ann is not None:
            origin = getattr(ann, "__origin__", None)
            if ann is list or origin is list:
                return "[]"
        return '""'  # string placeholder for scalars/bool/int; user must replace

    def _render_section(
        section: str,
        model,
        *,
        placeholders: dict[str, str] | None = None,
        extra_comments: list[str] | None = None,
        dotted_nested: FrozenSet[str] | None = None,
    ) -> str:
        lines: list[str] = []
        if extra_comments:
            lines.extend([f"# {c}" for c in extra_comments])
        lines.append(f"[{section}]")

        placeholders = placeholders or {}
        dotted_nested = dotted_nested or frozenset()

        for field_name, field in model.model_fields.items():
            if _is_basemodel_type(field.annotation):
                continue
            key = field.alias or field_name
            value = placeholders.get(key, _placeholder_for_field(field))
            if field.description:
                lines.append(f"# {field.description}")
            lines.append(f"{key} = {value}")
            lines.append("")

        for field_name, field in model.model_fields.items():
            if field_name not in dotted_nested or not _is_basemodel_type(field.annotation):
                continue
            inner = _strip_optional(field.annotation)
            if not isinstance(inner, type) or not issubclass(inner, BaseModel):
                continue
            for sub_name, sub_field in inner.model_fields.items():
                sub_key = f"{field_name}.{sub_name}"
                value = placeholders.get(sub_key, _placeholder_for_field(sub_field))
                if sub_field.description:
                    lines.append(f"# {sub_field.description}")
                lines.append(f"{sub_key} = {value}")
                lines.append("")

        nested_blocks: list[str] = []
        for field_name, field in model.model_fields.items():
            if not _is_basemodel_type(field.annotation):
                continue
            if field_name in dotted_nested:
                continue
            inner = _strip_optional(field.annotation)
            if not isinstance(inner, type) or not issubclass(inner, BaseModel):
                continue
            nested_path = f"{section}.{field_name}"
            nested_blocks.append(
                _render_section(nested_path, inner, placeholders=None, extra_comments=None)
            )

        if nested_blocks:
            if lines and lines[-1] == "":
                lines.pop()
            lines.append("")
            lines.extend(nested_blocks)

        if lines and lines[-1] == "":
            lines.pop()
        return "\n".join(lines)

    dest_path = Path(destination or DEFAULT_CONFIG_PATH).expanduser()
    dest_path.parent.mkdir(parents=True, exist_ok=True)
    if dest_path.exists() and not force:
        raise FileExistsError(f"Config file already exists at {dest_path}. Use --force to overwrite.")

    sections = [
        "# Generated from OpenSandbox config schema. Remove sections you do not use.",
        _render_section("server", ServerConfig),
        _render_section(
            "renew_intent",
            RenewIntentConfig,
            extra_comments=[
                "Renew-intent: top-level section (not under [server]). "
                "Redis options use dotted keys in this table (redis.enabled, redis.queue_key, …)."
            ],
            dotted_nested=frozenset({"redis"}),
        ),
        _render_section("runtime", RuntimeConfig),
        _render_section("docker", DockerConfig),
        _render_section(
            "egress",
            EgressConfig,
            extra_comments=["Used when networkPolicy is provided. Requires docker.network_mode = \"bridge\"."],
        ),
        _render_section(
            "kubernetes",
            KubernetesRuntimeConfig,
            extra_comments=["Only used when runtime.type = \"kubernetes\""],
        ),
        _render_section(
            "agent_sandbox",
            AgentSandboxRuntimeConfig,
            extra_comments=["Requires kubernetes.workload_provider = \"agent-sandbox\""],
        ),
        _render_section("ingress", IngressConfig),
        _render_section("storage", StorageConfig),
    ]

    content = "\n\n".join(sections) + "\n"
    dest_path.write_text(content, encoding="utf-8")
    return dest_path


def main() -> None:
    parser = _build_parser()
    args = parser.parse_args()

    if args.command == "init-config":
        try:
            if args.example:
                dest = copy_example_config(args.path, force=args.force, kind=args.example)
                print(f"Wrote example config ({args.example}) to {dest}\n")
            else:
                dest = render_full_config(args.path, force=args.force)
                print(f"Wrote full config skeleton to {dest}\n")
        except Exception as exc:  # noqa: BLE001
            print(f"Failed to write config template: {exc}\n")
            raise SystemExit(1)
        return

    if args.config:
        os.environ[CONFIG_ENV_VAR] = args.config

    # Load config + logging without importing opensandbox_server.main: importing
    # main eagerly constructs sandbox_service (restoring containers and starting
    # expiration timers), which we defer to the actual worker process so the
    # uvicorn reloader supervisor does not run them.
    app_config = load_config()
    log_config = configure_logging(app_config.log)
    server_cfg = app_config.server

    uvicorn.run(
        "opensandbox_server.main:app",
        host=server_cfg.host,
        port=server_cfg.port,
        reload=args.reload,
        log_config=log_config,
        timeout_keep_alive=server_cfg.timeout_keep_alive,
        limit_concurrency=server_cfg.limit_concurrency,
        backlog=server_cfg.backlog,
        loop=server_cfg.loop,
        http=server_cfg.http,
    )


if __name__ == "__main__":
    main()
