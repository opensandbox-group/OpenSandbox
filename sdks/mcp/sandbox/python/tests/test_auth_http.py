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

"""End-to-end Streamable HTTP OAuth 2.1 security, protocol, and isolation tests."""

from __future__ import annotations

import asyncio
import json
import logging
import time
from dataclasses import replace
from typing import Any

import httpx
import jwt
import pytest
from cryptography.hazmat.primitives.asymmetric import rsa
from jwt.algorithms import RSAAlgorithm
from mcp_types.version import LATEST_HANDSHAKE_VERSION
from opensandbox.config import ConnectionConfig
from opensandbox.models.execd import Execution

from opensandbox_mcp.auth import (
    MAX_AUTHORIZATION_HEADER_BYTES,
    JWTTokenVerifier,
    OAuthConfig,
    OAuthConfigError,
)
from opensandbox_mcp.server import create_server, create_streamable_http_app

ISSUER = "https://idp.example.com"
RESOURCE = "https://mcp.example.com/mcp"
JWKS_URL = "https://idp.example.com/.well-known/jwks.json"
REQUIRED_SCOPE = "opensandbox:mcp"
BACKEND_API_KEY = "test-backend-api-key-only"


class _SpyBackend:
    """Tracks every backend SDK call to prove rejected requests have zero side effects."""

    def __init__(self) -> None:
        self.killed: list[tuple[str, str | None]] = []
        self.commands_run: list[tuple[str, str]] = []
        self.files_written: list[tuple[str, str, str]] = []


class _FakeCommands:
    def __init__(self, spy: _SpyBackend, sandbox_id: str) -> None:
        self._spy = spy
        self._sandbox_id = sandbox_id

    async def run(self, command: str, *, opts=None) -> Execution:
        self._spy.commands_run.append((self._sandbox_id, command))
        return Execution(id="exec-1")


class _FakeFiles:
    def __init__(self, spy: _SpyBackend, sandbox_id: str) -> None:
        self._spy = spy
        self._sandbox_id = sandbox_id

    async def write_file(self, path: str, content: str, **_kwargs: Any) -> None:
        self._spy.files_written.append((self._sandbox_id, path, content))


class _FakeSandbox:
    def __init__(self, spy: _SpyBackend, sandbox_id: str) -> None:
        self.id = sandbox_id
        self.commands = _FakeCommands(spy, sandbox_id)
        self.files = _FakeFiles(spy, sandbox_id)


class _TestOAuthEnv:
    def __init__(self, spy: _SpyBackend) -> None:
        self.spy = spy
        self.private_key = rsa.generate_private_key(
            public_exponent=65537, key_size=2048
        )
        self.jwk = json.loads(RSAAlgorithm.to_jwk(self.private_key.public_key()))
        self.jwk["kid"] = "test-kid-1"
        self.jwk["use"] = "sig"
        self.jwk["alg"] = "RS256"

        self.config = OAuthConfig(
            issuer_url=ISSUER,
            resource_url=RESOURCE,
            jwks_url=JWKS_URL,
            required_scopes=(REQUIRED_SCOPE,),
            clock_skew_seconds=0.0,
        )
        jwks_client = httpx.AsyncClient(
            transport=httpx.MockTransport(
                lambda _req: httpx.Response(200, json={"keys": [self.jwk]})
            )
        )
        self.verifier = JWTTokenVerifier(self.config, http_client=jwks_client)
        self.connection_config = ConnectionConfig(
            api_key=BACKEND_API_KEY,
            domain="sandbox.internal:8080",
            protocol="http",
        )
        self.mcp = create_server(
            connection_config=self.connection_config,
            oauth_config=self.config,
            token_verifier=self.verifier,
        )
        self.app = create_streamable_http_app(
            self.mcp,
            host="127.0.0.1",
            oauth_config=self.config,
        )

    def mint_token(
        self,
        *,
        scope: str = REQUIRED_SCOPE,
        sub: str = "user-alice",
        client_id: str = "chatgpt",
        aud: str = RESOURCE,
        iss: str = ISSUER,
        exp_offset: int = 300,
    ) -> str:
        now = int(time.time())
        return jwt.encode(
            {
                "iss": iss,
                "aud": aud,
                "sub": sub,
                "client_id": client_id,
                "scope": scope,
                "iat": now - 5,
                "exp": now + exp_offset,
            },
            self.private_key,
            algorithm="RS256",
            headers={"kid": "test-kid-1", "typ": "at+jwt"},
        )


