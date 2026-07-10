---
title: API Specifications
description: OpenAPI specification documents defining the complete API interfaces and data models for OpenSandbox.
---

# OpenSandbox API Specifications

This section contains the OpenAPI specification documents for the OpenSandbox project, defining the complete API interfaces and data models. Use the server base URLs defined in each spec (for example, `http://localhost:8080/v1` for the lifecycle API, `http://localhost:44772` for execd, and `http://localhost:18080` for egress) when constructing requests.

## Specification Files

### 1. sandbox-lifecycle.yml

**Sandbox Lifecycle Management API**

Defines the complete lifecycle interfaces for creating, managing, and destroying sandbox environments from container images or snapshots.

**Core Features:**
- **Sandbox Management**: Create, list, query, and delete sandbox instances with metadata filters and pagination
- **State Control**: Pause and resume sandbox execution
- **Lifecycle States**: Supports transitions across Pending -> Running -> Pausing -> Paused -> Stopping -> Terminated, and error handling with `Failed`
- **Resource & Runtime Configuration**: Specify CPU/memory/GPU resource limits, image startup `entrypoint`, optional `secureAccess`, environment variables, and opaque `extensions`
- **Image Support**: Create sandboxes from public or private registries, including registry auth
- **Timeout Management**: Optional `timeout` on creation (omit or set to `null` to disable automatic expiration) with explicit renewal via API
- **Endpoint Access**: Retrieve public access endpoints for services running inside sandboxes, including required headers when secured access is enabled
- **Snapshot Management**: Create snapshots from sandboxes, list snapshots, and delete snapshots

**Main Endpoints (base path `/v1`):**
- `POST /sandboxes` - Create a sandbox from an image or snapshot with timeout and resource limits
- `GET /sandboxes` - List sandboxes with state/metadata filters and pagination
- `GET /sandboxes/{sandboxId}` - Get full sandbox details (including startup source and entrypoint)
- `DELETE /sandboxes/{sandboxId}` - Delete a sandbox
- `POST /sandboxes/{sandboxId}/snapshots` - Create a snapshot from a sandbox
- `GET /snapshots` - List snapshots with optional sandbox filtering and pagination
- `GET /snapshots/{snapshotId}` - Get snapshot state and metadata
- `DELETE /snapshots/{snapshotId}` - Delete a snapshot
- `POST /sandboxes/{sandboxId}/pause` - Pause a sandbox (asynchronous)
- `POST /sandboxes/{sandboxId}/resume` - Resume a paused sandbox
- `POST /sandboxes/{sandboxId}/renew-expiration` - Renew sandbox expiration (TTL)
- `PATCH /sandboxes/{sandboxId}/metadata` - Patch sandbox metadata (JSON Merge Patch, RFC 7396)
- `GET /sandboxes/{sandboxId}/endpoints/{port}` - Get an access endpoint for a service port

**Authentication:**
- HTTP Header: `OPEN-SANDBOX-API-KEY: your-api-key`
- Environment Variable: `OPEN_SANDBOX_API_KEY` (for SDK clients)

### 2. diagnostic-api.yml

**Sandbox Diagnostics API**

Defines best-effort troubleshooting descriptors for sandbox diagnostic logs and events. The descriptors either embed plain-text diagnostic content inline or return a download URL for the content. This spec does not define a structured audit or observability model.

**Main Endpoints (base path `/v1`):**
- `GET /sandboxes/{sandboxId}/diagnostics/logs` - Retrieve a diagnostic log content descriptor for an optional scope
- `GET /sandboxes/{sandboxId}/diagnostics/events` - Retrieve a diagnostic event content descriptor for an optional scope

**Authentication:**
- HTTP Header: `OPEN-SANDBOX-API-KEY: your-api-key`
- Environment Variable: `OPEN_SANDBOX_API_KEY` (for SDK clients)

### 3. execd-api.yaml

**Code Execution API Inside Sandbox**

Defines interfaces for executing code, commands, and file operations within sandbox environments, providing complete code interpreter and filesystem management capabilities. All endpoints require the `X-EXECD-ACCESS-TOKEN` header.

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
- `GET /capabilities` - Get isolator capabilities
- `GET /session/{sessionId}` - Get isolated session state
- `DELETE /session/{sessionId}` - Delete an isolated session
- `POST /session/{sessionId}/run` - Run code in an isolated session (SSE streaming)
- `GET /session/{sessionId}/diff` - Download upper directory diff
- `POST /session/{sessionId}/commit` - Commit upper changes to workspace
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

### 4. egress-api.yaml

**Sandbox Egress Runtime API**

Defines the runtime egress policy interface exposed directly by the [egress sidecar](/components/egress)
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
