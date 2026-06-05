# opensandbox-supervisor

A lightweight process supervisor that wraps a single worker with restart backoff, lifecycle hooks, a crashloop circuit breaker, and a structured event log. Designed to run as a container `ENTRYPOINT` or as a child of another process; it does not assume PID 1 and performs no zombie reaping.

## Usage

```
opensandbox-supervisor [flags] -- <worker-cmd> [worker-args...]
```

Everything after `--` is the worker command. The supervisor starts the worker, monitors it, and restarts it on unexpected exits.

### Example (egress sidecar)

```dockerfile
ENTRYPOINT ["/opt/opensandbox-egress/supervisor", \
            "--pre-start=/opt/opensandbox-egress/cleanup.sh", \
            "--name=egress", \
            "--grace-period=20s", \
            "--", \
            "/opt/opensandbox-egress/egress"]
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--pre-start` | _(none)_ | Executable to run before each worker launch (repeatable). No shell expansion; wrap in a script if needed. |
| `--post-exit` | _(none)_ | Executable to run after each worker exit (repeatable). Receives `WORKER_*` env vars. Failures are logged, not fatal. |
| `--event-log` | stderr | Path to JSONL event log file. Supports rotation via lumberjack. |
| `--backoff-min` | `1s` | Minimum restart backoff. |
| `--backoff-max` | `30s` | Maximum restart backoff (exponential growth capped here). |
| `--backoff-jitter` | `0.1` | Jitter fraction (±10%). Set to `0` to disable. |
| `--stable-after` | `60s` | Worker uptime after which backoff resets to minimum. |
| `--burst-window` | `5m` | Sliding window for crashloop detection. |
| `--burst-max` | `10` | Maximum launches allowed within `burst-window` before the breaker trips. |
| `--on-burst-exit` | `true` | `true`: supervisor exits non-zero when burst budget trips (lets kubelet react). `false`: keep retrying indefinitely. |
| `--grace-period` | `10s` | Time between SIGTERM and SIGKILL when shutting the worker down. |
| `--pre-start-timeout` | `30s` | Timeout for each pre-start hook execution. |
| `--post-exit-timeout` | `30s` | Timeout for each post-exit hook execution. |
| `--name` | _(basename of worker cmd)_ | Worker name shown in logs and events. |
| `--log-level` | `info` | Supervisor diagnostic log level (`debug`\|`info`\|`warn`\|`error`). |

## Restart Behavior

### Exponential Backoff

When the worker exits unexpectedly, the supervisor sleeps before restarting:

```
1s → 2s → 4s → 8s → 16s → 30s → 30s → ...
```

Each delay is perturbed by ±`backoff-jitter` (default ±10%) to avoid thundering herds. After the worker has been alive at least `stable-after` (default 60 s), the backoff resets to `backoff-min`.

### Crashloop Circuit Breaker

A sliding-window counter tracks launches. If more than `burst-max` (default 10) launches occur within `burst-window` (default 5 min), the supervisor either:

- **Exits non-zero** (`--on-burst-exit=true`, default) — surfacing the crashloop via Kubernetes pod status instead of silently retrying.
- **Continues retrying** (`--on-burst-exit=false`) — for environments without an outer restart supervisor.

## Lifecycle Hooks

### Pre-start hooks

Run **before each worker launch**. A non-zero exit aborts that launch attempt and counts toward the crashloop budget. Use for cleanup tasks like reaping orphaned child processes from a previous crash.

### Post-exit hooks

Run **after the worker has been reaped**. Failures are logged but do not block the restart loop. Post-exit hooks run to completion even during shutdown (bounded by `--post-exit-timeout`) so cleanup paths are not aborted.

Post-exit hooks receive these environment variables:

| Variable | Description |
|----------|-------------|
| `WORKER_EXIT_CODE` | Worker's exit code (`-1` if not available) |
| `WORKER_SIGNAL` | Signal name if worker was signaled (e.g. `terminated`, `killed`) |
| `WORKER_DURATION_MS` | Wall-clock worker runtime in milliseconds |
| `WORKER_PID` | Worker's PID |
| `WORKER_ATTEMPT` | Launch attempt number (1-based) |