def _init_payload(req_id: int = 1) -> dict[str, Any]:
    return {
        "jsonrpc": "2.0",
        "id": req_id,
        "method": "initialize",
        "params": {
            "protocolVersion": LATEST_HANDSHAKE_VERSION,
            "capabilities": {},
            "clientInfo": {"name": "test-oauth-client", "version": "1.0.0"},
        },
    }


def _mcp_headers(
    token: str | None = None, *, session_id: str | None = None
) -> dict[str, str]:
    headers = {
        "Host": "mcp.example.com",
        "Accept": "application/json, text/event-stream",
        "Content-Type": "application/json",
    }
    if token is not None:
        headers["Authorization"] = f"Bearer {token}"
    if session_id is not None:
        headers["Mcp-Session-Id"] = session_id
    return headers


def _parse_mcp_response_body(resp: httpx.Response) -> dict[str, Any]:
    content_type = resp.headers.get("content-type", "")
    if "application/json" in content_type:
        return resp.json()
    for line in resp.text.splitlines():
        if line.startswith("data: "):
            return json.loads(line[6:])
    raise AssertionError(f"Unable to parse MCP response body: {resp.text!r}")


@pytest.fixture
def oauth_env(monkeypatch: pytest.MonkeyPatch) -> _TestOAuthEnv:
    spy = _SpyBackend()

    class _FakeManager:
        def __init__(self, config: ConnectionConfig) -> None:
            self._config = config

        @classmethod
        async def create(cls, connection_config: ConnectionConfig):
            return cls(connection_config)

        async def kill_sandbox(self, sandbox_id: str) -> None:
            spy.killed.append((sandbox_id, self._config.api_key))

        async def close(self) -> None:
            return None

    class _FakeSandboxModule:
        @staticmethod
        async def connect(sandbox_id: str, connection_config=None, **_kwargs: Any):
            return _FakeSandbox(spy, sandbox_id)

    monkeypatch.setattr("opensandbox_mcp.server.SandboxManager", _FakeManager)
    monkeypatch.setattr("opensandbox_mcp.server.Sandbox", _FakeSandboxModule)
    return _TestOAuthEnv(spy)


@pytest.mark.parametrize(
    ("failure", "error"),
    [
        ("", ""),
        ("network", "Unable to discover"),
        ("issuer", "issuer does not match"),
        ("algorithm", "no signing keys for the allowed algorithms"),
    ],
)
async def test_oauth_startup_checks_discovery_and_signing_keys(
    oauth_env: _TestOAuthEnv,
    failure: str,
    error: str,
) -> None:
    requests: list[str] = []
    discovery_url = f"{ISSUER}/.well-known/oauth-authorization-server"

    def provider(request: httpx.Request) -> httpx.Response:
        url = str(request.url)
        requests.append(url)
        if failure == "network":
            raise httpx.ConnectError("mock network error", request=request)
        if url == discovery_url:
            issuer = "https://other.example.com" if failure == "issuer" else ISSUER
            return httpx.Response(200, json={"issuer": issuer, "jwks_uri": JWKS_URL})
        assert url == JWKS_URL
        return httpx.Response(200, json={"keys": [oauth_env.jwk]})

    config = replace(
        oauth_env.config,
        jwks_url=None,
        allowed_algorithms=("ES256",) if failure == "algorithm" else ("RS256",),
    )
    async with httpx.AsyncClient(
        transport=httpx.MockTransport(provider)
    ) as http_client:
        verifier = JWTTokenVerifier(config, http_client=http_client)
        mcp = create_server(oauth_config=config, token_verifier=verifier)
        app = create_streamable_http_app(mcp, oauth_config=config)

        if failure:
            with pytest.raises(OAuthConfigError, match=error):
                async with app.router.lifespan_context(app):
                    pytest.fail("An invalid provider reached readiness")
        else:
            async with app.router.lifespan_context(app):
                assert requests == [discovery_url, JWKS_URL]
                assert await verifier.verify_token(oauth_env.mint_token()) is not None
                assert requests == [discovery_url, JWKS_URL]


