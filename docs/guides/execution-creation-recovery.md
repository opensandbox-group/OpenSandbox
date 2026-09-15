---
title: Recovering execution creation
description: Caller-bound command and PTY creation within one execd lifetime.
---

# Recovering execution creation

::: warning Experimental proposal
This experimental implementation for [issue #1547](https://github.com/opensandbox-group/OpenSandbox/issues/1547) is pending maintainer design review. It does not claim maintainer acceptance or production validation.
:::

When a command starts but its create response is lost, sending another ordinary
`POST /command` starts another process. An execution ID received in that lost
response cannot help a restarted caller recover its own attempt.

The opt-in operation APIs use `operation_id` to associate an immutable create request with its original
handle. The caller generates and saves this identity **before** the first send,
along with the command/options needed for an identical retry. A trace ID generated
per HTTP attempt is not an operation identity.

## Using the API

1. Authenticated `GET /execution/instance` returns `instance_id`, `issued_at` (server
   Unix seconds), `retention_seconds` (86400), and configured `capacity` (4096 by default).
2. Generate an 8–128 character random token using letters, digits, `_` or `-`.
   Form `<instance_id>.<issued_at>.<token>` and persist the full string and request.
3. Send that identity as required `operation_id` in `POST /command/operations` or `POST /pty/operations`.
4. After a lost response or caller restart, resend the same body or call
   `GET /execution/operation?kind=command` (or `kind=pty`) with header
   `X-EXECD-OPERATION-ID: <saved identity>`.
5. A `creating` response contains a reserved handle, which may not be visible to
   known-ID APIs yet. Query the operation until its creation state resolves.

The new JSON response is:

```json
{
  "id": "original-runtime-handle",
  "kind": "command",
  "state": "created",
  "expires_at": "2026-09-09T00:00:00Z"
}
```

`202 creating` acknowledges a claim, not process startup. `200 created` means
command process creation succeeded, or a dormant PTY session was created.
`200 failed` means creation failed and this identity will not reattempt it.
A command that starts and exits with code 7 has creation state `created`; inspect
`GET /command/status/{id}` for execution state. Do not infer a script's success
or its business effects from the operation record.

Operation commands acknowledge creation with JSON, for both foreground
and background mode. Existing command timeouts still apply. Foreground output has
its existing temporary lifetime and is **not replayed** by this API. Background
output uses the existing `GET /command/{id}/logs` cursor. To consume retained output,
choose background mode in the saved request. Ordinary `/command` requests retain their SSE behavior; unkeyed PTY creation retains `201` with
`session_id`. No existing SDK call gains an operation identity or automatic retry.

## SDK entry points

| SDK | Scope and identity | Creation and reconciliation |
| --- | --- | --- |
| Python async/sync recovery capability | `get_execution_instance()`, `instance.new_operation_id()` | `create_command_operation(id, command, opts=...)`, `create_pty_operation(id, cwd=..., command=...)`, `get_execution_operation(kind, id)` |
| JavaScript recovery capability | `getExecutionInstance()`, exported `newOperationId(instance)` | `createCommandOperation(id, command, opts)`, `createPTYOperation(id, opts)`, `getExecutionOperation(kind, id)` |
| Go `ExecdClient` | `GetExecutionInstance(ctx)`, `instance.NewOperationID()` | `CreateCommandOperation(ctx, id, request)`, `CreatePTYOperation(ctx, id, cwd, command)`, `GetExecutionOperation(ctx, kind, id)` |
| Kotlin `Commands` | `getExecutionInstance()`, `instance.newOperationId()` | `createCommandOperation(id, request)`, `createPTYOperation(id, cwd, command)`, `getExecutionOperation(kind, id)` |
| C# `IExecdCommands` | `GetExecutionInstanceAsync()`, `instance.NewOperationId()` | `CreateCommandOperationAsync(id, command, options)`, `CreatePtyOperationAsync(id, cwd, command)`, `GetExecutionOperationAsync(kind, id)` |

JavaScript obtains the optional capability with exported
`getExecutionOperations(sandbox.commands)`. Python imports
`get_execution_operations` from `opensandbox.services` (async) or
`opensandbox.sync.services` (sync), then calls
`operations = get_execution_operations(sandbox.commands)`. Use the returned
capability for the methods in the table. The standard adapters support it; a legacy
third-party adapter remains valid for ordinary commands and raises `TypeError` when
asked for unsupported recovery. Existing command interfaces and factory return
types do not require the recovery methods.

These calls return a creation model, not the SDK's completed execution model. Save
the operation ID before calling create. On recovery, load the saved ID; do not call
the identity generator again. No method turns an unknown business result into
success or failure. Error mappings preserve the server's conflict/expiry codes.

In C#, import `OpenSandbox.Services` for the `IExecdCommands` extension methods.
They use the additive `IExecutionOperations` capability implemented by the standard
adapter, preserving the existing interface on all supported target frameworks.

### Instance caching for new operations

Call the SDK's instance method for each **new logical operation**, then generate
and save its ID. Standard Python async/sync, JavaScript, Go, Kotlin and C# clients
reuse a private instance snapshot for at most 60 seconds from the start of its
fetch, measured with a monotonic clock. Concurrent cache misses share one fetch.
Returned snapshots are independent of the cache. Failed fetches are not cached.
The raw HTTP endpoint continues to return the current server scope and time.

The cache avoids one network round-trip per command in a busy agent loop. Its age
reduces the remaining recovery window: an ID containing yesterday's server time
does not gain a new 24-hour window when generated today. Do not keep a returned
instance object as a permanent ID source; obtain it through the SDK method for
each new operation. The maximum additional reduction due to cache reuse is 60
seconds, apart from request transit time. The returned operation's `expires_at`
is the authoritative deadline.

An operation's `operation_instance_mismatch` or `operation_expired` response
invalidates the SDK cache for future operations and is returned to the caller.
It never regenerates an ID or resends a create request. Reconciliation always
loads the saved original ID; refreshing the instance is not recovery of an
unknown execution.

## Scope and request matching

The namespace is one authenticated runtime controller/sandbox and resource kind.
A daemon configured with one access token has one authenticated principal. Users
sharing that token share that trust boundary; `uid`, arbitrary request headers and
trace IDs do not establish a separate tenant. With authentication disabled, all
requests share the anonymous boundary. Tokens are hashed before indexing records.
Another controller, including another sandbox, rejects the instance component.

Commands compare command text or the ordered native `argv` array, cwd, background mode,
timeout, UID, GID and environment overrides. PTYs compare command and cwd. Fields
are normalized through typed JSON; map order and absent/empty env maps are equal.
Strings are compared literally, without shell parsing or whitespace rewriting.
Unknown fields on keyed requests are rejected. The winning command resolves daemon
environment/defaults during launch; retries do not re-read them. PTY environment
and PTY/pipe mode follow the existing first-WebSocket-launch behavior, not the POST
fingerprint; the first launch fixes those choices for that recovered session.

Operation lookups expose no command, arguments, env, stdin, logs, output,
fingerprint or caller identity. Authentication failures on the four operation
endpoints return HTTP 401 with `code: UNAUTHORIZED` and `message`; legacy endpoint
authentication errors keep their existing shape. The 1 MiB body limit is applied
to the input stream before JSON decoding, including trailing data. Operation
lookups are private, not an inventory, and use `Cache-Control: no-store`, including
authentication errors. The existing authenticated `GET /command/status/{id}`
returns command content and the runtime error for both keyed and unkeyed commands.
Those diagnostics can include command arguments and filesystem paths; callers
sharing execd's access token share access to them.

Recovering a command handle does not recover foreground stdout/stderr: foreground
output files are removed when execution finishes. The status error describes a
launch or wait failure (for example, a missing executable or `exit status 7`), not
the program's stderr. Select background mode before creation and use the existing
log cursor when retained output is required. A failed keyed command also retains
its status and launch error under the reserved handle.

## Failure and recovery contract

| Window | Result |
| --- | --- |
| No claim accepted | Lookup returns 404 within this instance/window; the same ID may be submitted. |
| Claimed, not launched | Duplicates return the original reserved handle with `creating`; only the owner launches. |
| Startup failed | Retained `failed`; same identity does not reattempt creation. |
| Started, response lost | Retry/lookup recovers original handle; no second launch. |
| Request cancelled after claim | Creation is controller-owned and continues; lookup with the saved identity. |
| Execution ended | Return the same creation handle while retained; query execution status separately. |
| Same ID, different typed request | 409 `operation_conflict`; no process created by the conflicting request. |
| Execd crash/replacement | 409 `operation_instance_mismatch` on old IDs; previous execution outcome is unknown. |
| Recovery window expired and inactive record removed | 410 `operation_expired`; creation refused. |
| Capacity exhausted | 503 `operation_capacity_exceeded`; no active/unexpired record evicted to admit a new identity. |

Memory registration and OS process creation are not a transaction. A crash can
occur between them. This implementation offers **no cross-execd-restart recovery**:
a fresh random controller instance rejects every old identity, including when the
old child may have survived. Do not replace the instance/time to retry an unknown
outcome. Caller restart within the same daemon/window is the supported scenario.

Identities remain recoverable for 24 hours from their encoded server timestamp,
not from the last retry. Creating and active executions survive that deadline.
The registry holds at most the configured capacity, including creating, active,
successful and failed entries. Lookups and duplicate creates check only their
own record. Capacity pressure and the existing hourly command janitor collect
expired records. PTY lifecycle checks and resource closure run outside the registry
mutex, so cleaning one session does not lock out unrelated recovery requests.
Dormant and terminal keyed PTY sessions expire with their records;
launch and expiration synchronize through the same session lock. Active PTYs are
never evicted for capacity or age. Legacy command status/output cleanup remains
separate (24 hours after completion). Explicit PTY deletion may remove the resource
while the creation record still returns its old handle; it never recreates it.

The timestamp in an identity lets the server reject an expired identity after
removing its record, without keeping unbounded tombstones. This is why recovering
with a new timestamp is unsafe. A controller stuck after claiming but before
finishing creation retains `creating` until its lifecycle ends; it does not guess
that launching again is safe.

### Capacity and observability

Set `--operation-capacity` or `EXECD_OPERATION_CAPACITY` to a positive integer
(default 4096); the flag overrides the environment. Configure capacity at startup,
before accepting operations. Instance discovery reports the actual value.
Keep room for the expected new-operation rate multiplied by the 24-hour window,
plus creating or active executions that outlive it. Successful short commands
consume retained slots too. A full registry rejects new identities with 503;
existing identities remain recoverable. Failed records retain the same result
for the full window, rather than becoming permission to try the same identity again.

Existing OpenTelemetry export includes `execd.operation.requests` by `kind`,
`action` and `result`; `execd.operation.records` by `kind` and `state`;
`execd.operation.capacity`; and `execd.operation.creating.oldest_age` in seconds.
The latter reports age, not a determination that a launch is safe to retry.
These metrics contain no operation IDs, handles, principals or request content.

## PTY connections

`POST /pty` creates a dormant session; it does not start a shell. After recovering
`id`, connect to `/pty/{id}/ws`. Existing exclusive connection locking, `since`
replay and `takeover=1` apply. A keyed session permits one launch attempt. After
process exit, reconnect replays the retained output and terminal event rather than
starting another shell. A failed launch is not retried by reconnecting. Unkeyed
sessions preserve their previous behavior.

`GET /pty/{id}` returns `launch_attempted` and `launch_failed` alongside `running`:

| Launch attempted | Launch failed | Running | Meaning |
| --- | --- | --- | --- |
| false | false | false | Dormant session; no launch attempt. |
| true | true | false | The launch attempt failed; a keyed session cannot launch again. |
| true | false | true | The process started and is running. |
| true | false | false | The process started and then exited; replay/terminal reconnect applies. |

A nonzero process exit is not a launch failure. For unkeyed sessions, the flags
describe the latest launch attempt, including any legacy restart. Older servers
omit these fields; absence means the launch outcome is unavailable, not false.
The PTY operation stays `created` after a WebSocket launch failure because the
session was created; its launch outcome belongs to the session status. This lets
a caller recover the outcome even if the `start_failed` WebSocket frame was lost.

The keyed `POST /pty/operations` claims its record before scheduling directory
and session creation in the background. A `202 creating` response may precede
session availability; wait for `created` before attaching. Filesystem failure
becomes a retained `failed` creation. Both command and PTY creation are owned by
the runtime after the claim, independently of the originating HTTP connection.

## Guarantee

The guarantee is at-most-once creation for the saved identity within the described
controller and recovery scope. It does not guarantee command completion, successful
scripts, or exactly-once arbitrary business side effects. Command inventory
[#1309](https://github.com/opensandbox-group/OpenSandbox/pull/1309) and SSE recovery
[#507](https://github.com/opensandbox-group/OpenSandbox/issues/507) address different
stages and are not assumed to have landed in this baseline.

The separate create paths also fail safely against older execd versions: unsupported
paths return an error instead of silently ignoring a new field and executing. They
avoid changing the generated Kotlin return type of the existing `runCommand` API.
Do not send `operation_id` to the legacy paths and assume recovery is enabled.