## Graceful Shutdown

On context cancellation (typically from `SIGTERM` or `SIGINT`):

1. Supervisor sends `SIGTERM` to the worker.
2. Waits up to `--grace-period` for the worker to exit on its own.
3. Sends `SIGKILL` if the worker does not exit in time.

### Signal Handling

- The supervisor does **not** install `signal.Notify` itself; the caller (e.g. `cmd/supervisor/main.go`) translates OS signals into context cancellation.
- `SIGINT` and `SIGTERM` both result in `SIGTERM` to the worker.
- Other signals (`SIGHUP`, `SIGUSR1`, etc.) are **not forwarded**. Add forwarding in the caller if the worker needs them.

### Process Group Isolation

The worker is started with `Setpgid=true` on Unix so signals delivered to the supervisor's process group do not reach the worker by side channel. The supervisor signals the worker explicitly via its PID.

## Structured Event Log

One JSONL record per lifecycle event, written to stderr by default or to the file specified by `--event-log` (with automatic rotation).

### Event Kinds

| Event | When | Key Fields |
|-------|------|------------|
| `start` | Worker process launched | `pid`, `gen`, `attempt` |
| `exit` | Worker exited | `pid`, `gen`, `attempt`, `exit_code`, `signal`, `duration_ms`, `reason` |
| `prestart` | Pre-start hook ran | `hook`, `exit_code`, `duration_ms` |
| `postexit` | Post-exit hook ran | `hook`, `exit_code`, `duration_ms` |
| `backoff` | Sleeping before next restart | `sleep_ms`, `next_attempt` |
| `stable` | Worker uptime exceeded `stable-after`; backoff reset | `pid`, `gen`, `duration_ms`, `reset_backoff` |
| `burst_exit` | Crashloop budget exceeded | `attempts`, `window` |
| `shutdown` | Supervisor shutting down | `reason` |

### Example Events

```jsonl
{"ts":"2026-01-15T10:30:00Z","name":"egress","event":"start","pid":42,"gen":1,"attempt":1}
{"ts":"2026-01-15T10:30:00.15Z","name":"egress","event":"exit","pid":42,"gen":1,"attempt":1,"exit_code":1,"duration_ms":150,"reason":"crashed"}
{"ts":"2026-01-15T10:30:00.15Z","name":"egress","event":"backoff","sleep_ms":1000,"next_attempt":2}
{"ts":"2026-01-15T10:30:01.15Z","name":"egress","event":"prestart","hook":"cleanup.sh","exit_code":0,"duration_ms":50}
{"ts":"2026-01-15T10:30:01.2Z","name":"egress","event":"start","pid":43,"gen":2,"attempt":2}
```

### Exit Reasons

| Reason | Meaning |
|--------|---------|
| `exited` | Worker exited with code 0 |
| `crashed` | Worker exited with non-zero code |
| `signaled` | Worker killed by signal |
| `shutdown` | Supervisor-initiated stop (context cancelled) |
| `launch_failed` | Worker binary could not be started |
| `no_processstate` | Unexpected: no process state available |

## Library Usage

The `internal/supervisor` package can be used programmatically:

```go
import "github.com/alibaba/opensandbox/internal/supervisor"

spec := supervisor.Spec{
    Name:        "my-worker",
    Cmd:         "/usr/local/bin/worker",
    Args:        []string{"--config", "/etc/worker.toml"},
    PreStart:    []supervisor.Hook{{Argv: []string{"/usr/local/bin/cleanup.sh"}}},
    BackoffMin:  time.Second,
    BackoffMax:  30 * time.Second,
    GracePeriod: 15 * time.Second,
}

ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer cancel()

err := supervisor.Run(ctx, spec)
```

`Run` blocks until context cancellation or `ErrBurstExceeded`. Zero-valued fields receive sensible defaults (see Flags table above for values).