async def _run_raw_asgi(
    app: Any,
    *,
    method: str,
    path: str,
    headers: list[tuple[bytes, bytes]],
    query_string: bytes = b"",
    body: bytes = b"",
) -> tuple[int, dict[str, str], bytes]:
    status_code = 0
    resp_headers: dict[str, str] = {}
    resp_body = bytearray()
    body_sent = False

    async def receive() -> dict[str, Any]:
        nonlocal body_sent
        if not body_sent:
            body_sent = True
            return {"type": "http.request", "body": body, "more_body": False}
        return {"type": "http.disconnect"}

    async def send(message: dict[str, Any]) -> None:
        nonlocal status_code
        if message["type"] == "http.response.start":
            status_code = message["status"]
            resp_headers.update(
                {
                    k.decode("latin-1").lower(): v.decode("latin-1")
                    for k, v in message["headers"]
                }
            )
        elif message["type"] == "http.response.body":
            resp_body.extend(message.get("body", b""))

    scope = {
        "type": "http",
        "asgi": {"version": "3.0"},
        "http_version": "1.1",
        "method": method,
        "scheme": "http",
        "path": path,
        "raw_path": path.encode("ascii"),
        "query_string": query_string,
        "headers": headers,
        "client": ("127.0.0.1", 54321),
        "server": ("127.0.0.1", 8000),
    }
    await app(scope, receive, send)
    return status_code, resp_headers, bytes(resp_body)


async def test_protected_resource_metadata_discovery_and_proxy_invariance(
    oauth_env: _TestOAuthEnv,
) -> None:
    async with oauth_env.app.router.lifespan_context(oauth_env.app):
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=oauth_env.app),
            base_url="http://127.0.0.1:8000",
        ) as client:
            for path in (
                "/.well-known/oauth-protected-resource/mcp",
                "/.well-known/oauth-protected-resource",
            ):
                # Even with spoofed forwarded headers, metadata stays canonical
                resp = await client.get(
                    path,
                    headers={
                        "Host": "mcp.example.com",
                        "X-Forwarded-Host": "attacker.example.com",
                        "X-Forwarded-Proto": "http",
                        "Forwarded": "host=attacker.example.com;proto=http",
                    },
                )
                assert resp.status_code == 200
                data = resp.json()
                assert data["resource"] == RESOURCE
                assert data["authorization_servers"] == [ISSUER]
                assert data["scopes_supported"] == [REQUIRED_SCOPE]
                assert data["bearer_methods_supported"] == ["header"]

                # CORS preflight succeeds anonymously
                opt = await client.options(
                    path,
                    headers={
                        "Host": "mcp.example.com",
                        "Origin": "https://chatgpt.com",
                        "Access-Control-Request-Method": "GET",
                    },
                )
                assert opt.status_code == 200


