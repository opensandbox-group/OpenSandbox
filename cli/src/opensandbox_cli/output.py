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

"""Output formatting: table (rich), JSON, YAML."""

from __future__ import annotations

import json
import sys
from collections.abc import Generator, Sequence
from contextlib import contextmanager
from typing import Any

import click

try:
    import yaml
except ImportError:  # pragma: no cover
    yaml = None  # type: ignore[assignment]

from pydantic import BaseModel
from rich import box
from rich.console import Console
from rich.panel import Panel
from rich.status import Status
from rich.table import Table
from rich.text import Text

# ---------------------------------------------------------------------------
# Status badge styling  (sandbox state → color + icon)
# ---------------------------------------------------------------------------

_STATUS_STYLES: dict[str, tuple[str, str]] = {
    # state → (rich style, icon)
    "running": ("bold green", "●"),
    "ready": ("bold green", "●"),
    "healthy": ("bold green", "●"),
    "pending": ("bold yellow", "◐"),
    "creating": ("bold yellow", "◐"),
    "starting": ("bold yellow", "◐"),
    "paused": ("bold blue", "⏸"),
    "stopped": ("dim", "○"),
    "terminated": ("dim", "○"),
    "killed": ("dim", "○"),
    "error": ("bold red", "✗"),
    "failed": ("bold red", "✗"),
    "unhealthy": ("bold red", "✗"),
    "created": ("bold cyan", "✦"),
}

# Columns that contain status-like values
_STATUS_COLUMNS = {"status", "state", "healthy"}

# Columns that should be rendered in a dimmer style (long IDs, timestamps)
_DIM_COLUMNS = {"created_at", "expires_at", "modified_at", "updated_at"}

# Columns that are primary identifiers
_ID_COLUMNS = {"id", "sandbox_id", "execution_id", "context_id"}


def _style_value(col: str, value: str) -> Text:
    """Apply contextual styling to a cell value."""
    lower = value.lower()

    if col in _STATUS_COLUMNS:
        style, icon = _STATUS_STYLES.get(lower, ("", ""))
        if style:
            return Text(f"{icon} {value}", style=style)

    if col in _DIM_COLUMNS:
        return Text(value, style="dim")

    if col in _ID_COLUMNS:
        return Text(value, style="bold cyan")

    return Text(value)


