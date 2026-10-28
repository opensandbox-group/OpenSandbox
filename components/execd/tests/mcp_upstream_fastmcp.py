#!/usr/bin/env python3

# Copyright 2025 The OpenSandbox Authors
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

"""HTTP MCP upstream for the execd MCP proxy smoke tests.

Serves a Streamable HTTP MCP server (FastMCP) with one "add" tool, and
echoes the Authorization header into a tool so header behavior can be
asserted end to end.
"""

import os

from mcp.server.fastmcp import FastMCP

mcp = FastMCP("smoke-upstream", host="127.0.0.1", port=int(os.environ.get("MCP_UPSTREAM_PORT", "46001")))


@mcp.tool()
def add(a: int, b: int) -> int:
    """Add two integers."""
    return a + b


if __name__ == "__main__":
    mcp.run(transport="streamable-http")