async def test_authentication_failures_and_zero_backend_side_effects(
    oauth_env: _TestOAuthEnv,
) -> None:
    valid_token = oauth_env.mint_token()
    wrong_aud = oauth_env.mint_token(aud="https://other.example.com/mcp")
    expired = oauth_env.mint_token(exp_offset=-60)
    expected_meta = "https://mcp.example.com/.well-known/oauth-protected-resource/mcp"

    kill_rpc = {
        "jsonrpc": "2.0",
        "id": 99,
        "method": "tools/call",
        "params": {"name": "sandbox_kill", "arguments": {"sandbox_id": "sbx-prod"}},
    }
    cmd_rpc = {
        "jsonrpc": "2.0",
        "id": 100,
        "method": "tools/call",
        "params": {
            "name": "command_run",
            "arguments": {"sandbox_id": "sbx-prod", "command": "rm -rf /"},
        },
    }
    file_rpc = {
        "jsonrpc": "2.0",
        "id": 101,
        "method": "tools/call",
        "params": {
            "name": "file_write",
            "arguments": {
                "sandbox_id": "sbx-prod",
                "path": "/etc/pwned",
                "content": "evil",
            },
        },
    }

    async with oauth_env.app.router.lifespan_context(oauth_env.app):
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=oauth_env.app),
            base_url="http://127.0.0.1:8000",
        ) as client:
            # Missing Authorization header
            r_missing = await client.post("/mcp", headers=_mcp_headers(), json=kill_rpc)
            assert r_missing.status_code == 401
            assert (
                f'resource_metadata="{expected_meta}"'
                in r_missing.headers["www-authenticate"]
            )

            # Unsupported scheme (Basic)
            h_basic = _mcp_headers()
            h_basic["Authorization"] = "Basic dXNlcjpwYXNz"
            r_basic = await client.post("/mcp", headers=h_basic, json=cmd_rpc)
            assert r_basic.status_code == 401

            # Empty bearer token
            h_empty = _mcp_headers()
            h_empty["Authorization"] = "Bearer "
            r_empty = await client.post("/mcp", headers=h_empty, json=file_rpc)
            assert r_empty.status_code == 401

            # Malformed bearer token with embedded whitespace
            h_space = _mcp_headers()
            h_space["Authorization"] = f"Bearer {valid_token} extra"
            assert (
                await client.post("/mcp", headers=h_space, json=kill_rpc)
            ).status_code == 401

            # Oversized Authorization header
            h_big = _mcp_headers()
            h_big["Authorization"] = "Bearer " + (
                "A" * (MAX_AUTHORIZATION_HEADER_BYTES + 100)
            )
            assert (
                await client.post("/mcp", headers=h_big, json=kill_rpc)
            ).status_code == 401

            # Query-parameter token MUST be rejected even if valid token also in header
            r_query = await client.post(
                f"/mcp?access_token={valid_token}",
                headers=_mcp_headers(valid_token),
                json=kill_rpc,
            )
            assert r_query.status_code == 401
            assert "query parameters" in r_query.json()["error_description"]

            # Body-parameter token without Authorization header -> 401
            r_body = await client.post(
                "/mcp",
                headers=_mcp_headers(),
                json={"access_token": valid_token, **kill_rpc},
            )
            assert r_body.status_code == 401

            # Wrong audience & expired tokens -> 401
            assert (
                await client.post(
                    "/mcp", headers=_mcp_headers(wrong_aud), json=kill_rpc
                )
            ).status_code == 401
            assert (
                await client.post("/mcp", headers=_mcp_headers(expired), json=cmd_rpc)
            ).status_code == 401

            # GET (SSE) and DELETE without token -> 401
            assert (
                await client.get("/mcp", headers={"Host": "mcp.example.com"})
            ).status_code == 401
            assert (
                await client.delete("/mcp", headers={"Host": "mcp.example.com"})
            ).status_code == 401

        # Duplicate Authorization headers via raw ASGI (cannot be bypassed)
        status, headers, _ = await _run_raw_asgi(
            oauth_env.app,
            method="POST",
            path="/mcp",
            headers=[
                (b"host", b"mcp.example.com"),
                (b"content-type", b"application/json"),
                (b"authorization", f"Bearer {valid_token}".encode("ascii")),
                (b"authorization", b"Bearer second-token"),
            ],
            body=json.dumps(kill_rpc).encode("utf-8"),
        )
        assert status == 401
        assert f'resource_metadata="{expected_meta}"' in headers["www-authenticate"]

        # Non-ASCII Authorization header -> 401 (never 500)
        status_non_ascii, _, _ = await _run_raw_asgi(
            oauth_env.app,
            method="POST",
            path="/mcp",
            headers=[
                (b"host", b"mcp.example.com"),
                (b"content-type", b"application/json"),
                (b"authorization", b"Bearer \xff\xfe"),
            ],
            body=json.dumps(kill_rpc).encode("utf-8"),
        )
        assert status_non_ascii == 401

    assert oauth_env.spy.killed == []
    assert oauth_env.spy.commands_run == []
    assert oauth_env.spy.files_written == []


