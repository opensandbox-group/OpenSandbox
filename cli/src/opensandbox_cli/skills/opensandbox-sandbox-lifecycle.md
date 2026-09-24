---
name: sandbox-lifecycle
description: Use OpenSandbox CLI lifecycle commands to create, inspect, verify, renew, pause, resume, expose, and terminate sandboxes. Also covers template (golden-image) builds and snapshot management. Trigger when users want help provisioning a sandbox, choosing create flags, creating from a template or snapshot, managing templates or snapshots, checking runtime state, retrieving endpoints, or safely cleaning up sandboxes.
---

# OpenSandbox Sandbox Lifecycle

Use OpenSandbox lifecycle commands directly instead of giving generic container advice. Prefer a verified lifecycle flow over isolated commands.

## When To Use

- the user wants to create a sandbox for a task or workflow
- the user wants to create a sandbox from a template (golden image) or restore a snapshot
- the user wants to build, inspect, list, or delete templates
- the user needs to inspect sandbox state or health
- the user wants to expose a service port through an OpenSandbox endpoint
- the user wants to renew, pause, resume, or terminate a sandbox
- the user is unsure which create flags or file-based inputs to use

## Configuration Resolution

Before giving lifecycle commands, resolve the active OpenSandbox connection configuration:

```bash
osb config show -o json
```

Check the resolved values for:

- `domain`
- `api_key`
- `protocol`
- `use_server_proxy` when endpoint routing matters

`osb config show` redacts the API key. Use it to confirm which server and protocol the CLI will target, not to recover credentials.

Do not focus only on environment variables. `osb config show` already reflects the effective configuration after applying CLI flags, environment variables, config file values, and defaults.

Hard stop:

```bash
osb config show -o json
```

If `domain` is missing, stop and set it before lifecycle commands:

```bash
osb config set connection.domain <host:port> -o json
osb config show -o json
```

If auth is required and `api_key` is missing, stop and set it before lifecycle commands:

```bash
osb config set connection.api_key <api-key> -o json
osb config show -o json
```

If the effective target is clear, then continue with lifecycle commands.

## Preflight Checks

Run these checks before lifecycle commands:

```bash
osb --version
osb config show -o json
```

If `osb` is missing, try a project-local entrypoint:

```bash
.venv/bin/osb --version
.venv/bin/osb config show -o json
```

Use the results to classify the situation:

- CLI available
- config resolves
- target looks plausible for the current environment

## Golden Path

Use this as the default lifecycle flow unless the user asks for a narrower action:

```bash
osb config show -o json
osb sandbox create --image python:3.12 --timeout 30m -o json
osb sandbox get <sandbox-id> -o json
osb sandbox health <sandbox-id> -o json
```

If the sandbox is intended to serve traffic on a known port, continue with:

```bash
osb sandbox endpoint <sandbox-id> --port 8080 -o json
```

This sequence is safer than stopping at `sandbox get`, because `get` confirms object state while `health` confirms the sandbox is actually reachable through the execd health path.

## Create Options

Start with the narrowest create command that matches the request:

```bash
osb sandbox create --image python:3.12 -o json
osb sandbox create --image node:20 --timeout 30m -o json
osb sandbox create --image my-registry.example.com/team/app:latest --image-auth-username alice --image-auth-password <token> -o json
osb sandbox create --image python:3.12 --timeout none -o json
osb sandbox create --image python:3.12 --entrypoint python --entrypoint -m --entrypoint http.server -o json
osb sandbox create --image python:3.12 --extension storage.id=abc123 -o json
osb sandbox create --image python:3.12 --ready-timeout 90s -o json
osb sandbox create --image python:3.12 --network-policy-file network-policy.json -o json
osb sandbox create --image python:3.12 --network-policy-file network-policy.json --credential-proxy -o json
osb sandbox create --image python:3.12 --volumes-file volumes.json -o json
```

Use these options deliberately:

- `--image`: required unless the CLI already has `defaults.image` configured
- `--image-auth-username` and `--image-auth-password`: use together when the image is in a private registry
- `--timeout`: recommended for most temporary workloads so sandboxes do not linger indefinitely
- `--timeout none`: disable automatic expiration and switch the sandbox to manual cleanup mode
- omit `--timeout`: use `defaults.timeout` when configured; otherwise the request falls back to the SDK/server default TTL
- `--entrypoint`: repeat the flag once per argv item; do not use JSON or shell-wrapped command strings
- `--extension`: repeat for opaque extension key-value pairs that should be passed through as-is
- `--ready-timeout`: increase this when the image or workload needs more startup time
- `--skip-health-check`: use only when the user explicitly wants object creation without waiting for readiness; do not use it to mask startup problems
- `--credential-proxy`: enable Credential Vault transparent proxy support; requires `--network-policy-file`
- `--template <tpl-id>`: create from a `Succeeded` template; mutually exclusive with `--image` and `--snapshot-id`; requires an explicit `--timeout`
- `--snapshot-id <snap-id>`: restore a sandbox from a snapshot; mutually exclusive with `--image` and `--template`

If the user does not specify an image, recommend one that matches the runtime they need instead of guessing silently.

## Templates And Snapshots

Templates are golden-image builds. The build is asynchronous: `template create` returns a template in `Pending`; poll `template get` until the phase is `Succeeded` (or `Failed`).

```bash
osb template create --image python:3.12 --publish s3://bucket/publish -o json
osb template get <template-id> -o json
osb template list -o json
osb template delete <template-id> -o json
```

Useful `template create` options: `--resource cpu=1 memory=512Mi disk=2Gi`, `--entrypoint` (repeat per argv item), `--env KEY=VALUE`, `--metadata KEY=VALUE`, `--format native|overlaybd`, `--readiness-probe tcp://127.0.0.1:44772`, `--warmup-seconds 60`.

Create a sandbox from a succeeded template:

```bash
osb sandbox create --template <template-id> --timeout 30m -o json
```

Template mode fixes the workload shape on the server: `--image`, `--env`, `--resource`, `--entrypoint`, `--volumes-file`, and `--credential-proxy` cannot be combined with `--template`. Only `--timeout` (required), `--metadata`, `--extension`, and `--network-policy-file` may accompany it.

Snapshots capture a running sandbox's memory and disk state and can back new sandboxes:

```bash
osb snapshot create <sandbox-id> --name golden -o json
osb snapshot get <snapshot-id> -o json
osb snapshot list --sandbox-id <sandbox-id> -o json
osb snapshot delete <snapshot-id> -o json
osb sandbox create --snapshot-id <snapshot-id> --timeout 30m -o json
```

Template-based creation requires a Kubernetes-backed fast-sandbox runtime; snapshot flows depend on the runtime supporting checkpoint/restore.

## JSON Shapes

When recommending `--network-policy-file` or `--volumes-file`, always show the JSON shape instead of assuming the user knows it.

Example `network-policy.json`:

```json
{
  "defaultAction": "deny",
  "egress": [
    {
      "action": "allow",
      "target": "pypi.org"
    },
    {
      "action": "allow",
      "target": "files.pythonhosted.org"
    }
  ]
}
```

Example `volumes-host.json`:

```json
[
  {
    "name": "workspace-data",
    "host": {
      "path": "/tmp/opensandbox-data"
    },
    "mountPath": "/workspace/data",
    "readOnly": false
  }
]
```

Example `volumes-pvc.json`:

```json
[
  {
    "name": "shared-models",
    "pvc": {
      "claimName": "shared-models-pvc"
    },
    "mountPath": "/workspace/models",
    "readOnly": true
  }
]
```

Prefer `pvc` when the environment supports it and the user needs a more portable storage definition. Use `host` when the user explicitly needs a host-path bind mount and the server has been configured to allow that path.

## Verification

Use verification commands in this order:

```bash
osb sandbox get <sandbox-id> -o json
osb sandbox health <sandbox-id> -o json
osb sandbox metrics <sandbox-id> -o json
osb sandbox metrics <sandbox-id> --watch -o raw
```

Use:

- `sandbox get` to inspect the current lifecycle state and metadata
- `sandbox health` to confirm the sandbox is usable
- `sandbox metrics` for a point-in-time resource snapshot
- `sandbox metrics --watch` when the user wants live CPU and memory updates while diagnosing load or pressure

If the user needs a public or routed port, verify it explicitly:

```bash
osb sandbox endpoint <sandbox-id> --port 8080 -o json
```

## Lifecycle Actions

Use these commands for ongoing lifecycle management:

```bash
osb sandbox list -o json
osb sandbox list --state running --state paused -o json
osb sandbox renew <sandbox-id> --timeout 30m -o json
osb sandbox pause <sandbox-id> -o json
osb sandbox resume <sandbox-id> -o json
osb sandbox kill <sandbox-id> -o json
```

Rules:

- use `sandbox list` for discovery or filtering, not single-sandbox verification
- `sandbox list --state` accepts known lifecycle states case-insensitively
- use `renew` before long-running work instead of waiting for expiry
- use `pause` only when the workload can tolerate suspension
- treat the pause result as request acceptance and poll `sandbox get` until the state is `Paused` or `Failed`
- use `kill` when cleanup is the real goal; do not leave orphaned sandboxes behind

## Runtime Notes

- `renew` resets the expiration to approximately `now + timeout`; treat it as a fresh TTL, not a simple additive extension to the old timestamp
- `create --timeout none` means no automatic expiration; cleanup becomes an explicit `kill` responsibility
- `create` without `--timeout` does not mean manual cleanup; it uses `defaults.timeout` first and otherwise leaves TTL selection to the SDK/server default
- `pause` is asynchronous; `Pause request accepted` does not mean the sandbox is already paused
- `pause` and `resume` may depend on the underlying runtime; if the runtime does not support them, avoid promising they will work
- only templates in the `Succeeded` phase can back `sandbox create --template`; poll `template get` while the build is `Pending`/`Building`
- `sandbox create --template` requires an explicit finite `--timeout`; `--timeout none` is rejected in template mode
- host-path volumes depend on server-side allowed host path configuration
- if creation fails or the sandbox never becomes healthy, switch to `sandbox-troubleshooting` instead of adding more create flags blindly

## Response Pattern

Structure the answer as:

1. exact command to run
2. what state change or verification result to expect
3. the next lifecycle command if the workflow continues

Keep command examples concrete and ready to paste.

## Minimal Closed Loops

Create and verify readiness:

```bash
osb sandbox create --image python:3.12 --timeout 30m -o json
osb sandbox get <sandbox-id> -o json
osb sandbox health <sandbox-id> -o json
```

Create a service sandbox and retrieve its endpoint:

```bash
osb sandbox create --image python:3.12 --timeout 30m -o json
osb sandbox health <sandbox-id> -o json
osb sandbox endpoint <sandbox-id> --port 8080 -o json
```

Create with network policy:

```bash
osb sandbox create --image python:3.12 --network-policy-file network-policy.json -o json
osb sandbox get <sandbox-id> -o json
osb sandbox health <sandbox-id> -o json
```

Build a template, wait for it, and create from it:

```bash
osb template create --image python:3.12 --publish s3://bucket/publish -o json
osb template get <template-id> -o json
osb sandbox create --template <template-id> --timeout 30m -o json
osb sandbox health <sandbox-id> -o json
```

Snapshot a sandbox and restore it:

```bash
osb snapshot create <sandbox-id> --name golden -o json
osb sandbox create --snapshot-id <snapshot-id> --timeout 30m -o json
osb sandbox health <sandbox-id> -o json
```

Renew before long work:

```bash
osb sandbox renew <sandbox-id> --timeout 30m -o json
osb sandbox get <sandbox-id> -o json
```

Pause and confirm state:

```bash
osb sandbox pause <sandbox-id> -o json
osb sandbox get <sandbox-id> -o json
```

Resume and verify health:

```bash
osb sandbox resume <sandbox-id> -o json
osb sandbox health <sandbox-id> -o json
```

## Best Practices

- resolve the active connection configuration before assuming which server the command will hit
- prefer `osb config show` over checking individual environment variables in isolation
- confirm whether the user wants to keep, override, or persist connection settings before changing them

- Prefer explicit `--image` and `--timeout` when demonstrating commands
- Prefer `health` over assuming readiness from `create` output alone
- Prefer `endpoint` over telling the user to guess host/port mappings
- Prefer `pvc` over `host` when portability matters
- Prefer troubleshooting over `--skip-health-check` when startup is failing
