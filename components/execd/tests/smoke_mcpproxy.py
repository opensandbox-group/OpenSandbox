#!/usr/bin/env python3

# Copyright 2025 Alibaba Group Holding Ltd.
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

"""
Smoke tests for the execd MCP proxy (/mcpproxy).

Prerequisites:
- execd server running locally (default http://localhost:44772)
- python3 available (used for the mock MCP stdio server)
- Optional: set env BASE_URL to override
- Optional: set env API_TOKEN if server expects X-EXECD-ACCESS-TOKEN
"""

import os
import sys
import tempfile
import textwrap

import requests

BASE_URL = os.environ.get("BASE_URL", "http://localhost:44772").rstrip("/")
API_TOKEN = os.environ.get("API_TOKEN")

HEADERS = {}
if API_TOKEN:
    HEADERS["X-EXECD-ACCESS-TOKEN"] = API_TOKEN

session = requests.Session()
session.headers.update(HEADERS)

# Path to the mock MCP server script (created at runtime).
_mock_script = None


def expect(cond: bool, msg: str):
    if not cond:
        raise SystemExit(msg)


def create_mock_mcp_server() -> str:
    """Write a minimal stdio MCP server script and return its path."""
    global _mock_script
    script = textwrap.dedent("""\
        #!/usr/bin/env python3
        import json, sys

        TOOLS = [
            {
                "name": "echo_tool",
                "description": "Echoes the input message back.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "message": {"type": "string", "description": "Message to echo"}
                    },
                    "required": ["message"]
                }
            },
            {
                "name": "add_numbers",
                "description": "Adds two numbers.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "a": {"type": "number"},
                        "b": {"type": "number"}
                    },
                    "required": ["a", "b"]
                }
            }
        ]

        def respond(id, result):
            msg = {"jsonrpc": "2.0", "id": id, "result": result}
            sys.stdout.write(json.dumps(msg) + "\\n")
            sys.stdout.flush()

        def error(id, code, message):
            msg = {"jsonrpc": "2.0", "id": id, "error": {"code": code, "message": message}}
            sys.stdout.write(json.dumps(msg) + "\\n")
            sys.stdout.flush()

        for line in sys.stdin:
            line = line.strip()
            if not line:
                continue
            try:
                req = json.loads(line)
            except json.JSONDecodeError:
                continue

            method = req.get("method", "")
            rid = req.get("id")

            if rid is None:
                continue

            if method == "initialize":
                respond(rid, {
                    "protocolVersion": "2025-03-26",
                    "capabilities": {"tools": {"listChanged": True}},
                    "serverInfo": {"name": "mock-mcp", "version": "1.0.0"}
                })
            elif method == "tools/list":
                respond(rid, {"tools": TOOLS})
            elif method == "tools/call":
                params = req.get("params", {})
                name = params.get("name", "")
                args = params.get("arguments", {})
                if name == "echo_tool":
                    respond(rid, {
                        "content": [{"type": "text", "text": args.get("message", "")}],
                        "isError": False
                    })
                elif name == "add_numbers":
                    result = args.get("a", 0) + args.get("b", 0)
                    respond(rid, {
                        "content": [{"type": "text", "text": str(result)}],
                        "isError": False
                    })
                else:
                    error(rid, -32601, f"unknown tool: {name}")
            elif method == "ping":
                respond(rid, {})
            else:
                error(rid, -32601, f"unknown method: {method}")
    """)
    fd, path = tempfile.mkstemp(suffix=".py", prefix="mock_mcp_")
    with os.fdopen(fd, "w") as f:
        f.write(script)
    os.chmod(path, 0o755)
    _mock_script = path
    return path


def cleanup_mock():
    if _mock_script and os.path.exists(_mock_script):
        os.unlink(_mock_script)


# ── JSON-RPC helpers ───────────────────────────────────────────────

def jsonrpc_request(method: str, params=None, req_id=None):
    msg = {"jsonrpc": "2.0", "method": method}
    if req_id is not None:
        msg["id"] = req_id
    if params is not None:
        msg["params"] = params
    return msg


def mcp_post(body: dict, session_id: str = None) -> requests.Response:
    headers = {"Content-Type": "application/json", "Accept": "application/json"}
    if session_id:
        headers["Mcp-Session-Id"] = session_id
    return session.post(f"{BASE_URL}/mcpproxy", json=body, headers=headers, timeout=15)


def mcp_delete(session_id: str) -> requests.Response:
    headers = {"Mcp-Session-Id": session_id}
    return session.delete(f"{BASE_URL}/mcpproxy", headers=headers, timeout=5)


# ── Test functions ─────────────────────────────────────────────────

def test_register_upstream(mock_path: str) -> dict:
    """Register the mock MCP server as a stdio upstream."""
    payload = {
        "name": "mock",
        "transport": "stdio",
        "command": sys.executable,
        "args": [mock_path],
    }
    r = session.post(f"{BASE_URL}/mcpproxy/upstreams", json=payload, timeout=30)
    expect(r.status_code == 201, f"register upstream failed: {r.status_code} {r.text}")
    info = r.json()
    expect(info["name"] == "mock", f"upstream name mismatch: {info}")
    expect(info["status"] == "running", f"upstream not running: {info}")
    expect(len(info["tools"]) == 2, f"expected 2 tools, got {info['tools']}")
    return info