async def test_insufficient_scope_returns_403_across_all_methods_and_batches(
    oauth_env: _TestOAuthEnv,
) -> None:
    low_scope_token = oauth_env.mint_token(scope="other:read")

    async with oauth_env.app.router.lifespan_context(oauth_env.app):
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=oauth_env.app),
            base_url="http://127.0.0.1:8000",
        ) as client:
            for payload in (
                _init_payload(),
                {"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
                {
                    "jsonrpc": "2.0",
                    "id": 3,
                    "method": "tools/call",
                    "params": {
                        "name": "sandbox_kill",
                        "arguments": {"sandbox_id": "sbx-1"},
                    },
                },
                [_init_payload(1), {"jsonrpc": "2.0", "id": 2, "method": "tools/list"}],
            ):
                resp = await client.post(
                    "/mcp", headers=_mcp_headers(low_scope_token), json=payload
                )
                assert resp.status_code == 403
                assert "insufficient_scope" in resp.headers["www-authenticate"]

            r_get = await client.get(
                "/mcp",
                headers={
                    "Host": "mcp.example.com",
                    "Authorization": f"Bearer {low_scope_token}",
                    "Accept": "text/event-stream",
                },
            )
            assert r_get.status_code == 403

            r_del = await client.delete(
                "/mcp",
                headers={
                    "Host": "mcp.example.com",
                    "Authorization": f"Bearer {low_scope_token}",
                },
            )
            assert r_del.status_code == 403

    assert oauth_env.spy.killed == []


async def test_full_mcp_flow_session_binding_mid_session_expiry_and_no_token_leak(
    oauth_env: _TestOAuthEnv,
    caplog: pytest.LogCaptureFixture,
) -> None:
    caplog.set_level(logging.DEBUG)
    alice_token = oauth_env.mint_token(sub="user-alice", client_id="chatgpt")
    bob_token = oauth_env.mint_token(sub="user-bob", client_id="chatgpt")
    expired_alice = oauth_env.mint_token(
        sub="user-alice", client_id="chatgpt", exp_offset=-30
    )

    async with oauth_env.app.router.lifespan_context(oauth_env.app):
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=oauth_env.app),
            base_url="http://127.0.0.1:8000",
        ) as client:
            # 1. Initialize session with valid Alice token
            init_resp = await client.post(
                "/mcp",
                headers=_mcp_headers(alice_token),
                json=_init_payload(),
            )
            assert init_resp.status_code == 200
            session_id = init_resp.headers["mcp-session-id"]
            init_body = _parse_mcp_response_body(init_resp)
            assert init_body["result"]["serverInfo"]["name"] == "OpenSandbox Sandbox"

            # Send initialized notification
            notif = await client.post(
                "/mcp",
                headers=_mcp_headers(alice_token, session_id=session_id),
                json={"jsonrpc": "2.0", "method": "notifications/initialized"},
            )
            assert notif.status_code in (200, 202)

            # 2. List tools (all 21 tools intact)
            list_resp = await client.post(
                "/mcp",
                headers=_mcp_headers(alice_token, session_id=session_id),
                json={"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
            )
            assert list_resp.status_code == 200
            tools = _parse_mcp_response_body(list_resp)["result"]["tools"]
            assert len(tools) == 21

            # 3. Call sandbox_kill -> backend receives OPEN_SANDBOX_API_KEY only
            kill_resp = await client.post(
                "/mcp",
                headers=_mcp_headers(alice_token, session_id=session_id),
                json={
                    "jsonrpc": "2.0",
                    "id": 3,
                    "method": "tools/call",
                    "params": {
                        "name": "sandbox_kill",
                        "arguments": {"sandbox_id": "sbx-valid"},
                    },
                },
            )
            assert kill_resp.status_code == 200
            assert alice_token not in kill_resp.text
            assert oauth_env.spy.killed == [("sbx-valid", BACKEND_API_KEY)]

            failed_tool = await client.post(
                "/mcp",
                headers=_mcp_headers(alice_token, session_id=session_id),
                json={
                    "jsonrpc": "2.0",
                    "id": 6,
                    "method": "tools/call",
                    "params": {
                        "name": "command_run",
                        "arguments": {
                            "sandbox_id": "sbx-missing",
                            "command": "test-command-not-for-audit",
                        },
                    },
                },
            )
            assert _parse_mcp_response_body(failed_tool)["result"]["isError"]
            assert not oauth_env.spy.commands_run

            # 4. Mid-session token expiration -> 401 and no backend side effect
            exp_resp = await client.post(
                "/mcp",
                headers=_mcp_headers(expired_alice, session_id=session_id),
                json={
                    "jsonrpc": "2.0",
                    "id": 4,
                    "method": "tools/call",
                    "params": {
                        "name": "sandbox_kill",
                        "arguments": {"sandbox_id": "sbx-expired"},
                    },
                },
            )
            assert exp_resp.status_code == 401
            assert oauth_env.spy.killed == [("sbx-valid", BACKEND_API_KEY)]

            # 5. Cross-user session hijacking (Bob reuses Alice's session_id) -> 404
            hijack_resp = await client.post(
                "/mcp",
                headers=_mcp_headers(bob_token, session_id=session_id),
                json={
                    "jsonrpc": "2.0",
                    "id": 5,
                    "method": "tools/call",
                    "params": {
                        "name": "sandbox_kill",
                        "arguments": {"sandbox_id": "sbx-hijack"},
                    },
                },
            )
            assert hijack_resp.status_code == 404
            assert oauth_env.spy.killed == [("sbx-valid", BACKEND_API_KEY)]

            # 6. Authorized DELETE closes Alice's session cleanly
            del_resp = await client.delete(
                "/mcp",
                headers=_mcp_headers(alice_token, session_id=session_id),
            )
            assert del_resp.status_code in (200, 204)

    # Confirm neither user JWT nor backend API key leaked into logs
    log_text = "\n".join(rec.getMessage() for rec in caplog.records)
    assert alice_token not in log_text
    assert bob_token not in log_text
    assert BACKEND_API_KEY not in log_text
    audit = [
        json.loads(record.getMessage().removeprefix("mcp_tool_call "))
        for record in caplog.records
        if record.name == "opensandbox_mcp.audit"
    ]
    assert audit == [
        {
            "client_id": "chatgpt",
            "iss": ISSUER,
            "sub": "user-alice",
            "tool": "sandbox_kill",
            "sandbox_id": "sbx-valid",
            "outcome": "succeeded",
        },
        {
            "client_id": "chatgpt",
            "iss": ISSUER,
            "sub": "user-alice",
            "tool": "command_run",
            "sandbox_id": "sbx-missing",
            "outcome": "failed",
        },
    ]


async def test_dns_rebinding_host_and_origin_enforcement(
    oauth_env: _TestOAuthEnv,
) -> None:
    token = oauth_env.mint_token()

    async with oauth_env.app.router.lifespan_context(oauth_env.app):
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=oauth_env.app),
            base_url="http://127.0.0.1:8000",
        ) as client:
            # Untrusted Host header -> 421 Invalid Host
            bad_host = _mcp_headers(token)
            bad_host["Host"] = "evil.example.com"
            r_host = await client.post("/mcp", headers=bad_host, json=_init_payload())
            assert r_host.status_code == 421

            # Untrusted Origin header -> 403 Invalid Origin
            bad_origin = _mcp_headers(token)
            bad_origin["Origin"] = "https://evil.example.com"
            r_origin = await client.post(
                "/mcp", headers=bad_origin, json=_init_payload()
            )
            assert r_origin.status_code == 403