class OutputFormatter:
    """Renders data in table / json / yaml format."""

    def __init__(self, fmt: str = "table", *, color: bool = True) -> None:
        self.fmt = fmt
        self.color = color
        self.console = Console(
            stderr=False, no_color=not color, force_terminal=None
        )
        self._err_console = Console(
            stderr=True, no_color=not color, force_terminal=None
        )

    # ------------------------------------------------------------------
    # Status messages with icons
    # ------------------------------------------------------------------

    def success(self, msg: str) -> None:
        """Print a success message with ✅ icon."""
        if self.fmt == "json":
            self._print_json({"status": "ok", "message": msg})
            return
        if self.fmt == "yaml":
            self._print_yaml({"status": "ok", "message": msg})
            return
        if self.color:
            self.console.print(f"  [bold green]✅ {msg}[/]")
        else:
            click.echo(f"OK: {msg}")

    def info(self, msg: str) -> None:
        """Print an info message with ℹ️  icon."""
        if self.fmt == "json":
            self._print_json({"status": "info", "message": msg})
            return
        if self.fmt == "yaml":
            self._print_yaml({"status": "info", "message": msg})
            return
        if self.color:
            self.console.print(f"  [bold blue]ℹ️  {msg}[/]")
        else:
            click.echo(f"INFO: {msg}")

    def warning(self, msg: str) -> None:
        """Print a warning message with ⚠️  icon."""
        if self.fmt == "json":
            self._print_json({"status": "warning", "message": msg})
            return
        if self.fmt == "yaml":
            self._print_yaml({"status": "warning", "message": msg})
            return
        if self.color:
            self._err_console.print(f"  [bold yellow]⚠️  {msg}[/]")
        else:
            click.echo(f"WARN: {msg}", err=True)

    def error(self, msg: str) -> None:
        """Print an error message with ❌ icon."""
        if self.fmt == "json":
            self._print_json({"status": "error", "message": msg})
            return
        if self.fmt == "yaml":
            self._print_yaml({"status": "error", "message": msg})
            return
        if self.color:
            self._err_console.print(f"  [bold red]❌ {msg}[/]")
        else:
            click.echo(f"ERROR: {msg}", err=True)

    def error_panel(self, msg: str, title: str = "Error") -> None:
        """Print an error with a bold header and message."""
        if self.color:
            self._err_console.print()
            self._err_console.print(f"  [bold red]{title}[/]")
            self._err_console.print(f"  [dim]{'─' * (len(title) + 2)}[/]")
            for line in msg.splitlines():
                self._err_console.print(f"  {line}")
            self._err_console.print()
        else:
            click.echo(f"ERROR [{title}]: {msg}", err=True)

    # ------------------------------------------------------------------
    # Spinner for long-running operations
    # ------------------------------------------------------------------

    @contextmanager
    def spinner(self, msg: str) -> Generator[Status, None, None]:
        """Context manager that shows a spinner while work is in progress."""
        if self.color and self.fmt == "table":
            with self._err_console.status(f"[bold cyan]⏳ {msg}[/]", spinner="dots") as status:
                yield status
        else:
            # No spinner in non-color or non-table mode
            yield None  # type: ignore[arg-type]

    # ------------------------------------------------------------------
    # Panel output
    # ------------------------------------------------------------------

    def panel(self, content: str, *, title: str | None = None, style: str = "cyan") -> None:
        """Print content inside a styled panel."""
        if self.color:
            self.console.print(Panel(
                content,
                title=title,
                title_align="left",
                border_style=style,
                box=box.ROUNDED,
                padding=(0, 1),
            ))
        else:
            if title:
                click.echo(f"--- {title} ---")
            click.echo(content)

    def success_panel(self, data: dict[str, Any], *, title: str = "Success") -> None:
        """Print a success result with a header and indented key-value pairs."""
        if self.fmt != "table":
            if self.fmt == "json":
                self._print_json(data)
            elif self.fmt == "yaml":
                self._print_yaml(data)
            return

        if self.color:
            self.console.print()
            self.console.print(f"  [bold green]✓ {title}[/]")
            self.console.print(f"  [dim]{'─' * (len(title) + 2)}[/]")
            for k, v in data.items():
                self.console.print(f"  [bold]{k}:[/] [cyan]{v}[/]")
            self.console.print()
        else:
            click.echo(f"--- {title} ---")
            for k, v in data.items():
                click.echo(f"  {k}: {v}")

    # ------------------------------------------------------------------
    # Public helpers
    # ------------------------------------------------------------------

    def print_model(self, model: BaseModel, title: str | None = None) -> None:
        """Print a single Pydantic model as key-value panel or JSON/YAML."""
        data = _model_to_dict(model)
        if self.fmt == "json":
            self._print_json(data)
        elif self.fmt == "yaml":
            self._print_yaml(data)
        else:
            self._print_kv_table(data, title=title)

    def print_models(
        self,
        models: Sequence[BaseModel],
        columns: list[str],
        *,
        title: str | None = None,
    ) -> None:
        """Print a list of Pydantic models as a table or JSON/YAML."""
        rows = [_model_to_dict(m) for m in models]
        if self.fmt == "json":
            self._print_json(rows)
        elif self.fmt == "yaml":
            self._print_yaml(rows)
        else:
            self._print_table(rows, columns, title=title)

    def print_rows(
        self,
        rows: list[dict[str, Any]],
        columns: list[str],
        *,
        title: str | None = None,
    ) -> None:
        """Print pre-processed rows (list of dicts) as a table or JSON/YAML."""
        if self.fmt == "json":
            self._print_json(rows)
        elif self.fmt == "yaml":
            self._print_yaml(rows)
        else:
            self._print_table(rows, columns, title=title)

    def print_dict(self, data: dict[str, Any], title: str | None = None) -> None:
        """Print a flat dict."""
        if self.fmt == "json":
            self._print_json(data)
        elif self.fmt == "yaml":
            self._print_yaml(data)
        else:
            self._print_kv_table(data, title=title)

    def print_text(self, text: str) -> None:
        """Print raw text (ignores format)."""
        click.echo(text)

    # ------------------------------------------------------------------
    # Internal renderers
    # ------------------------------------------------------------------

    def _print_json(self, data: Any) -> None:
        if self.color:
            self.console.print_json(json.dumps(data, default=str))
        else:
            click.echo(json.dumps(data, indent=2, default=str))

    def _print_yaml(self, data: Any) -> None:
        if yaml is None:
            click.secho(
                "PyYAML is not installed. Use --output json instead.", fg="red", err=True
            )
            sys.exit(1)
        click.echo(yaml.dump(data, default_flow_style=False, allow_unicode=True).rstrip())

    def _print_kv_table(self, data: dict[str, Any], *, title: str | None = None) -> None:
        table = Table(
            title=title,
            show_header=True,
            header_style="bold magenta",
            title_style="bold cyan",
            box=box.ROUNDED,
            border_style="bright_black",
            padding=(0, 1),
            show_lines=True,
        )
        table.add_column("Key", style="bold cyan", no_wrap=True)
        table.add_column("Value")
        for k, v in data.items():
            val_text = _style_value(k, str(v)) if v is not None else Text("-", style="dim")
            table.add_row(str(k), val_text)
        self.console.print(table)

    def _print_table(
        self,
        rows: list[dict[str, Any]],
        columns: list[str],
        *,
        title: str | None = None,
    ) -> None:
        table = Table(
            title=title,
            show_header=True,
            header_style="bold magenta",
            title_style="bold cyan",
            box=box.ROUNDED,
            border_style="bright_black",
            padding=(0, 1),
            row_styles=["", "dim"],
        )
        for col in columns:
            style = ""
            if col in _ID_COLUMNS:
                style = "bold cyan"
            elif col in _DIM_COLUMNS:
                style = "dim"
            table.add_column(col.upper(), style=style, no_wrap=(col in _ID_COLUMNS))

        for row in rows:
            cells: list[Text | str] = []
            for col in columns:
                val = str(row.get(col, "-"))
                if col in _STATUS_COLUMNS:
                    cells.append(_style_value(col, val))
                else:
                    cells.append(val)
            table.add_row(*cells)
        self.console.print(table)


# ------------------------------------------------------------------
# Helpers
# ------------------------------------------------------------------


def _model_to_dict(model: BaseModel) -> dict[str, Any]:
    return model.model_dump(mode="json")
