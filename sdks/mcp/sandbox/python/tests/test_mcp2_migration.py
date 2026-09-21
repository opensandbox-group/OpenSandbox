# Copyright 2026 The OpenSandbox Authors
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

"""Regression tests for the MCP 2.x migration (follow-up to the 1.x pin in #1725).

The server now targets the mcp 2.x API (`MCPServer` / 2-param `Context`) and the
dependency constraint is `mcp[cli]>=2,<3`. These tests pin both halves:
the distribution metadata keeps the constraint, and the public entry points
only exist on the 2.x API surface.
"""

from __future__ import annotations

import importlib.metadata
import importlib.util
import pathlib

import pytest


def _project_root() -> pathlib.Path:
    return pathlib.Path(__file__).resolve().parents[1]


def _read_requires_dist() -> list[str]:
    try:
        dist = importlib.metadata.distribution("opensandbox-mcp")
    except importlib.metadata.PackageNotFoundError:
        return _read_requires_dist_from_source()
    return dist.requires or []


def _read_requires_dist_from_source() -> list[str]:
    # Fallback when the package is not installed in the test environment:
    # ask uv to build nothing and instead derive Requires-Dist from pyproject.
    import tomllib

    pyproject = _project_root() / "pyproject.toml"
    data = tomllib.loads(pyproject.read_text())
    deps = data["project"]["dependencies"]
    return [f"{dep}" for dep in deps] if deps else []


class TestMcp2Constraint:
    def test_requires_dist_pins_mcp_2(self) -> None:
        requires = _read_requires_dist()
        mcp_entries = [r for r in requires if r.startswith("mcp[")]
        assert mcp_entries, f"no mcp requirement found in: {requires}"
        entry = mcp_entries[0]
        assert ">=2" in entry, entry
        assert "<3" in entry, entry

    def test_pyproject_declares_mcp_2(self) -> None:
        import tomllib

        pyproject = _project_root() / "pyproject.toml"
        data = tomllib.loads(pyproject.read_text())
        deps = data["project"]["dependencies"]
        mcp_deps = [d for d in deps if d.startswith("mcp[")]
        assert mcp_deps == ["mcp[cli]>=2,<3"]


class TestMcp2Imports:
    def test_server_module_imports(self) -> None:
        spec = importlib.util.find_spec("opensandbox_mcp.server")
        assert spec is not None
        import opensandbox_mcp.server as server_mod

        assert hasattr(server_mod, "create_server")

    def test_mcp2_api_surface(self) -> None:
        from mcp.server.mcpserver import Context

        # Context in mcp 2.x is generic over (LifespanContextT, RequestT)
        assert Context.__parameters__ is not None and len(Context.__parameters__) == 2

    def test_old_fastmcp_import_raises(self) -> None:
        # mcp 2.x keeps a stub that raises with a migration pointer
        with pytest.raises(ModuleNotFoundError):
            __import__("mcp.server.fastmcp")


class TestCreateServer:
    def test_create_server_returns_mcp2_server(self) -> None:
        from mcp.server.mcpserver import MCPServer  # noqa: I001

        from opensandbox_mcp.server import create_server

        server = create_server()
        assert isinstance(server, MCPServer)
