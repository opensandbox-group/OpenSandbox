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

from importlib.metadata import metadata


def test_published_metadata_pins_mcp_2() -> None:
    """Keep releases on the MCP 2 API now that the server has migrated."""
    requirements = metadata("opensandbox-mcp").get_all("Requires-Dist") or []
    normalized = {requirement.replace(" ", "") for requirement in requirements}

    mcp_entries = {r for r in normalized if r.startswith("mcp[")}
    assert mcp_entries, f"no mcp requirement found in: {sorted(normalized)}"
    entry = mcp_entries.pop()
    assert ">=2" in entry, entry
    assert "<3" in entry, entry
