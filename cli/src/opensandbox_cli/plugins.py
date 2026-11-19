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

"""Load third-party Click commands from Python entry points."""

from __future__ import annotations

from collections.abc import Iterable
from importlib import metadata
from typing import Protocol

import click

PLUGIN_ENTRY_POINT_GROUP = "opensandbox.cli.plugins"


class EntryPointLike(Protocol):
    """Minimal entry-point interface shared with ``importlib.metadata``."""

    @property
    def name(self) -> str: ...

    def load(self) -> object: ...


def _installed_entry_points() -> Iterable[EntryPointLike]:
    """Return entry points declared in the CLI plugin group."""
    return metadata.entry_points(group=PLUGIN_ENTRY_POINT_GROUP)


def _warn(message: str) -> None:
    """Print a single-line warning to stderr."""
    click.echo(f"Warning: {message}", err=True)


def _available_entry_points(
    entry_points: Iterable[EntryPointLike] | None,
) -> list[EntryPointLike]:
    """Return the entry points to load, or none when discovery fails.

    Reading installed entry points parses the metadata of every installed
    distribution before filtering by group, so one unrelated package with
    malformed metadata can raise here. Discovery failure must not stop the
    built-in commands from running.
    """
    try:
        return list(
            entry_points if entry_points is not None else _installed_entry_points()
        )
    except Exception as exc:  # discovery is third-party metadata, not our code
        detail = " ".join(str(exc).split()) or type(exc).__name__
        _warn(f"skipping plugin discovery: failed to read entry points ({detail})")
        return []


def load_plugin_commands(
    cli: click.Group,
    entry_points: Iterable[EntryPointLike] | None = None,
) -> list[str]:
    """Register plugin commands and groups on the root ``cli`` group.

    Each entry point in ``entry_points`` (by default, the installed
    ``opensandbox.cli.plugins`` group) must load a ``click.Command`` or
    ``click.Group``. The entry-point name becomes the command name. A plugin is
    skipped with a warning when its name is already taken by a built-in
    command, when loading raises, or when the loaded object is not a Click
    command or group. A failing entry-point discovery is reported once and the
    CLI keeps its built-in commands only. Returns the names that were
    registered.
    """
    registered: list[str] = []
    for entry_point in _available_entry_points(entry_points):
        name = entry_point.name
        if name in cli.commands:
            _warn(f"skipping plugin '{name}': command name already registered")
            continue
        try:
            command = entry_point.load()
        except (Exception, SystemExit) as exc:
            # Plugin imports may raise anything, including SystemExit from a
            # dependency check. User interrupts are not plugin failures.
            text = " ".join(str(exc).split())
            detail = f"{type(exc).__name__}: {text}" if text else type(exc).__name__
            _warn(f"skipping plugin '{name}': failed to load ({detail})")
            continue
        if not isinstance(command, click.Command):
            _warn(
                f"skipping plugin '{name}': loaded object is not a click command or group"
            )
            continue
        cli.add_command(command, name=name)
        registered.append(name)
    return registered