def test_list_upstreams():
    """List registered upstreams."""
    r = session.get(f"{BASE_URL}/mcpproxy/upstreams", timeout=5)
    expect(r.status_code == 200, f"list upstreams failed: {r.status_code} {r.text}")
    upstreams = r.json()
    expect(len(upstreams) >= 1, "no upstreams found")
    names = [u["name"] for u in upstreams]
    expect("mock" in names, f"mock upstream not found in {names}")


def test_get_upstream():
    """Get detail of a specific upstream."""
    r = session.get(f"{BASE_URL}/mcpproxy/upstreams/mock", timeout=5)
    expect(r.status_code == 200, f"get upstream failed: {r.status_code} {r.text}")
    info = r.json()
    expect(info["name"] == "mock", f"name mismatch: {info}")


def test_initialize() -> str:
    """Initialize an MCP session and return the session ID."""
    body = jsonrpc_request("initialize", {
        "protocolVersion": "2025-03-26",
        "capabilities": {},
        "clientInfo": {"name": "smoke-test", "version": "1.0.0"},
    }, req_id=1)
    r = mcp_post(body)
    expect(r.status_code == 200, f"initialize failed: {r.status_code} {r.text}")

    session_id = r.headers.get("Mcp-Session-Id")
    expect(bool(session_id), "missing Mcp-Session-Id in response")

    resp = r.json()
    expect(resp.get("result", {}).get("protocolVersion") == "2025-03-26",
           f"protocol version mismatch: {resp}")
    return session_id


def test_initialized_notification(session_id: str):
    """Send the initialized notification (no response expected)."""
    body = jsonrpc_request("notifications/initialized")
    r = mcp_post(body, session_id=session_id)
    expect(r.status_code in (200, 202), f"initialized notification failed: {r.status_code} {r.text}")


def test_tools_list(session_id: str):
    """List tools — should return aggregated tools from all upstreams."""
    body = jsonrpc_request("tools/list", {}, req_id=2)
    r = mcp_post(body, session_id=session_id)
    expect(r.status_code == 200, f"tools/list failed: {r.status_code} {r.text}")

    resp = r.json()
    tools = resp.get("result", {}).get("tools", [])
    tool_names = [t["name"] for t in tools]
    expect("echo_tool" in tool_names, f"echo_tool not found in {tool_names}")
    expect("add_numbers" in tool_names, f"add_numbers not found in {tool_names}")


def test_tools_call_echo(session_id: str):
    """Call echo_tool and verify the response."""
    body = jsonrpc_request("tools/call", {
        "name": "echo_tool",
        "arguments": {"message": "hello from smoke test"},
    }, req_id=3)
    r = mcp_post(body, session_id=session_id)
    expect(r.status_code == 200, f"tools/call echo failed: {r.status_code} {r.text}")

    resp = r.json()
    content = resp.get("result", {}).get("content", [])
    expect(len(content) == 1, f"expected 1 content block, got {content}")
    expect(content[0]["text"] == "hello from smoke test",
           f"echo mismatch: {content[0]}")


def test_tools_call_add(session_id: str):
    """Call add_numbers and verify the result."""
    body = jsonrpc_request("tools/call", {
        "name": "add_numbers",
        "arguments": {"a": 17, "b": 25},
    }, req_id=4)
    r = mcp_post(body, session_id=session_id)
    expect(r.status_code == 200, f"tools/call add failed: {r.status_code} {r.text}")

    resp = r.json()
    content = resp.get("result", {}).get("content", [])
    expect(len(content) == 1, f"expected 1 content block, got {content}")
    expect(content[0]["text"] == "42", f"add result mismatch: {content[0]}")


def test_tools_call_unknown(session_id: str):
    """Call a nonexistent tool — should get an error."""
    body = jsonrpc_request("tools/call", {
        "name": "nonexistent_tool",
        "arguments": {},
    }, req_id=5)
    r = mcp_post(body, session_id=session_id)
    expect(r.status_code == 200, f"tools/call unknown HTTP failed: {r.status_code}")

    resp = r.json()
    expect(resp.get("error") is not None, f"expected error for unknown tool: {resp}")


def test_ping(session_id: str):
    """Send a ping and expect a pong."""
    body = jsonrpc_request("ping", {}, req_id=6)
    r = mcp_post(body, session_id=session_id)
    expect(r.status_code == 200, f"ping failed: {r.status_code} {r.text}")

    resp = r.json()
    expect(resp.get("error") is None, f"ping error: {resp}")


def test_missing_session():
    """Requests without session ID should fail (except initialize)."""
    body = jsonrpc_request("tools/list", {}, req_id=99)
    r = mcp_post(body)
    expect(r.status_code == 400, f"expected 400 without session: {r.status_code}")


