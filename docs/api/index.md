---
title: API Specifications
description: OpenAPI specification documents defining the complete API interfaces and data models for OpenSandbox.
---

# OpenSandbox API Specifications

This section contains the OpenAPI specification documents for the OpenSandbox project, defining the complete API interfaces and data models. Use the server base URLs defined in each spec (for example, `http://localhost:8080/v1` for the lifecycle API, `http://localhost:44772` for execd, and `http://localhost:18080` for egress) when constructing requests.

## Specification Files

### 1. sandbox-lifecycle.yml

[OpenAPI source](https://github.com/opensandbox-group/OpenSandbox/blob/main/specs/sandbox-lifecycle.yml)

**Sandbox Lifecycle Management API**

Defines the complete lifecycle interfaces for creating, managing, and destroying sandbox environments from container images or snapshots.

**Core Features:**
- **Sandbox Management**: Create, list, query, and delete sandbox instances with metadata filters and pagination
- **State Control**: Pause and resume sandbox execution
- **Lifecycle States**: Supports transitions across Pending -> Running -> Pausing -> Paused -> Stopping -> Terminated, and error handling with `Failed`
- **Resource & Runtime Configuration**: Specify resource limits and optional Kubernetes `resourceRequests`, image startup `entrypoint`, `platform`, lifecycle hooks, optional `secureAccess`, volumes, environment variables, and opaque `extensions`
- **Image Support**: Create sandboxes from public or private registries, including registry auth
- **Timeout Management**: Optional `timeout` on creation (omit or set to `null` to disable automatic expiration) with explicit renewal via API
- **Endpoint Access**: Retrieve public access endpoints for services running inside sandboxes, including required headers when secured access is enabled; endpoint lookups report the sandbox origin via the `OPEN-SANDBOX-ORIGIN` response header (`template` for fsb golden-image sandboxes)
- **Template Management**: Create, list, inspect, and delete fsb golden-image templates; template builds are asynchronous (poll until `Succeeded`)
- **Snapshot Management**: Create snapshots from sandboxes, list snapshots with source/name filters, and delete snapshots

**Main Endpoints (base path `/v1`):**
- `POST /sandboxes` - Create a sandbox from an image, snapshot, or template with timeout and resource limits
- `GET /sandboxes` - List sandboxes with state/metadata filters and pagination
- `GET /sandboxes/{sandboxId}` - Get full sandbox details (including startup source and entrypoint)
- `DELETE /sandboxes/{sandboxId}` - Delete a sandbox
- `POST /sandboxes/{sandboxId}/snapshots` - Create a snapshot from a sandbox
- `GET /snapshots` - List snapshots with optional source sandbox, exact name, and state filtering plus pagination
- `GET /snapshots/{snapshotId}` - Get snapshot state and metadata
- `DELETE /snapshots/{snapshotId}` - Delete a snapshot
- `POST /sandboxes/{sandboxId}/pause` - Pause a sandbox (asynchronous)
- `POST /sandboxes/{sandboxId}/resume` - Resume a paused sandbox
- `POST /sandboxes/{sandboxId}/renew-expiration` - Renew sandbox expiration (TTL)
- `PATCH /sandboxes/{sandboxId}/metadata` - Patch sandbox metadata (JSON Merge Patch, RFC 7396)
- `GET /sandboxes/{sandboxId}/endpoints/{port}` - Get an access endpoint and required headers; supports `use_server_proxy` and signed-endpoint `expires` parameters
- `GET/PUT/PATCH/DELETE /sandboxes/{sandboxId}/networkpolicy` - Inspect and manage the sandbox egress network policy (Fsb persists intent on the Sandbox CR; other backends proxy the sandbox-side egress service)
- `POST /templates` - Create a fsb template (asynchronous golden-image build)
- `GET /templates` - List templates with metadata filters and pagination
- `GET /templates/{templateId}` - Get template status and artifact references
- `DELETE /templates/{templateId}` - Delete a template

**Optional `Sandbox.allocation` response field:**
- Returned only when the runtime confirms the sandbox's current concrete Pool allocation.
- Omitted for unconfirmed allocations, non-Pool sandboxes, and allocations being released.
- This field is not a request echo, allocation history, or readiness signal, and does not expose Pod names or other Kubernetes-internal fields.

**Authentication:**
- HTTP Header: `OPEN-SANDBOX-API-KEY: your-api-key`
- Environment Variable: `OPEN_SANDBOX_API_KEY` (for SDK clients)

### 2. diagnostic-api.yml {#diagnostics}

[OpenAPI source](https://github.com/opensandbox-group/OpenSandbox/blob/main/specs/diagnostic-api.yml)

**Sandbox Diagnostics API**

Defines best-effort troubleshooting descriptors for sandbox diagnostic logs and events. The descriptors either embed plain-text diagnostic content inline or return a download URL for the content. This spec does not define a structured audit or observability model.

**Main Endpoints (base path `/v1`):**
- `GET /sandboxes/{sandboxId}/diagnostics/logs` - Retrieve a diagnostic log content descriptor; `scope` is required
- `GET /sandboxes/{sandboxId}/diagnostics/events` - Retrieve a diagnostic event content descriptor; `scope` is required

**Authentication:**
- HTTP Header: `OPEN-SANDBOX-API-KEY: your-api-key`
- Environment Variable: `OPEN_SANDBOX_API_KEY` (for SDK clients)

`scope` is required. Docker and Kubernetes support `container`/`all` for logs and
`runtime`/`all` for events. Fast Sandbox supports `runtime`/`all` events; log
collection is not implemented there. Unsupported scopes return
`DIAGNOSTICS_SCOPE_UNSUPPORTED`.

Responses use `delivery: inline` with `content`, or `delivery: url` with
`contentUrl` and an optional expiry. The SDK does not download URL content;
inspect `truncated` and `warnings` before treating results as complete.
Python models and CLI JSON/YAML use snake_case fields such as `content_url`.
CLI raw output prints the content or URL without following it.

See [SDK diagnostics](/sdks/#diagnostics) for language support and
[CLI diagnostics](/cli/#collect-diagnostics) for command examples.

### 3. execd-api.yaml

[OpenAPI source](https://github.com/opensandbox-group/OpenSandbox/blob/main/specs/execd-api.yaml)

**Code Execution API Inside Sandbox**

Defines interfaces for executing code, commands, and file operations within sandbox environments, providing complete code interpreter and filesystem management capabilities. Forward the headers returned by lifecycle endpoint resolution, including
`X-EXECD-ACCESS-TOKEN` when required. Do not hard-code an execd address or assume
the lifecycle API key alone authorizes direct sandbox access.

**Core Features:**
- **Code Execution**: Stateful code execution supporting Python, JavaScript, and other languages with context lifecycle management
- **Command Execution**: Shell command execution with foreground/background modes and polling endpoints for status/output
- **File Operations**: Complete CRUD operations for files and directories
- **Real-time Streaming**: Real-time output streaming via SSE (Server-Sent Events)
- **System Monitoring**: Real-time monitoring of CPU and memory metrics
- **Access Control**: Token-based API authentication via `X-EXECD-ACCESS-TOKEN`

**Main Endpoint Categories:**

**Health Check:**
- `GET /ping` - Service health check

**Code Interpreter:**
- `GET /code/contexts` - List active code execution contexts (filterable by language)
- `DELETE /code/contexts` - Delete all contexts for a language
- `DELETE /code/contexts/{context_id}` - Delete a specific context
- `POST /code/context` - Create a code execution context
- `POST /code` - Execute code in a context (streaming output)
- `DELETE /code` - Interrupt code execution

**Command Execution:**
- `POST /command` - Execute shell command (streaming output)
- `DELETE /command` - Interrupt command execution
- `GET /command/status/{id}` - Get foreground/background command status
- `GET /command/{id}/logs` - Fetch accumulated stdout/stderr for a background command

**Bash Session:**
- `POST /session` - Create a bash session
- `POST /session/{sessionId}/run` - Run command in a bash session (streaming output)
- `DELETE /session/{sessionId}` - Delete a bash session

**Filesystem:**
- `GET /files/info` - Get metadata for files
- `DELETE /files` - Delete files (not directories)
- `POST /files/permissions` - Change file permissions
- `POST /files/mv` - Move/rename files
- `GET /files/search` - Search files (supports glob patterns)
- `POST /files/replace` - Batch replace file content
- `POST /files/upload` - Upload files (multipart)
- `GET /files/download` - Download files (supports range requests)

**Directory Operations:**
- `GET /directories/list` - List directory contents with optional depth control
- `POST /directories` - Create directories with permissions (mkdir -p semantics)
- `DELETE /directories` - Recursively delete directories

**System Metrics:**
- `GET /metrics` - Get system resource metrics
- `GET /metrics/watch` - Watch system metrics in real-time (SSE stream)

**Isolated Execution (base path `/v1/isolated`):**
- `POST /session` - Create an isolated bash session
- `GET /sessions` - List isolated sessions
- `GET /capabilities` - Get isolator capabilities
- `GET /session/{sessionId}` - Get isolated session state
- `DELETE /session/{sessionId}` - Delete an isolated session
- `POST /session/{sessionId}/run` - Run a command in an isolated session (foreground SSE or background execution)
- `GET/DELETE /session/{sessionId}/runs/{runId}` - Get status or interrupt an isolated run
- `GET /session/{sessionId}/runs/{runId}/logs` - Retrieve isolated run logs
- `GET /session/{sessionId}/files/info` - Get file information
- `GET /session/{sessionId}/files/download` - Download a file
- `POST /session/{sessionId}/files/upload` - Upload a file
- `DELETE /session/{sessionId}/files` - Remove files
- `POST /session/{sessionId}/files/mv` - Rename or move files
- `POST /session/{sessionId}/files/permissions` - Change file permissions
- `POST /session/{sessionId}/files/replace` - Replace file content
- `GET /session/{sessionId}/files/search` - Search files
- `GET /session/{sessionId}/directories/list` - List directory contents
- `POST /session/{sessionId}/directories` - Create directories
- `DELETE /session/{sessionId}/directories` - Delete directories

`GET /session/{sessionId}/diff` and `POST /session/{sessionId}/commit` appear
in the contract but are not implemented in the current execd: both return
`503` with a not-supported error, and capabilities report `diff_supported: false`
and `commit_supported: false`. Check `/v1/isolated/capabilities` before using
runtime-dependent isolation features.

### 4. egress-api.yaml

[OpenAPI source](https://github.com/opensandbox-group/OpenSandbox/blob/main/specs/egress-api.yaml)

**Sandbox Egress Runtime API**

Defines the runtime egress policy interface exposed directly by the [egress sidecar](/architecture/network/egress)
inside a sandbox. Unlike lifecycle operations, this API is reached by first resolving
the sandbox endpoint for the egress port and then calling the sidecar endpoint directly.

**Core Features:**
- **Policy Inspection**: Retrieve the currently enforced egress policy and derived runtime mode
- **Policy Mutation**: Patch egress rules at runtime using sidecar merge semantics
- **Direct Sidecar Access**: Access via sandbox endpoint resolution instead of server-side lifecycle forwarding
- **Optional Sidecar Auth**: Supports endpoint-specific headers when the egress sidecar requires auth

**Main Endpoints:**
- `GET /policy` - Get the current egress policy
- `PATCH /policy` - Merge new egress rules into the current policy
- `DELETE /policy` - Remove specific egress rules from the current policy by target
- `POST/GET/PATCH/DELETE /credential-vault` - Create, inspect, mutate, or delete vault state
- `GET /credential-vault/credentials` and `GET /credential-vault/credentials/{credential_name}` - Read sanitized credential metadata
- `GET /credential-vault/bindings` and `GET /credential-vault/bindings/{binding_name}` - Read binding metadata

Credential values are write-only. Enable Credential Proxy and an egress policy
before creating a vault. Template-backed sandboxes have no egress sidecar or
Credential Vault; their policy operations use the lifecycle `networkpolicy`
endpoints. See [Credential Vault](/guides/credential-vault).

## Technical Features

### Streaming Output (Server-Sent Events)

Code execution and command execution interfaces use SSE for real-time streaming output, supporting the following event types:
- `init` - Initialization event
- `status` - Status update
- `stdout` / `stderr` - Standard output/error streams
- `result` - Execution result
- `execution_complete` - Execution completed
- `execution_count` - Execution count
- `error` - Error information

### Resource Limits

Supports flexible resource configuration (similar to Kubernetes):
```json
{
  "cpu": "500m",
  "memory": "512Mi",
  "gpu": "1"
}
```

### File Permissions

Supports Unix-style file permission management:
- Owner
- Group
- Permission mode values such as 644 or 755
