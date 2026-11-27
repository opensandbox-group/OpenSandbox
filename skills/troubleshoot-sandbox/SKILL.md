---
name: troubleshoot-sandbox
description: Troubleshoot OpenSandbox issues by running diagnostics (context, logs, events) via CLI or HTTP API to diagnose sandbox failures like OOM, crash, image pull errors, network problems, etc.
user-invocable: true
argument-hint: "[sandbox-id]"
---

# OpenSandbox Troubleshooting

Troubleshoot sandbox $ARGUMENTS using the opensandbox diagnostics.

There are two ways to interact with the diagnostics API: **CLI** (if opensandbox CLI is installed) or **HTTP** (curl against the server directly). Use whichever is available. The HTTP approach works regardless of how the sandbox was created (SDK, API, CLI).

## Workflow

### Step 1: Confirm sandbox state

**CLI:**
```bash
opensandbox sandbox get <sandbox-id>
```

**HTTP:**
```bash
curl http://<server-domain>/v1/sandboxes/<sandbox-id>
```

If the server requires authentication, add `-H "OPEN-SANDBOX-API-KEY: <your-key>"` to all curl commands.

Check the sandbox status (Running, Pending, Paused, Failed, etc.). If the sandbox is not found, it may have been deleted or expired.

### Step 2: Collect diagnostics (recommended first action)

**CLI:**
```bash
opensandbox diagnostics context <sandbox-id> -o json
opensandbox diagnostics events <sandbox-id> --scope runtime -o raw
opensandbox diagnostics logs <sandbox-id> --scope container -o raw
```

**HTTP:**
```bash
curl http://<server-domain>/v1/sandboxes/<sandbox-id>/diagnostics/summary
```

The CLI commands return:
- **Context**: backend type, diagnostic identifiers, and current state
- **Events**: state transitions, OOM kills, errors
- **Logs**: recent container output

Read the output carefully and look for common failure patterns listed below.

### Step 3: Drill down if needed

If the scoped output is not enough, use `--scope all` to collect every built-in stream:

**CLI:**
```bash
opensandbox diagnostics logs <sandbox-id> --scope all -o raw
opensandbox diagnostics events <sandbox-id> --scope all -o raw
```

**HTTP:**
```bash
# Get more log lines
curl "http://<server-domain>/v1/sandboxes/<sandbox-id>/diagnostics/logs?tail=500"

# Get logs from recent time window
curl "http://<server-domain>/v1/sandboxes/<sandbox-id>/diagnostics/logs?since=30m"

# Detailed container/pod inspection
curl "http://<server-domain>/v1/sandboxes/<sandbox-id>/diagnostics/inspect"

# More events
curl "http://<server-domain>/v1/sandboxes/<sandbox-id>/diagnostics/events?limit=100"
```

### Step 4: Diagnose common problems

| Symptom | What to check | Likely cause |
|---------|---------------|--------------|
| Status=Pending, no IP | events - look for scheduling/image failures | Image pull failure, insufficient resources, node scheduling |
| OOMKilled=true | events - check OOM kill records | Container exceeded memory limit, increase memory resource |
| Exit Code 137 | events + logs | OOM kill or external SIGKILL |
| Exit Code 1 | logs - check application output | Application error, check entrypoint and env vars |
| Exit Code 126/127 | logs | Entrypoint command not found or not executable |
| Connection refused to sandbox | context + events - check ports and network policy | Service not started inside sandbox, wrong port, network policy blocking |
| Sandbox stuck in Running but unresponsive | logs --scope container | Application hung, check for deadlocks or resource exhaustion |
| execd health check failing | logs - look for execd errors | execd daemon crashed or port conflict |
| ImagePullBackOff (K8s) | events | Wrong image name, missing registry credentials |
| CrashLoopBackOff (K8s) | events + logs | Application keeps crashing, check exit code and logs |

### Step 5: Suggest resolution

Based on the diagnosis, suggest one of:
- **Image issue**: Verify image name, check registry access
- **OOM**: Increase memory limit in sandbox creation (e.g. `memory=4Gi`)
- **Application error**: Fix the entrypoint or application code
- **Network**: Check network policy, verify port configuration
- **Scheduling (K8s)**: Check node resources, check pool availability
- **execd**: Update execd image version, check port conflicts

## API Reference

All diagnostics endpoints return `text/plain` and are available at:

| Endpoint | Query Params | Description |
|----------|-------------|-------------|
| `GET /v1/sandboxes/{id}/diagnostics/summary` | `tail` (default 50), `event_limit` (default 20) | Combined inspect + events + logs |
| `GET /v1/sandboxes/{id}/diagnostics/logs` | `tail` (default 100), `since` (e.g. 10m, 1h) | Container/pod logs |
| `GET /v1/sandboxes/{id}/diagnostics/inspect` | - | Container/pod detailed state |
| `GET /v1/sandboxes/{id}/diagnostics/events` | `limit` (default 50) | Container/pod events |