async def test_concurrent_valid_and_invalid_clients_isolated(
    oauth_env: _TestOAuthEnv,
) -> None:
    valid_token = oauth_env.mint_token(sub="user-concurrent")
    low_scope = oauth_env.mint_token(sub="user-low", scope="other:read")
    invalid_token = oauth_env.mint_token(aud="https://wrong.example.com/mcp")

    async with oauth_env.app.router.lifespan_context(oauth_env.app):
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=oauth_env.app),
            base_url="http://127.0.0.1:8000",
        ) as client:

            async def call(tok: str | None) -> int:
                resp = await client.post(
                    "/mcp",
                    headers=_mcp_headers(tok),
                    json=_init_payload(),
                )
                return resp.status_code

            results = await asyncio.gather(
                call(valid_token),
                call(None),
                call(low_scope),
                call(invalid_token),
                call(valid_token),
                call(low_scope),
                call(None),
                call(valid_token),
            )
            assert results == [200, 401, 403, 401, 200, 403, 401, 200]


async def test_client_discovery_to_authorized_command_and_file_tools(
    oauth_env: _TestOAuthEnv,
) -> None:
    """Demonstrates the full MCP OAuth 2.1 client sequence:
    401 challenge -> RFC 9728 metadata -> AS token -> initialize -> command_run & file_write.
    """
    async with oauth_env.app.router.lifespan_context(oauth_env.app):
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=oauth_env.app),
            base_url="http://127.0.0.1:8000",
        ) as client:
            # Step 1: Unauthenticated probe yields 401 + resource_metadata URL
            probe = await client.post(
                "/mcp", headers=_mcp_headers(), json=_init_payload()
            )
            assert probe.status_code == 401
            www_auth = probe.headers["www-authenticate"]
            assert (
                'resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp"'
                in www_auth
            )

            # Step 2: Client fetches RFC 9728 Protected Resource Metadata
            meta_resp = await client.get(
                "/.well-known/oauth-protected-resource/mcp",
                headers={"Host": "mcp.example.com"},
            )
            assert meta_resp.status_code == 200
            meta = meta_resp.json()
            assert meta["resource"] == RESOURCE
            assert meta["authorization_servers"] == [ISSUER]
            assert meta["scopes_supported"] == [REQUIRED_SCOPE]

            # Step 3: External Authorization Server issues audience-bound access token
            access_token = oauth_env.mint_token(
                aud=meta["resource"],
                scope=" ".join(meta["scopes_supported"]),
            )

            # Step 4: Client initializes MCP session and invokes high-privilege tools
            init = await client.post(
                "/mcp", headers=_mcp_headers(access_token), json=_init_payload()
            )
            assert init.status_code == 200
            session_id = init.headers["mcp-session-id"]

            await client.post(
                "/mcp",
                headers=_mcp_headers(access_token, session_id=session_id),
                json={"jsonrpc": "2.0", "method": "notifications/initialized"},
            )

            cmd_resp = await client.post(
                "/mcp",
                headers=_mcp_headers(access_token, session_id=session_id),
                json={
                    "jsonrpc": "2.0",
                    "id": 10,
                    "method": "tools/call",
                    "params": {
                        "name": "command_run",
                        "arguments": {
                            "sandbox_id": "sbx-demo",
                            "command": "echo ok",
                            "connect_if_missing": True,
                        },
                    },
                },
            )
            assert cmd_resp.status_code == 200
            assert oauth_env.spy.commands_run == [("sbx-demo", "echo ok")]

            file_resp = await client.post(
                "/mcp",
                headers=_mcp_headers(access_token, session_id=session_id),
                json={
                    "jsonrpc": "2.0",
                    "id": 11,
                    "method": "tools/call",
                    "params": {
                        "name": "file_write",
                        "arguments": {
                            "sandbox_id": "sbx-demo",
                            "path": "/tmp/hello.txt",
                            "content": "hello",
                        },
                    },
                },
            )
            assert file_resp.status_code == 200
            assert oauth_env.spy.files_written == [
                ("sbx-demo", "/tmp/hello.txt", "hello")
            ]
