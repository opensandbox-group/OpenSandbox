---
title: MCP Proxy Bridge
description: Connect official MCP SDKs to HTTP and stdio MCP servers through an OpenSandbox execd endpoint.
---

# MCP Proxy Bridge

Execd can act as an MCP (Model Context Protocol) gateway: clients connect with any official MCP SDK over Streamable HTTP at `/mcp/{name}`, and execd forwards to a registered upstream MCP server. No MCP-specific SDK work is needed on the client side — `streamablehttp_client("https://<sandbox>/mcp/{name}")` in Python or `HttpClientStreamableHttpTransport.builder(base).endpoint("/mcp/{name}")` in Java, both with the usual execd access-token header.

## Table of Contents

- [Registering Upstreams](#registering-upstreams)
- [Connecting with MCP SDKs](#connecting-with-mcp-sdks)
- [Transports](#transports)
- [Managing Servers at Runtime](#managing-servers-at-runtime)
- [Keepalive Tuning](#keepalive-tuning)
- [Security Notes](#security-notes)

## Registering Upstreams

Upstreams are registered at runtime through the management API; the registry starts empty and `/mcp/{name}` answers `404` until a server with that name is registered. A registration is one JSON object with a `name` and a `transport`:

```json
{"name": "search", "transport": "http", "url": "http://127.0.0.1:8000/mcp", "headers": {"Authorization": "Bearer ..."}}
```

```json
{"name": "fs", "transport": "stdio", "command": "npx", "args": ["-y", "@modelcontextprotocol/server-everything"], "cwd": "/workspace"}
```

## Connecting with MCP SDKs

Any client that speaks Streamable HTTP works. Python:

```python
from mcp import streamablehttp_client
from mcp.client.session import ClientSession

async with streamablehttp_client(
    "https://<sandbox-host>/mcp/search",
    headers={"X-EXECD-ACCESS-TOKEN": "<token>"},
) as (read, write, _):
    async with ClientSession(read, write) as session:
        await session.initialize()
        tools = await session.list_tools()
```

Java:

```java
var transport = HttpClientStreamableHttpTransport.builder(baseUri)
    .endpoint("/mcp/search")
    .customHeaders(Map.of("X-EXECD-ACCESS-TOKEN", token))
    .build();
```

Upstream status codes — including `404` (session terminated) and `405` — are returned unmodified so clients apply their normal session-recovery behavior.

## Transports

- **`http`** proxies a Streamable HTTP MCP server as an opaque stream — request/response bodies, SSE streams, status codes, and MCP headers (`Mcp-Session-Id`, `MCP-Protocol-Version`) pass through without JSON-RPC inspection, so the bridge stays compatible with any protocol version both sides support. Query strings of the configured URL (`?api_key=...`) are preserved and merged with client parameters; a configured `Host` header routes to `Request.Host`.
- **`stdio`** spawns the registered command as a sandbox-local subprocess — one per MCP session — and bridges it to Streamable HTTP using the official Go MCP SDK. Sessions end (and subprocesses terminate) on client `DELETE`, idle timeout, or child exit. Commands come from the management API; in-sandbox clients can already run arbitrary commands through the exec APIs, so a client-supplied stdio command grants no new privilege.

## Managing Servers at Runtime

Clients register, list, and remove upstreams through the management API (same access-token check; the server name `servers` is reserved for it):

- `GET /mcp/servers` — list every bridge as `{"name", "transport"}`.
- `POST /mcp/servers` — register one server; the body is a single registration object as shown above (both transports, `headers` may carry upstream credentials). Answers `201` with the created entry, `400` on validation errors, `409` when the name is taken.
- `DELETE /mcp/servers/{name}` — remove a bridge and tear down its stdio sessions. Answers `204`, or `404` when unknown.

For example, registering a stdio upstream and connecting to it right after:

```bash
curl -X POST "https://<sandbox-host>/mcp/servers" \
  -H "X-EXECD-ACCESS-TOKEN: <token>" \
  -H "Content-Type: application/json" \
  -d '{"name":"fs","transport":"stdio","command":"npx","args":["-y","@modelcontextprotocol/server-everything"]}'
```

Registrations are in-memory: execd answers `404` for them again after a restart, so control planes or SDKs re-register after recreating a sandbox.

## Keepalive Tuning

Idle SSE responses receive keepalive comment frames every `EXECD_MCP_SSE_KEEPALIVE` (`--mcp-sse-keepalive`, default 30s, `0` disables) so read-idle clients stay connected. Frames are injected only between complete events, never inside one.

## Security Notes

- The execd access token never leaves execd: it is stripped before forwarding, while client `Authorization` headers pass through to the upstream.
- Registered `headers` on an upstream override client-supplied values, which keeps upstream credentials inside the sandbox instead of flowing through every MCP call.
- stdio subprocesses run through the same managed launch path as commands and PTYs: when the hardening floor is enabled they get capability dropping, `no_new_privs`, seccomp, and Landlock like every other workload, and their environment is the standard user-process environment (sandbox binding envs and `EXECD_ENVS` layered in, execd's own config env — `EXECD_ACCESS_TOKEN`, `JUPYTER_TOKEN`, and the rest of the blacklist — stripped) with the registration's `env` overlaid. A stdio MCP server never sees execd's credentials and never runs with the daemon's privileges.

See the [execd architecture page](/architecture/data-plane/execd) for the bridge's design and how it fits into execd.
