---
title: Implementation Compatibility
description: Published OpenSandbox contracts, reference implementation boundaries, and current SDK expectations for independent implementations.
---

# Implementation compatibility

OpenSandbox separates sandbox lifecycle management, in-sandbox execution, and
workload-specific services. This page describes the current repository's
interfaces and client behavior. It does not define a new v1 conformance profile
or a capability-negotiation protocol. Pin the spec revision and SDK version you
target: an implementation of one API is not automatically compatible with every
feature exposed by a sandbox SDK.

## Contracts and processes

The [published OpenAPI files](/api/) are the starting point for implementation:

| Contract | Interface and consumers |
| --- | --- |
| [`sandbox-lifecycle.yml`](https://github.com/opensandbox-group/OpenSandbox/blob/main/specs/sandbox-lifecycle.yml) | Control plane: creation, inspection, deletion, expiration, endpoint resolution, pause/resume, snapshots, templates, and lifecycle network-policy operations. Used by sandbox SDKs and management clients. |
| [`diagnostic-api.yml`](https://github.com/opensandbox-group/OpenSandbox/blob/main/specs/diagnostic-api.yml) | Additional lifecycle-server routes for diagnostic log/event descriptors. Needed when clients use diagnostics; not the command-output API. |
| [`execd-api.yaml`](https://github.com/opensandbox-group/OpenSandbox/blob/main/specs/execd-api.yaml) | Data plane: health, commands, files, directories, code contexts, metrics, and isolated-session operations. Used by the execution services in sandbox SDKs and Code Interpreter clients. |
| [`egress-api.yaml`](https://github.com/opensandbox-group/OpenSandbox/blob/main/specs/egress-api.yaml) | Egress policy and Credential Vault management. A separate API from execd; the reference deployment normally serves it through an egress component. |

The wire contract does not require an independent implementation to run the Go
`execd` binary. A native execution service can serve the execd operations that
its clients use. Compatibility requires their behavior as well as their route
names: request/response schemas, authentication headers, file transfer semantics,
SSE event payloads and completion, errors, and context/command lifetimes all matter.
Implementing only the lifecycle contract does not supply SDK command, file, or
code execution. Conversely, an execd-compatible service alone does not implement
sandbox creation or endpoint resolution.

The reference server's binary injection, container bootstrap, init/supervision,
Jupyter integration, and proxy layout are implementation mechanisms. A backend
can use different mechanisms, but must preserve the observable semantics of the
operations it claims to support. OpenAPI coverage is not complete for every
reference extension: for example, the current [execd router](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/execd/pkg/web/router.go)
exposes `/pty` HTTP/WebSocket routes which are not yet in `execd-api.yaml`.
Do not infer support for such extensions from OpenAPI compatibility alone.

## Workload APIs and endpoint resolution

`GET /v1/sandboxes/{sandboxId}/endpoints/{port}` resolves access to a service on
the requested sandbox port. It does not convert that service into an execd API
or impose execd's command/file schemas on it. In the [AIO example](https://github.com/opensandbox-group/OpenSandbox/blob/main/examples/aio-sandbox/main.py),
OpenSandbox manages the sandbox and resolves port `8080`, then `AioSandboxClient`
speaks the image's own shell/file/browser API.

Endpoint resolution is agnostic to the workload's application API; transport
support still depends on the selected network path. For example, the
[Docker embedding proxy](/architecture/network/single-host-network) forwards
HTTP, SSE, and WebSocket traffic. An arbitrary port number does not imply a
generic TCP/UDP tunnel. Preserve the returned address (including proxy path) and
required headers, and use a client suitable for the service and transport.
Passing through execd's proxy does not make the destination process execd.

## Current Python SDK endpoint expectations

The following describes [`SandboxSync`](https://github.com/opensandbox-group/OpenSandbox/blob/main/sdks/sandbox/python/src/opensandbox/sync/sandbox.py)
in the current tree, rather than a portable protocol requirement for all clients:

| Flow | Endpoint resolution before the health check |
| --- | --- |
| `create()` from an image or snapshot | Resolves execd port `44772`, then egress port `18080`. If the execd endpoint reports template origin, policy operations use the lifecycle service, but this create path has already resolved the egress endpoint. |
| `create_from_template()` | Resolves `44772`; uses lifecycle network-policy operations without resolving `18080`. |
| `connect()` | Resolves `44772` and reads the endpoint response's `OPEN-SANDBOX-ORIGIN` header. Template origin selects lifecycle network policy; other origins also resolve `18080`. |

A custom `health_check` replaces the default execd `/ping` readiness probe.
It does **not** remove these endpoint lookups or the construction of SDK service
adapters. `skip_health_check=True` also still waits for endpoint publication.
This explains why the AIO example's custom health check does not make it a
lifecycle-only SDK client. Resolving an endpoint is distinct from invoking its
API: service adapters are constructed before the application chooses operations.

An independent backend targeting these high-level flows must account for these
lookups, returned headers, and the operations it will expose. A client using only
the lifecycle HTTP API and a workload-specific client can choose a narrower
integration. Verify other SDK languages and versions separately; the table is
not a promise that all SDKs have identical startup behavior. Template origin is
an existing routing signal, not a general capability manifest.

## Backend-dependent features and unsupported operations

The presence of an operation in a spec or SDK does not mean every runtime,
image, or deployment enables it:

| Feature | Current boundary |
| --- | --- |
| Pause/resume and snapshots | Depend on the workload provider, runtime, and storage configuration. In particular, the Kubernetes rootfs snapshot committer rejects gVisor RuntimeClasses. Do not infer snapshot support from basic create/execute support. |
| Templates | Template management uses the Kubernetes Fast Sandbox integration; the reference Docker server rejects it with HTTP `501`. |
| Egress policy and Credential Vault | Depend on egress configuration and backend routing. Template-backed Python sandboxes manage policy through the lifecycle service; `credential_vault` access raises an SDK exception for that origin. Policy support does not imply Vault support. |
| Code execution | The reference execd uses Jupyter and installed kernels for code contexts. A shell-only image does not thereby support notebook-style code execution. A native implementation must reproduce the code API semantics if it claims that feature. |
| PTY and isolated sessions | Depend on platform facilities and reference extensions. `/v1/isolated/capabilities` describes the isolator/hardening environment, not all lifecycle and execution features. Isolated diff/commit currently report unsupported and are not implemented. |

There is no single general capability-negotiation endpoint in the current
contracts. Document a backend's supported operations and prerequisites, and use
feature-specific discovery where available. Unsupported requests should produce
an explicit failure with an actionable error, not a fabricated success. Use the
operation's specified error shape and distinguish an unsupported feature from
missing authorization, missing resources, or a temporarily unready service.

There is also no universal unsupported-operation status to substitute everywhere.
Current reference examples include HTTP `501` for unsupported template management,
HTTP `409` for the snapshot service's unsupported-runtime preflight, and HTTP
`503` for unimplemented isolated diff/commit. See the
[template handler](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/opensandbox_server/api/templates.py),
[snapshot service](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/opensandbox_server/services/snapshot_service.py),
and [isolated API notes](/api/).
These describe present behavior, not a new uniform error convention.

## Choosing an initial compatibility target

Start with a named client version and an explicit operation set. For a command
and file integration, validate lifecycle create/inspect/delete and endpoint
resolution, authentication and readiness, command SSE completion and errors,
and the file operations the client needs. Add code contexts, snapshots, or
policy/Vault operations only when the backend can provide their semantics.
Use the repository's [SDK end-to-end tests](https://github.com/opensandbox-group/OpenSandbox/tree/main/tests)
for the selected operations and document unimplemented features. This is a
practical integration target, not a claim of full protocol conformance.
