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

"""Smoke tests for the execd MCP proxy bridge using the official python SDK.

Runs against both python SDK release lines (1.x and 2.x) — mcp_smoke.sh
invokes this script twice with opposite version pins; the API differences
(entrypoint name, header mechanism) are adapted below.

Covers, against a running execd:
  1. /mcp/{name}    — HTTP upstream bridge (Streamable HTTP passthrough)
  2. /mcp/{name}    — stdio upstream bridge (per-session subprocess)
  3. /proxy/{port}  — legacy reverse proxy path to an MCP server (phase 0)
  4. access token   — requests without the token are rejected (401)

Env:
  BASE_URL         execd base URL (default http://localhost:44772)
  API_TOKEN        execd access token (optional)
  HTTP_UPSTREAM_PORT  port of the FastMCP upstream (default 46001)
"""

import contextlib
import json
import os
import sys

import httpx
from mcp import ClientSession

try:  # SDK 2.x: renamed entrypoint, headers via a custom http client.
    from mcp.client.streamable_http import streamable_http_client as streamable_client

    SDK_V2 = True
    import httpx2
except ImportError:  # SDK 1.x.
    from mcp.client.streamable_http import streamablehttp_client as streamable_client

    SDK_V2 = False

BASE_URL = os.environ.get("BASE_URL", "http://localhost:44772").rstrip("/")
API_TOKEN = os.environ.get("API_TOKEN", "")
HTTP_UPSTREAM_PORT = os.environ.get("HTTP_UPSTREAM_PORT", "46001")

HEADERS = {"X-EXECD-ACCESS-TOKEN": API_TOKEN} if API_TOKEN else {}


def expect(cond, msg):
    if not cond:
        raise SystemExit(f"FAIL: {msg}")
    print(f"ok: {msg}")


@contextlib.asynccontextmanager
async def open_client(url, headers):
    """Yield (read, write) streams for the installed python SDK line."""
    if SDK_V2:
        async with streamable_client(
            url, http_client=httpx2.AsyncClient(headers=headers)
        ) as streams:
            yield streams[0], streams[1]
    else:
        async with streamable_client(url, headers=headers) as (read, write, _):
            yield read, write


async def drive(url, *, tool_name, tool_args, expect_text, client_headers=None):
    headers = dict(HEADERS)
    if client_headers:
        headers.update(client_headers)
    async with open_client(url, headers) as (read, write):
        async with ClientSession(read, write) as session:
            init = await session.initialize()
            # SDK 1.x uses camelCase schema fields; 2.x uses snake_case.
            server_name = getattr(init, "server_info", None) or getattr(init, "serverInfo", None)
            expect(server_name, f"{url}: initialize returned server info {server_name}")
            tools = await session.list_tools()
            expect(any(t.name == tool_name for t in tools.tools), f"{url}: tools/list contains {tool_name}")
            result = await session.call_tool(tool_name, tool_args)
            expect(
                result.content and result.content[0].text == expect_text,
                f"{url}: call_tool({tool_name}) == {expect_text!r}",
            )


async def main():
    from importlib.metadata import version as pkg_version

    sdk_line = "2.x" if SDK_V2 else "1.x"
    try:
        sdk_version = pkg_version("mcp")
    except Exception:
        sdk_version = "unknown"
    print(f"python mcp SDK line: {sdk_line} (mcp {sdk_version})")

    # 1. HTTP bridge: /mcp/{name} → FastMCP upstream.
    await drive(
        f"{BASE_URL}/mcp/smoke-http",
        tool_name="add",
        tool_args={"a": 2, "b": 3},
        expect_text="5",
    )

    # 2. stdio bridge: /mcp/{name} → execd-managed subprocess.
    await drive(
        f"{BASE_URL}/mcp/smoke-stdio",
        tool_name="echo",
        tool_args={"message": "through-stdio"},
        expect_text="through-stdio",
    )

    # 3. Existing /proxy passthrough to the same HTTP upstream (phase 0 path).
    await drive(
        f"{BASE_URL}/proxy/{HTTP_UPSTREAM_PORT}/mcp",
        tool_name="add",
        tool_args={"a": 20, "b": 22},
        expect_text="42",
    )

    # 4. Auth: the same MCP handshake without the token must fail.
    if API_TOKEN:
        try:
            async with open_client(f"{BASE_URL}/mcp/smoke-http", {}) as (read, write):
                async with ClientSession(read, write) as session:
                    await session.initialize()
            raise SystemExit("FAIL: unauthenticated MCP initialize unexpectedly succeeded")
        except SystemExit:
            raise
        except Exception:
            print("ok: unauthenticated MCP initialize rejected")

    # 5. Bridge index lists both servers.
    resp = httpx.get(f"{BASE_URL}/mcp", headers=HEADERS, timeout=10)
    expect(resp.status_code == 200, "/mcp index returns 200")
    names = {s["name"] for s in resp.json()}
    expect({"smoke-http", "smoke-stdio"} <= names, f"/mcp index lists bridges: {sorted(names)}")

    # 6. Unknown bridge name is a clean 404.
    resp = httpx.post(
        f"{BASE_URL}/mcp/nope",
        content=json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/list"}),
        headers={**HEADERS, "Content-Type": "application/json"},
        timeout=10,
    )
    expect(resp.status_code == 404, "unknown bridge name returns 404")

    print(f"ALL MCP SMOKE TESTS PASSED (python SDK {sdk_line})")


if __name__ == "__main__":
    import anyio

    anyio.run(main)
