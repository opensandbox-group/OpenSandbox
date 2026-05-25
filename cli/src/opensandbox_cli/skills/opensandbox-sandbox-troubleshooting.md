---
name: sandbox-troubleshooting
description: Use OpenSandbox CLI state, health, and stable diagnostics logs/events to investigate failed, unhealthy, or unreachable sandboxes. Trigger when users report startup failures, crashes, OOM, image pull problems, pending sandboxes, network issues, or an unresponsive sandbox and want root cause plus next actions.
---

# OpenSandbox Sandbox Troubleshooting

Investigate the reported sandbox before proposing a fix. Prefer evidence from OpenSandbox state, health checks, and stable diagnostics streams over speculation.

## Inputs To Collect

Capture these from the user request or surrounding context before running commands:

- sandbox ID or an unambiguous short ID
- whether `osb` or `opensandbox` CLI is available locally
- any reported symptom: pending forever, crash, OOM, unreachable service, bad image, failed exec, etc.

If the sandbox ID is missing, ask for it first.

## Configuration Resolution

Before troubleshooting a sandbox, resolve the active OpenSandbox connection configuration:

```bash
osb config show -o json
```

Check the resolved values for:

- `domain`
- `api_key`
- `protocol`
- `use_server_proxy` when endpoint routing or proxy behavior may matter

`osb config show` redacts the API key. Use it to confirm the effective target server and protocol before troubleshooting.

If `domain` is missing, stop and set it first:

```bash
osb config set connection.domain <host:port> -o json
osb config show -o json
```

If auth is required and `api_key` is missing, stop and set it first:

```bash
osb config set connection.api_key <api-key> -o json
osb config show -o json
```

Use raw HTTP only after domain, protocol, and API key expectations are explicit.

## Operating Rules

- start with the highest-signal commands first: sandbox state, sandbox health, then stable diagnostics events/logs
- use CLI commands when `osb` is available because they are shorter and usually already authenticated
- use HTTP only when the CLI is unavailable or the user is clearly working from raw API access
- distinguish observed facts from inference and quote the field, event, or log line that supports the diagnosis
- separate sandbox/runtime failures from workload/application failures before suggesting a fix
- do not paper over readiness problems with `--skip-health-check`
- end with a likely root cause and 1-3 concrete remediation steps

## Triage Order

Use this order by default:

```bash
osb sandbox get <sandbox-id> -o json
osb sandbox health <sandbox-id> -o json
osb diagnostics events <sandbox-id> --scope lifecycle -o raw
osb diagnostics events <sandbox-id> --scope runtime -o raw
osb diagnostics logs <sandbox-id> --scope container -o raw
```

Then drill down only where the stable diagnostics point:

```bash
osb diagnostics logs <sandbox-id> --scope lifecycle -o raw
osb diagnostics events <sandbox-id> --scope all -o raw
osb diagnostics logs <sandbox-id> --scope all -o raw
```

## Diagnostics Streams

Important properties of the diagnostics commands:

- `diagnostics events` and `diagnostics logs` are stable API-backed commands
- `--scope` is required for stable diagnostics; requests without scope use deprecated plain-text DevOps behavior
- if the server returns `DIAGNOSTICS_NOT_IMPLEMENTED`, state that stable diagnostics are unavailable on this server and stop diagnostics collection
- use known supported scopes first: `events:lifecycle`, `events:runtime`, `logs:lifecycle`, and `logs:container`
- `--scope all` is useful when the server supports aggregate diagnostics; if it is empty, retry concrete supported scopes
- other scopes such as `network` or `process` are server-defined and may be empty on some deployments
- `-o raw` prints inline diagnostic text directly, or a content URL when the server returns URL delivery
- `-o json` / `-o yaml` prints the CLI descriptor including `delivery`, `content_url`, `expires_at`, `truncated`, and `warnings`
- quote concrete lines from diagnostics output instead of summarizing vaguely

Use:

- `osb diagnostics events <sandbox-id> --scope lifecycle -o raw` for sandbox actions such as `CREATE`, `RENEW`, `DELETE`, `PAUSE`, `RESUME`, and `FORK`
- `osb diagnostics events <sandbox-id> --scope runtime -o raw` for scheduler and container events such as `Scheduled`, `Pulling`, `Pulled`, `Created`, `Started`, and `ContainerDied`
- `osb diagnostics logs <sandbox-id> --scope lifecycle -o raw` for manager server logs related to create, renew, delete, callbacks, request IDs, and server-side failures
- `osb diagnostics logs <sandbox-id> --scope container -o raw` for sandbox main-process stdout, including application errors, missing binaries, bad entrypoints, startup hangs, and health-check failures

## Evidence Semantics

