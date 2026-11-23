# OpenSandbox MCP Sandbox Server

## 1. Overview

OpenSandbox MCP Server exposes the OpenSandbox Python SDK as MCP tools for Claude Code, Cursor, and other MCP-capable clients. It provides focused sandbox lifecycle management, command execution, and text file operations.

## 2. Installation & Startup

### Source

```bash
uv sync
uv run opensandbox-mcp
```

### Package

```bash
pip install opensandbox-mcp
opensandbox-mcp
```

### Configuration

Environment variables:

- `OPEN_SANDBOX_API_KEY`
- `OPEN_SANDBOX_DOMAIN`

CLI overrides:

```bash
opensandbox-mcp --api-key ... --domain ... --protocol https
```

Config fields:

- `api_key`: OpenSandbox API key for authentication.
- `domain`: OpenSandbox API domain, for example `api.opensandbox.io`.
- `protocol`: `http` or `https` for API requests.
- `request_timeout_seconds`: HTTP request timeout in seconds.
- `transport`: `stdio` by default, or `streamable-http`.
- `use-server-proxy`: when present, forces the SDK client to use server proxy mode.

### Streamable HTTP

```bash
opensandbox-mcp \
  --transport streamable-http \
  --host 0.0.0.0 \
  --port 8000
```

The HTTP transport defaults to `127.0.0.1:8000`. Set `--host 0.0.0.0` when the MCP server must be reached from another container, and use `--port` to select a different listening port. Keep the default loopback host unless the container network or an upstream gateway protects the exposed endpoint. **Do not expose unauthenticated Streamable HTTP directly to the public internet.**

### OAuth 2.1 Resource Server (Streamable HTTP, Opt-In)

For remote HTTPS MCP clients (such as ChatGPT), `opensandbox-mcp` can run as an **OAuth 2.1 Resource Server** (`--oauth-mode jwt`) backed by an external Authorization Server / IdP. It advertises RFC 9728 Protected Resource Metadata and validates JWT bearer access tokens via JWKS. Every authorized caller shares the permissions of the configured `OPEN_SANDBOX_API_KEY`. Restrict who can obtain tokens for this deployment's audience and required scope in the IdP.

```bash
OPEN_SANDBOX_API_KEY="replace-with-backend-api-key" \
OPEN_SANDBOX_DOMAIN="127.0.0.1:8080" \
opensandbox-mcp \
  --transport streamable-http \
  --host 127.0.0.1 \
  --port 8000 \
  --oauth-mode jwt \
  --oauth-issuer-url "https://idp.example.com" \
  --oauth-resource-url "https://mcp.example.com/mcp" \
  --oauth-required-scope "opensandbox:mcp"
```

Key OAuth options (CLI flags override `OPEN_SANDBOX_MCP_OAUTH_*` environment variables; invalid or partial configuration fails closed at startup):

| CLI Flag | Environment Variable | Default | Purpose |
| --- | --- | --- | --- |
| `--oauth-mode` | `OPEN_SANDBOX_MCP_OAUTH_MODE` | `none` | `none` or `jwt` (`streamable-http` only). |
| `--oauth-issuer-url` | `OPEN_SANDBOX_MCP_OAUTH_ISSUER_URL` | — | Required when `jwt`. Exact trusted issuer URL (HTTPS). |
| `--oauth-resource-url` | `OPEN_SANDBOX_MCP_OAUTH_RESOURCE_URL` | — | Required when `jwt`. Match the client-facing MCP URL exactly, with lowercase scheme and host and no trailing slash on a non-root path. |
| `--oauth-required-scope` | `OPEN_SANDBOX_MCP_OAUTH_REQUIRED_SCOPES` | — | Required when `jwt` (repeatable flag or space/comma-separated env). |
| `--oauth-audience` | `OPEN_SANDBOX_MCP_OAUTH_AUDIENCE` | `resource_url` | Expected JWT `aud` claim override. |
| `--oauth-jwks-url` | `OPEN_SANDBOX_MCP_OAUTH_JWKS_URL` | Auto-discovered | Explicit trusted JWKS URL (required for cross-origin JWKS). |
| `--oauth-allowed-algs` | `OPEN_SANDBOX_MCP_OAUTH_ALLOWED_ALGS` | `RS*/PS*/ES*/EdDSA` | Allowed asymmetric JWS algorithms (`none` and `HS*` rejected). |
| `--oauth-clock-skew-seconds` | `OPEN_SANDBOX_MCP_OAUTH_CLOCK_SKEW_SECONDS` | `30` | Leeway for `exp`/`nbf` (`0..300`). |
| `--oauth-http-timeout-seconds` | `OPEN_SANDBOX_MCP_OAUTH_HTTP_TIMEOUT_SECONDS` | `5` | Outbound discovery/JWKS timeout (`(0..60]`). |
| `--oauth-jwks-cache-ttl-seconds` | `OPEN_SANDBOX_MCP_OAUTH_JWKS_CACHE_TTL_SECONDS` | `300` | Maximum JWKS cache lifetime (`(0..86400]`). |
| `--oauth-jwks-refresh-cooldown-seconds` | `OPEN_SANDBOX_MCP_OAUTH_JWKS_REFRESH_COOLDOWN_SECONDS` | `30` | Minimum interval between forced refreshes on unknown `kid`. |
| `--oauth-allowed-host` | `OPEN_SANDBOX_MCP_OAUTH_ALLOWED_HOSTS` | From `resource_url` | Extra `Host` header entries allowed by DNS-rebinding checks. |
| `--oauth-allow-insecure-http` | `OPEN_SANDBOX_MCP_OAUTH_ALLOW_INSECURE_HTTP` | `false` | Permits `http://` strictly on loopback hosts for local testing. |