def test_unknown_session():
    """Requests with a bogus session ID should fail."""
    body = jsonrpc_request("tools/list", {}, req_id=99)
    r = mcp_post(body, session_id="bogus-session-id")
    expect(r.status_code == 404, f"expected 404 for unknown session: {r.status_code}")


def test_delete_session(session_id: str):
    """Delete the MCP session."""
    r = mcp_delete(session_id)
    expect(r.status_code == 200, f"delete session failed: {r.status_code} {r.text}")

    # Subsequent requests with the deleted session should fail.
    body = jsonrpc_request("tools/list", {}, req_id=99)
    r2 = mcp_post(body, session_id=session_id)
    expect(r2.status_code == 404, f"expected 404 after session delete: {r2.status_code}")


def test_duplicate_upstream(mock_path: str):
    """Registering an upstream with the same name should fail."""
    payload = {
        "name": "mock",
        "transport": "stdio",
        "command": sys.executable,
        "args": [mock_path],
    }
    r = session.post(f"{BASE_URL}/mcpproxy/upstreams", json=payload, timeout=15)
    expect(r.status_code == 500, f"expected error for duplicate upstream: {r.status_code}")


def test_tool_conflict(mock_path: str):
    """Registering a second upstream with conflicting tools should fail."""
    payload = {
        "name": "mock-conflict",
        "transport": "stdio",
        "command": sys.executable,
        "args": [mock_path],
    }
    r = session.post(f"{BASE_URL}/mcpproxy/upstreams", json=payload, timeout=30)
    expect(r.status_code == 500, f"expected error for tool conflict: {r.status_code}")
    expect("conflicts" in r.text.lower() or "conflict" in r.text.lower(),
           f"error should mention conflict: {r.text}")


def test_remove_upstream():
    """Remove the mock upstream."""
    r = session.delete(f"{BASE_URL}/mcpproxy/upstreams/mock", timeout=10)
    expect(r.status_code == 200, f"remove upstream failed: {r.status_code} {r.text}")

    # Verify it's gone.
    r2 = session.get(f"{BASE_URL}/mcpproxy/upstreams/mock", timeout=5)
    expect(r2.status_code == 404, f"upstream should be gone: {r2.status_code}")


def test_tools_empty_after_remove():
    """After removing all upstreams, tools/list should return empty."""
    sid = test_initialize()
    body = jsonrpc_request("tools/list", {}, req_id=10)
    r = mcp_post(body, session_id=sid)
    expect(r.status_code == 200, f"tools/list failed: {r.status_code}")
    tools = r.json().get("result", {}).get("tools", [])
    expect(len(tools) == 0, f"expected 0 tools after remove, got {len(tools)}")
    mcp_delete(sid)


# ── Main ───────────────────────────────────────────────────────────

def main():
    print(f"[+] base: {BASE_URL}")

    # Health check
    r = session.get(f"{BASE_URL}/ping", timeout=5)
    expect(r.status_code == 200, "ping failed")
    print("[+] ping ok")

    # Create mock MCP server
    mock_path = create_mock_mcp_server()
    print(f"[+] mock MCP server: {mock_path}")

    try:
        # Upstream management
        info = test_register_upstream(mock_path)
        print(f"[+] upstream registered: {info['tools']}")

        test_list_upstreams()
        print("[+] list upstreams ok")

        test_get_upstream()
        print("[+] get upstream ok")

        test_duplicate_upstream(mock_path)
        print("[+] duplicate upstream rejected")

        test_tool_conflict(mock_path)
        print("[+] tool conflict rejected")

        # Session management
        test_missing_session()
        print("[+] missing session rejected")

        test_unknown_session()
        print("[+] unknown session rejected")

        # MCP protocol flow
        sid = test_initialize()
        print(f"[+] initialized, session: {sid}")

        test_initialized_notification(sid)
        print("[+] initialized notification ok")

        test_ping(sid)
        print("[+] ping/pong ok")

        test_tools_list(sid)
        print("[+] tools/list ok")

        test_tools_call_echo(sid)
        print("[+] tools/call echo_tool ok")

        test_tools_call_add(sid)
        print("[+] tools/call add_numbers ok (17+25=42)")

        test_tools_call_unknown(sid)
        print("[+] unknown tool error ok")

        test_delete_session(sid)
        print("[+] session delete ok")

        # Cleanup upstream
        test_remove_upstream()
        print("[+] upstream removed")

        test_tools_empty_after_remove()
        print("[+] tools empty after remove")

        print("[+] MCP proxy smoke tests PASS")

    finally:
        # Best-effort cleanup: remove upstream if still registered.
        try:
            session.delete(f"{BASE_URL}/mcpproxy/upstreams/mock", timeout=5)
        except Exception as exc:
            print(f"[!] cleanup: failed to remove upstream (ignored): {exc}", file=sys.stderr)
        cleanup_mock()


if __name__ == "__main__":
    try:
        main()
    except SystemExit as exc:
        print(f"[!] MCP proxy smoke tests FAIL: {exc}", file=sys.stderr)
        cleanup_mock()
        sys.exit(1)
