---
title: First-Class Agent Runtime Inside a Sandbox
authors:
  - "@REI-330"
creation-date: 2026-11-18
last-updated: 2026-11-18
status: draft
---

# OSEP-0032: First-Class Agent Runtime Inside a Sandbox

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [API Overview](#api-overview)
  - [Agent Adapters](#agent-adapters)
  - [Session Model](#session-model)
  - [Streaming](#streaming)
  - [Cancellation](#cancellation)
  - [Permissions and Elicitation](#permissions-and-elicitation)
  - [Scoped Identity](#scoped-identity)
  - [Pause and Resume](#pause-and-resume)
  - [Logs and Artifacts](#logs-and-artifacts)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Endpoints](#endpoints)
  - [Request and Response Schemas](#request-and-response-schemas)
  - [Event Schema](#event-schema)
  - [Adapter Manifest](#adapter-manifest)
  - [Protocol Negotiation](#protocol-negotiation)
  - [State Machine and Persistence](#state-machine-and-persistence)
  - [Concurrency and Backpressure](#concurrency-and-backpressure)
  - [Error Codes](#error-codes)
  - [Security and Redaction](#security-and-redaction)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
- [References](#references)
<!-- /toc -->

## Summary

OpenSandbox can already run agents inside a sandbox, but every integration installs the agent, launches it, polls readiness, parses its output format and implements interruption and cleanup on its own. This proposal adds a managed agent runtime: `sandbox.agents.spawn(...)` returns a handle whose `stream()` yields normalized agent events, whose `stop()` performs a bounded cancellation, and which survives sandbox pause/resume through ACP session rehydration. The runtime is lifecycle around a stdio [Agent Client Protocol (ACP)](https://agentclientprotocol.com) child inside an existing [isolated execution session](0013-isolated-execution-api.md), not a new sandbox or a new agent implementation.

## Motivation

Today agents run inside OpenSandbox in three patterns, all of which reimplement the same plumbing:

- Coding CLIs installed in the sandbox and invoked through `commands.run` (Claude Code, Codex CLI, Gemini CLI, Qwen Code, Kimi CLI, OpenCode, Copilot CLI), as the examples in `examples/claude-code`, `examples/codex-cli`, `examples/gemini-cli`, `examples/qwen-code`, `examples/kimi-cli`, `examples/opencode` and `examples/github-copilot-cli-sandbox` show.
- Agent gateways started as long-running processes and exposed over HTTP (OpenClaw, NullClaw), as in `examples/openclaw`.
- Client-side orchestrators (LangGraph, Google ADK, Microsoft Agent Framework) where the agent runs outside the sandbox and only uses the sandbox as a tool.

Each implementation repeats the same seven steps: install, launch, wait for readiness, connect to output, stream or parse events, interrupt, and collect logs and artifacts. The per-CLI impedance mismatch is the expensive part: Claude Code needs `--output-format json` to expose `session_id`, Codex needs `--json` plus JSONL parsing of the first `thread.started` event for `thread_id`, and permission flag semantics churn between releases (for example `--permission-prompts` becoming `--permission-mode dontAsk` in one cycle). The examples that work today do so by encoding those details per CLI instead of relying on a platform contract.

ACP has meanwhile become the common substrate for this exact lifecycle. Gemini CLI, Qwen Code, Kimi CLI and OpenCode speak ACP directly; Claude Agent and Codex CLI have maintained adapters (`zed-industries/claude-agent-acp`, `agentclientprotocol/codex-acp`); Copilot CLI has a public preview. A stdio ACP child therefore gives the runtime a normalized surface for spawning, streaming, cancelling, resuming and permission decisions without teaching OpenSandbox about individual CLIs.

The runtime also closes the seam with sandbox lifecycle. OpenSandbox already pauses and resumes sandboxes ([OSEP-0008](0008-pause-resume-rootfs-snapshot.md), [OSEP-0024](0024-auto-pause-and-wake-on-access-for-fast-sandbox.md)), but an agent session's state is currently in an in-memory child process plus vendor-specific rollout files. A pause/resume that freezes or loses that process leaves clients hanging with no terminal event. The agent runtime defines survival-on-resume once, next to the existing credential vault ([OSEP-0012](0012-credential-vault.md), [OSEP-0025](0025-credential-vault-provider-spi.md)).

### Goals

- One stable lifecycle for any agent: spawn, stream, follow-up prompt, cancel/stop, collect artifacts, resume.
- Adapter-agnostic: a built-in catalogue of stdio ACP adapters, and `custom` for any other stdio ACP binary.
- Normalized streaming events independent of the underlying CLI and its output format.
- A hard cancellation floor below the protocol, so callers always get a terminal event with a machine-readable reason.
- Headless permission and elicitation decisions with an explicit, auditable policy.
- Per-session scoped credentials injected at `session/new`, never long-lived account secrets.
- Survival on pause/resume through persisted ACP session identity and `session/resume`.
- Additive API with no change to `/command`, `/session`, `/code`, `/files` or the existing isolated execution endpoints.

### Non-Goals

- Implementing or hosting an agent model; the adapter supplies the agent binary and the sandbox supplies the runtime.
- Managing MCP servers on behalf of agents; agents keep owning their MCP configuration.
- A graphical console for agent sessions.
- Replacing sandbox runtimes (runc, gVisor, Firecracker) or the isolated execution isolator.
- Translating per-CLI flags; adapters are the only place CLI-specific behavior lives.
- Multi-tenant scheduling of agents across sandboxes.
- Guaranteeing ACP v2 until that spec version is stable; v2 features are negotiated and optional.

## Requirements

- The new API is additive and must not change the behavior or payloads of existing endpoints.
- The SDK must not silently fall back to `commands.run` when the agent runtime is unavailable; callers receive a typed error and the capability listing.
- The runtime must work on every runtime backend (Docker, Kubernetes, fast-sandbox) that provides an isolated execution session.
- A spawn must be honest about readiness: `spawn` resolves only after the adapter answers `initialize` and `session/new`, or fails with a typed error inside the readiness deadline.
- `stream()` must deliver events in order per session and survive client reconnects through a replay buffer and `Last-Event-ID`.
- `stop()` must make a best effort to cancel through ACP, then escalate to execd process termination, and always produce a terminal event; it must be idempotent.
- A session's identity, negotiated protocol version and event sequence must survive a lifecycle-server restart and sandbox resume.
- Credentials must never appear in events, logs, persisted session records or artifacts.
- Permission requests must be limited by a default policy that fails closed when no caller answers.
- All new behavior must be reachable through the Python SDK first, with the other SDKs following the generated-client process.

## Proposal

### API Overview

The SDK exposes a managed handle instead of a process:

```python
handle = sandbox.agents.spawn(
    kind="claude-code",              # built-in adapter name, or "custom"
    prompt="Fix the failing test",   # initial session/prompt
    cwd="/workspace/repo",
    env={"CI": "1"},
    policy={"permissions": "deny", "timeout_seconds": 900},
    identity={"vault": "github", "scopes": ["repo:read"]},
)

async for event in handle.stream():
    if event.type == "agent.update":
        print(event.update)

await handle.stop(reason="user")
result = await handle.result()       # final stopReason, usage, artifacts
```

The same surface is available over HTTP through the lifecycle server, which proxies to execd. The wire API is described below; the SDK only wraps it.

### Agent Adapters

An adapter is data, not code in the sandbox: a manifest naming a stdio command, its argument template, the environment keys it may receive, its readiness handshake, and the ACP protocol versions it supports. The built-in catalogue ships with OpenSandbox and resolves `kind` to one manifest; adding a CLI means adding a manifest, not a driver.

| Kind | Adapter source | Notes |
| --- | --- | --- |
| `claude-code` | `zed-industries/claude-agent-acp` | Needs `ANTHROPIC_API_KEY` or a vault-provided token |
| `codex` | `agentclientprotocol/codex-acp` | Auth through the Codex CLI's own credential path |
| `gemini` | Gemini CLI ACP mode | Native ACP support |
| `qwen` | Qwen Code ACP mode | Native ACP support |
| `kimi` | Kimi CLI ACP mode | Native ACP support |
| `opencode` | OpenCode ACP mode | Native ACP support |
| `custom` | caller-supplied `command` and `args` | Only when the sandbox policy allows custom adapters |

The catalogue records the adapter package and a version pin, not a command line copied into the document; the manifest (below) is where the exact argv lives, so a packaging change updates one file.

An unknown `kind` fails with `unknown_agent_kind` and lists the supported adapters. A manifest is selected once per session; its command runs inside the isolated execution session, so it inherits the sandbox's image, mounts and network policy.

### Session Model

An agent session is three things at three layers:

1. An **isolated execution session** in execd ([OSEP-0013](0013-isolated-execution-api.md)) providing the namespace, workspace overlay, resource scope and process group that owns the agent.
2. An **ACP child** talking JSON-RPC over stdio to the runtime.
3. A persisted **AgentSession record** on the lifecycle server holding the opaque session id, sandbox id, adapter kind, negotiated protocol version, ACP `sessionId`, state, event sequence and stop reason.

State transitions are explicit:

```text
pending -> starting -> ready -> running -> idle -> running -> ... -> stopped
                    \-> failed
running/idle -> paused -> running          (sandbox pause and resume)
```

`ready` means the adapter completed `initialize` and `session/new`; `running` means at least one `session/prompt` is in flight; `idle` means the last turn ended and the session can accept a follow-up prompt; `stopped` means a terminal event was emitted. Sessions are attachable and listable by sandbox, so a client that reconnects (or another client) can resume streaming rather than spawning a duplicate agent.

### Streaming

ACP `session/update` notifications are normalized into OpenSandbox events carried over the existing SSE infrastructure. The normalized event set is deliberately small:

| Event | Payload |
| --- | --- |
| `agent.session` | `sessionId`, adapter, negotiated protocol version, capabilities |
| `agent.update` | `kind` (`text_delta`, `thought`, `tool_call`, `tool_call_update`, `plan`, `available_commands`, `usage`, `mode`), plus the ACP payload fields that carry no local paths or credentials |
| `agent.permission_request` | request id, tool call, options, deadline |
| `agent.result` | `stopReason`, usage, turn duration, artifact manifest |
| `agent.error` | typed error code and message |
| `agent.stopped` | terminal `stopReason` (`end_turn`, `cancelled`, `failed`, `resume_unsupported`, `expired`) |

Every event carries a monotonically increasing `seq` per session. `GET /v1/agents/sessions/<id>/events` accepts `Last-Event-ID` and replays from the buffer, so a dropped connection does not lose events. The buffer is bounded (default 1 MiB per session, configurable); on overflow the stream emits `agent.error` with `buffer_overrun` and the session's terminal state, instead of silently skipping events.

### Cancellation

ACP cancellation is deliberately optional: `session/cancel` (or `$/cancel_request`) MAY be implemented by an adapter, and the terminal signal is a `state_update: idle` carrying a `stopReason`. The runtime therefore defines an escalation ladder:

1. Send `session/cancel` and wait `cancel_grace` (default 5 s).
2. If the adapter has not produced a terminal state, send `SIGTERM` to the agent process group inside the isolated session.
3. After `kill_grace` (default 5 s), send `SIGKILL`.
4. Always emit `agent.stopped` with a machine-readable `stopReason`.

`stop()` is idempotent: a second call returns the recorded terminal state instead of starting a second ladder. Deleting a sandbox stops every agent session in it through the same ladder, and the record says `cancelled` with `reason=sandbox_deleted`.

### Permissions and Elicitation

A headless sandbox has no human at the far end, so the runtime owns the answer to ACP `session/request_permission` and capability-negotiated elicitation. Three policies are defined:

| Policy | Behavior |
| --- | --- |
| `deny` (default) | Deny every request immediately; the agent may continue with a tool error |
| `sandbox` | Auto-approve within the sandbox boundary, where the sandbox itself is the externally hardened environment; exact tool names and paths are still recorded |
| `external` | Emit `agent.permission_request` and wait for a caller decision; timeout (default 60 s) denies |

The policy lives in the runtime, not in per-CLI flags. Every decision is recorded with the request, the chosen option and the policy that decided it, so an audit trail can tell an automatic allow from a human one. Elicitation (form/url) follows the same rule: `deny` by default, `external` for out-of-band answers, and no passthrough of raw CLI flags.

### Scoped Identity

Credentials are injected at `session/new`, not at sandbox creation, so scope follows the agent run rather than the sandbox lifetime. The runtime resolves the requested vault scope through the credential vault, passes allowed values as environment or ACP `mcpServers` headers, and records only the vault key and scope in the session record. If the vault is unavailable the spawn fails closed with `identity_unavailable`, unless the caller explicitly sets `identity: none`. On resume, credentials are re-injected before `session/resume` so a restored session never runs with stale or missing tokens; the OSEP-0025 delegated-header path is the same mechanism, so no second credential channel is introduced.

### Pause and Resume

Sandbox pause freezes the agent's process tree; resume either continues it (same host, same process) or restores the workspace without the process (cross-host, snapshot restore, fast-sandbox wake). The runtime defines both outcomes:

- On pause the session record is marked `paused` with the last `seq`. The default `on_pause` policy is `cancel`: the runtime stops the agent cleanly before the sandbox freezes, so a frozen turn is never reported as running. `on_pause: wait` is available for deployments that can guarantee the process survives.
- On resume the runtime reads the record, re-injects vault identity, and calls `session/resume` with the persisted ACP `sessionId` and `replayFrom` where the adapter supports it. A resumed session emits `agent.session` with `resumed: true` and continues the previous `seq`.
- If the adapter has no resume support, the session ends `resume_unsupported`; the transcript, artifacts and last events remain readable, and the caller decides whether to spawn a new session with a replayed prompt.

This makes pause/resume a defined lifecycle rather than an implicit assumption, and it matches the on-disk rollout files that Claude Code and Codex already persist.

### Logs and Artifacts

Agent process output that is not ACP (stderr, crash dumps) is captured by execd and exposed through the existing command-log path with the agent session id attached. Artifacts come from two sources:

- The isolated session's workspace overlay, exported with `GET /v1/isolated/session/<id>/diff` (tar.gz), so only changes the agent made are returned.
- Paths the adapter declares as outputs in its final `agent.result`, bounded by the policy's maximum artifact size.

Artifacts are listed in `agent.result` and downloadable through a session-scoped endpoint; paths outside the session workspace are never collected, and secrets detected by the existing redaction rules are removed from the manifest.

### Notes/Constraints/Caveats

- ACP v2 is still labeled draft; the runtime must negotiate per connection and must not require v2 features.
- The adapter process is long-lived; execd must keep it in the isolated session's process group so cancellation and sandbox deletion reach the whole tree.
- A session's first `session/prompt` is what starts work; `spawn` with no prompt opens a ready session that waits for `agents.prompt`.
- Event normalization must stay additive: a new ACP field becomes a new event field, not a breaking change to existing ones.
- The runtime is off by default in the first release (`[agents] enabled = false` in server config) so operators can validate it without changing existing sandbox behavior.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Adapter availability or version drift breaks spawn | Pin adapters in the sandbox image or a lock; readiness failure is typed and lists the command that failed |
| ACP v2 churn | Negotiate the lowest common version; feature flags; v2 remains optional |
| Cancellation hangs behind a stuck adapter | Hard SIGTERM/SIGKILL ladder with a bounded deadline; terminal event is guaranteed |
| Headless auto-approval widens blast radius | `deny` default; `sandbox` is explicit; decisions are recorded; egress policy remains the boundary |
| Event volume overwhelms the buffer | Bounded per-session buffer plus `buffer_overrun`; callers can attach with `Last-Event-ID` |
| Credentials leak into events or artifacts | Injection at `session/new`; redaction before persistence; no plaintext in records or logs |
| Resume restores a session without identity | Re-inject before `session/resume`; fail closed if identity cannot be restored |

## Design Details

### Endpoints

execd owns the process; the lifecycle server exposes the same operations to SDKs and proxies them, so a sandbox without a public execd port still works.

| Method | Path (execd) | Purpose |
| --- | --- | --- |
| `POST` | `/v1/agents/sessions` | Spawn an adapter and open a session |
| `GET` | `/v1/agents/sessions` | List sessions in the sandbox |
| `GET` | `/v1/agents/sessions/{id}` | Session state and capabilities |
| `GET` | `/v1/agents/sessions/{id}/events` | SSE stream; supports `Last-Event-ID` |
| `POST` | `/v1/agents/sessions/{id}/prompts` | Follow-up `session/prompt` while idle |
| `POST` | `/v1/agents/sessions/{id}/cancel` | Run the cancellation ladder |
| `POST` | `/v1/agents/sessions/{id}/permissions/{requestId}` | Answer an `external` permission request |
| `DELETE` | `/v1/agents/sessions/{id}` | Cancel and release the session |
| `GET` | `/v1/agents/sessions/{id}/artifacts` | Artifact manifest and downloads |
| `GET` | `/v1/agents/capabilities` | Adapters, ACP versions, limits, policy support |

### Request and Response Schemas

```json
{
  "kind": "claude-code",
  "prompt": "Fix the failing test",
  "cwd": "/workspace/repo",
  "env": {"CI": "1"},
  "policy": {
    "permissions": "deny",
    "timeout_seconds": 900,
    "cancel_grace_seconds": 5,
    "on_pause": "cancel"
  },
  "identity": {"vault": "github", "scopes": ["repo:read"]}
}
```

```json
{
  "id": "agt_01j9...",
  "sandboxId": "sbx_01j9...",
  "kind": "claude-code",
  "state": "running",
  "acpSessionId": "01J9...",
  "protocolVersion": 1,
  "createdAt": "2026-11-18T09:00:00Z",
  "lastSeq": 42,
  "capabilities": {"resume": true, "permissions": true, "elicitation": false}
}
```

`spawn` returns after `ready`; the initial prompt, when present, is sent immediately and the first `agent.update` may arrive before the caller attaches. The `seq` plus replay buffer makes that race harmless.

### Event Schema

```json
{
  "seq": 43,
  "sessionId": "agt_01j9...",
  "type": "agent.update",
  "at": "2026-11-18T09:00:12Z",
  "update": {
    "kind": "text_delta",
    "text": "The failing test is "
  }
}
```

Permission events carry the request id and the options the adapter offered, so a caller can answer exactly what the adapter asked. Result events carry `usage`, `stopReason`, `durationMs` and the artifact manifest, never local absolute paths.

### Adapter Manifest

```yaml
kind: codex
displayName: Codex CLI
transport: stdio
command: ["npx", "-y", "@agentclientprotocol/codex-acp"]
env:
  allow: ["OPENAI_API_KEY", "CODEX_HOME"]
readiness:
  initialize: true
  sessionNew: true
protocol:
  versions: [1, 2]
  preferred: 1
resume:
  supported: true
permissions:
  supported: true
```

Manifests ship as data in the SDK/server image and can be overridden by a sandbox image path for private adapters, but the override is only honored when the sandbox policy allows custom adapters.

### Protocol Negotiation

The runtime sends `initialize` with the adapter's preferred `protocolVersion` and the client capabilities it actually supports (filesystem proxy disabled, terminal disabled, MCP servers supported). The answer fixes the negotiated version for the session, and the runtime records it in the session record and the `agent.session` event. When the adapter answers a lower version, the runtime disables the features that version does not define and never fails a valid v1 adapter because v2 is absent. Feature flags in the initialize result are the only authority; the runtime does not guess by CLI name.

### State Machine and Persistence

The lifecycle server persists the AgentSession record through the same metadata store used for sandbox state, with one compare-and-set write per event batch and per state transition. execd is the source of truth for process liveness; on server restart the server reconciles records against execd's session list and marks a record whose process is gone as `failed` with `reason=process_gone`. On sandbox resume, the reconcile pass is also what triggers `session/resume`. Deleting a sandbox deletes its agent records after the cancellation ladder.

### Concurrency and Backpressure

One in-flight `session/prompt` per session; a prompt while running answers `agent_busy` and suggests attaching to the stream. A sandbox may hold several agent sessions (default limit 4, configurable) because multi-agent setups are a real use case, but the limit and the memory cost are explicit rather than unbounded. The SSE writer applies backpressure per connection: a slow consumer does not block the agent process; events accumulate in the ring buffer until the session's byte budget, then `buffer_overrun` terminates the subscription, not the agent.

### Error Codes

| Code | Meaning |
| --- | --- |
| `agent_disabled` | The runtime is not enabled in server config |
| `unknown_agent_kind` | No manifest matches `kind` |
| `adapter_unavailable` | The adapter command is missing or not executable in the sandbox |
| `adapter_handshake_failed` | `initialize`/`session/new` failed or timed out |
| `protocol_unsupported` | The adapter supports no protocol version the runtime speaks |
| `agent_busy` | A prompt is already in flight |
| `permission_denied` | The policy denied a request |
| `identity_unavailable` | The vault could not deliver the requested scope |
| `resume_unsupported` | The adapter cannot restore the session |
| `cancelled` | Terminal stop reason from `stop()` or sandbox deletion |
| `buffer_overrun` | The event buffer exceeded its budget |
| `process_gone` | execd no longer holds the process |

### Security and Redaction

Adapters run inside the isolated session under its uid/gid and mounts, so they inherit the sandbox's network and filesystem policy. The custom-adapter path is a policy switch, not a default. The runtime redacts vault values, tokens and absolute paths from events before they are persisted, and stores only vault keys in the session record. Permission decisions and adapter commands are logged with the session id so the planned agent audit trail can attribute actions; no log line contains a credential.

## Test Plan

- Unit tests: manifest resolution, unknown-kind errors, event normalization, sequence/replay, state machine transitions, cancellation ladder with a fake adapter, permission policies, redaction, `stop()` idempotency.
- Integration tests with a deterministic fake ACP adapter (no model calls): spawn, stream ordering, follow-up prompt, cancel during a tool call, permission ask and answer, adapter that ignores cancel (verifies SIGTERM/SIGKILL), adapter that fails `initialize`.
- Resume tests: pause and resume on the same host, resume after a simulated cross-host restore, adapter without resume support.
- E2E with one real adapter (Claude Code through `claude-agent-acp`) on Docker and Kubernetes: create a sandbox, spawn, stream a turn, stop, confirm a terminal event and artifacts; run the same scenario after pause/resume.
- Negative authorization: a second sandbox cannot read another sandbox's session events or artifacts; an `external` permission request times out to deny.
- Concurrency: session limit, `agent_busy`, slow consumer with `buffer_overrun`, server restart reconciliation.

## Drawbacks

The runtime adds public API surface and a long-lived process supervisor to execd, and it makes ACP a dependency of a platform primitive rather than an example detail. ACP v2 is still draft, so the runtime must carry version negotiation complexity. Adapter manifests need maintenance as CLI packaging moves. Streaming agent output is high-volume and needs a bounded buffer and clear overflow behavior. Finally, no single adapter can prove the abstraction; the first release must ship at least two real adapters and the fake one to keep the contract honest.

## Alternatives

- **Per-CLI drivers in the SDK.** Rejected: it moves CLI flag and output-format churn into every SDK and does not give the server a lifecycle to supervise.
- **Use `commands.run` / bash sessions only.** Rejected: no readiness contract, no normalized events, no cancellation floor, and no place to attach identity or resume semantics.
- **Keep client-side orchestration.** Rejected as the only option: it is the status quo pain, and it cannot survive sandbox pause/resume because the client owns the process.
- **Run agents behind an HTTP gateway (OpenClaw-style).** Partially compatible: the runtime can hold a `kind` whose adapter launches a gateway, but the first-class contract remains ACP stdio so lifecycle stays the same for every adapter.
- **Let the SDK speak ACP directly over the network.** Rejected: execd has no public port in most deployments, and the sandbox proxy is the existing security boundary.

## Infrastructure Needed

No new service. execd gains the process supervisor and event buffer; the server gains the AgentSession record and proxy endpoints; the SDK gains the handle. CI needs a deterministic fake ACP adapter (Node or Python, fixture-driven) and one real agent adapter with a test credential, run on the existing Docker and Kubernetes lanes.

## Upgrade & Migration Strategy

The API is additive and gated by `[agents] enabled`, default off in the first release. Existing sandboxes, SDKs and examples keep working unchanged. Session records are forward-compatible: unknown fields are preserved by the metadata store and the SDK ignores events it does not know. When ACP v2 stabilizes, adapters that support it advertise it in their manifest; sessions negotiated at v1 keep working. No existing endpoint or payload changes, so there is no migration for current users.

## References

- [OSEP-0013: Isolated Execution API](0013-isolated-execution-api.md)
- [OSEP-0012: Credential Vault and Credential Proxy](0012-credential-vault.md)
- [OSEP-0025: Credential Vault Exchange and Delegated Header Resolution](0025-credential-vault-provider-spi.md)
- [OSEP-0008: Pause and Resume via Rootfs Snapshot](0008-pause-resume-rootfs-snapshot.md)
- [OSEP-0024: Auto-Pause and Wake-on-Access for Fast Sandbox](0024-auto-pause-and-wake-on-access-for-fast-sandbox.md)
- [Agent Client Protocol](https://agentclientprotocol.com)
- Issue [#1287: first-class agent runtime inside a sandbox](https://github.com/alibaba/OpenSandbox/issues/1287)