For the full architecture diagram, IdP compatibility checklist, `curl` verification commands, ChatGPT setup checklist, troubleshooting table, threat model, and `mcp-oauth-proxy` migration/rollback guide, see [docs/sdks/mcp.md](../../../../docs/sdks/mcp.md#oauth-21-resource-server-streamable-http).

## 3. Integrations

### Claude Code stdio

```bash
claude mcp add opensandbox-sandbox --transport stdio -- \
  opensandbox-mcp --api-key "$OPEN_SANDBOX_API_KEY" --domain "$OPEN_SANDBOX_DOMAIN"
```

### Claude Code http

```bash
claude mcp add opensandbox-sandbox --transport http http://localhost:8000/mcp
```

### Cursor stdio

```json
{
  "mcpServers": {
    "opensandbox-sandbox": {
      "command": "opensandbox-mcp",
      "args": [
        "--api-key",
        "${OPEN_SANDBOX_API_KEY}",
        "--domain",
        "${OPEN_SANDBOX_DOMAIN}"
      ]
    }
  }
}
```

### Cursor http

```json
{
  "mcpServers": {
    "opensandbox-sandbox": {
      "url": "http://localhost:8000/mcp"
    }
  }
}
```

## 4. Tools

Notes:

- All tools operate on a `sandbox_id` returned by `sandbox_create` or `sandbox_connect`.
- `file_read`/`file_write` are text-only; use `encoding` and `range_header` for large files.

### Sandbox

- `sandbox_create`: create a new sandbox and register it locally
- `sandbox_connect`: attach to an existing sandbox and register it locally
- `sandbox_kill`: terminate a sandbox by ID
- `sandbox_get_info`: fetch sandbox info by ID
- `sandbox_list`: list sandboxes with optional `filter` object
- `sandbox_renew`: extend sandbox expiration
- `sandbox_healthcheck`: check if sandbox is healthy
- `sandbox_get_metrics`: get resource metrics
- `sandbox_get_endpoint`: get network endpoint for a port

### Command Execution

- `command_run`: run a command inside a sandbox
- `command_get_status`: get a command's running state, exit code, and error
- `command_get_logs`: read combined stdout/stderr of a background command, with an optional `cursor` for incremental reads
- `command_interrupt`: interrupt a running command

### Filesystem

- `file_read`: read a text file
- `file_write`: write a text file
- `file_delete`: delete files
- `file_search`: search for files by glob
- `file_create_directories`: create directories
- `file_delete_directories`: delete directories
- `file_move`: move/rename files or directories
- `file_replace_contents`: replace file content

## 5. Minimal Workflow

1. `sandbox_create` -> keep the `sandbox_id`.
2. `file_write` code or assets into the sandbox.
3. `command_run` to execute, install dependencies, or start a service.
4. `sandbox_get_endpoint` if you expose a port.
5. `sandbox_kill` when finished.

### Background Commands

1. `command_run` with `background=true` -> keep the execution `id`.
2. `command_get_status` until `running` is `false`; check `exit_code` and `error`.
3. Between status checks, `command_get_logs` with the `cursor` from the previous call to read only new output.
4. After `running` is `false`, call `command_get_logs` once more to read the remaining output.
5. `command_interrupt` to stop a command early.

The `cursor` is a server-side position: pass back the returned value instead of computing it from the content length. Empty `content` does not mean the command finished.

## 6. Usage Examples

Here are some examples of what you can ask an LLM to do:

- "Create a Python sandbox and run a quick health command."
- "Write a Python script into the sandbox and run it."
- "Download a GitHub repo, install dependencies, and run its tests."
- "Run the test suite in the background, report progress, and tell me whether it passed."
- "Generate a CSV file with fake sales data and run a simple summary script."
- "Start a tiny web server on port 8000 and return the public URL."
- "Build a minimal REST API (hello + health) and expose it on port 8000."
- "Create a tar.gz of /app and report the file size."
- "Build a simple Snake game and return the web endpoint where it can be accessed."