- `sandbox get` shows control-plane state; it does not prove the workload is healthy
- `sandbox health` shows readiness or endpoint health; it can fail even when the sandbox is running
- lifecycle diagnostics explain OpenSandbox manager behavior; container logs explain the user workload
- runtime events are platform facts and usually outrank application logs for scheduling, image pull, restart, and kill reasons
- empty diagnostics do not prove there is no issue; the scope may be unsupported, expired, or outside retention
- `truncated: true` means the evidence is incomplete; lower confidence and mention the truncation
- always read `warnings`; they may explain missing, partial, unsupported, or expired diagnostic content

## URL Delivery

- with `delivery: inline`, use `content` as the diagnostic text
- with `delivery: url`, `-o raw` prints the diagnostic content URL; fetch it only if you need the diagnostic body
- `content_url` in structured CLI output is a diagnostic artifact URL, not a sandbox service endpoint
- check `expires_at`; container log URLs may expire quickly, so request diagnostics again if the URL is stale
- do not forward diagnostic URLs or lifecycle logs to unrelated people because they may contain sensitive troubleshooting data

## Symptom To Command Mapping

Use the first command that best matches the reported symptom:

| Symptom | First command | What to confirm next |
| --- | --- | --- |
| pending forever or stuck creating | `osb diagnostics events <sandbox-id> --scope runtime -o raw` | image pull errors, scheduling failures, admission errors, then lifecycle logs |
| image pull failure | `osb diagnostics events <sandbox-id> --scope runtime -o raw` | image name, tag, registry auth |
| crash loop or repeated restarts | `osb diagnostics logs <sandbox-id> --scope container -o raw` | `osb diagnostics events <sandbox-id> --scope runtime -o raw` for restarts or kill signals |
| suspected OOM or exit code issue | `osb diagnostics events <sandbox-id> --scope runtime -o raw` | kill signals, restart events, resource pressure messages |
| endpoint unreachable or connection refused | `osb sandbox health <sandbox-id> -o json` | `osb sandbox endpoint <sandbox-id> --port <port> -o json` and then `osb diagnostics logs <sandbox-id> --scope container -o raw` |
| outbound network access failure | `osb sandbox health <sandbox-id> -o json` | check service behavior, then switch to `network-egress` if the issue is egress policy related |

## Diagnosis Playbooks

### Image Pull Failure

- first evidence: `events` shows `ImagePullBackOff`, `ErrImagePull`, or auth failures
- confirming evidence: sandbox stays `Pending` or never reaches healthy state
- likely cause: bad image reference or missing registry credentials
- next actions: verify image URI and tag, fix registry auth, recreate the sandbox

### OOM Kill

- first evidence: runtime events mention container killed due to out-of-memory or resource pressure
- confirming evidence: sandbox becomes unhealthy, restarts, or exits after memory-intensive work
- likely cause: memory limit too low for the workload
- next actions: increase memory, rerun the workload, compare peak workload memory with the configured limit

### Crash Loop Or Bad Entrypoint

- first evidence: `logs` show startup exceptions, missing binaries, or permission errors
- confirming evidence: runtime events show repeated restarts, failed starts, or container exit reasons
- likely cause: bad entrypoint, missing executable, or application crash on boot
- next actions: fix the command or image contents, correct file permissions, redeploy or recreate

### Endpoint Or Service Unreachable

- first evidence: sandbox is `Running` but client requests fail or connection is refused
- confirming evidence: `sandbox endpoint <id> --port <port>` is missing, wrong, or points to a service that is not listening
- likely cause: wrong exposed port, service not bound, or server endpoint host misconfiguration
- next actions: verify the port, inspect service logs, and if the endpoint host is unreachable from the client environment check the server endpoint configuration

## Minimal Closed Loops

CLI-first troubleshooting:

```bash
osb sandbox get <sandbox-id> -o json
osb sandbox health <sandbox-id> -o json
osb diagnostics events <sandbox-id> --scope lifecycle -o raw
osb diagnostics events <sandbox-id> --scope runtime -o raw
osb diagnostics logs <sandbox-id> --scope container -o raw
```

Crash-focused investigation:

```bash
osb diagnostics logs <sandbox-id> --scope container -o raw
osb diagnostics events <sandbox-id> --scope runtime -o raw
```

Endpoint troubleshooting:

```bash
osb sandbox get <sandbox-id> -o json
osb sandbox health <sandbox-id> -o json
osb sandbox endpoint <sandbox-id> --port <port> -o json
osb diagnostics logs <sandbox-id> --scope container -o raw
```

## Response Format

Structure the answer in this order:

1. current state: what the sandbox is doing now
2. evidence: the command output that matters
3. root cause: the most likely diagnosis, stated as confidence not certainty when needed
4. next actions: specific fixes or follow-up checks

Keep the conclusion compact.
