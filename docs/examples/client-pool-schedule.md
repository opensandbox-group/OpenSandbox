---
title: Scheduled Client Pool Sizing
description: Adjust a Go SandboxPool's idle target during business hours using an application-level controller and Resize.
---

# Scheduled Client Pool Sizing

Keep more sandboxes ready during business hours and reduce idle capacity at night
or on weekends. This Go example evaluates a small application-owned schedule and
calls the existing `SandboxPool.Resize` API. The SDK's reconciler handles creating
and removing idle sandboxes.

The schedule uses **Monday–Friday, 09:00 inclusive to 21:00 exclusive**, in an IANA
time zone such as `Asia/Shanghai` or `America/New_York`. Its default targets are
five idle sandboxes during business hours and zero outside them. Edit
`desiredMaxIdle` in the example to implement your own demand or budget policy.

Source: [`examples/client-pool-schedule/`](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/client-pool-schedule).

## Run

Install Go 1.20 or newer, check out this repository, and start an
[OpenSandbox server](/getting-started/). The example's Go module uses the SDK from
the same checkout through a local `replace` directive.

```bash
cd examples/client-pool-schedule
export OPEN_SANDBOX_DOMAIN=localhost:8080
export OPEN_SANDBOX_API_KEY=your-api-key
go run . -timezone Asia/Shanghai -peak-idle 5 -offpeak-idle 0
```

Use `OPEN_SANDBOX_PROTOCOL=https` for an HTTPS server. The example routes sandbox
health checks through the server proxy. It uses `ubuntu:22.04` with an entrypoint
that keeps the sandbox alive; use `-image` to select another compatible image.
Run `go run . -h` for all flags.

::: warning Sandbox resources
This command creates real sandboxes when the scheduled target is positive. Use
small targets while trying it out. Ctrl+C stops reconciliation, then drains and
kills the example's remaining idle sandboxes. Sandboxes already acquired by your
application remain the caller's responsibility. Cleanup is best effort; idle
sandboxes also have a finite TTL.
:::

## How resizing works

- The pool starts with the target for the current time, avoiding an initial
  business-hours warmup when started at night.
- Every 30 seconds, the controller compares the desired target with
  `Snapshot().MaxIdle`. It calls `Resize` only when those targets differ, so a
  temporary idle deficit does not trigger repeated resize calls.
- Target updates are asynchronous. The reconcile interval, sandbox startup and
  readiness time, and warmup concurrency determine how quickly capacity converges.
  Move the business-hours start earlier if capacity must be ready before traffic
  arrives.
- Scale-down removes surplus **idle** sandboxes. Acquired sandboxes are outside the
  idle buffer. A target of zero stops replenishment; it does not disable direct
  creation on acquire or act as a total sandbox quota.
- The time zone is evaluated on each check, including daylight-saving changes.
  The example embeds Go's timezone database so it also works on hosts without
  system zoneinfo files.
- A snapshot or resize error stops the example and runs cleanup. It does not call
  `Start` or clear namespace-destroy state to recover from an error.

## Multiple application processes

This runnable example uses an **in-memory store**, so its pool belongs to one
process. For a distributed pool, configure every participant with the same Redis
store and `pool_name`, and a unique `owner_id`; see the
[client pool guide](/guides/client-pool).

Run only one application-level scheduling controller for each shared namespace,
or elect one in your application. `Resize` updates the shared target, so you do
not need a cron job on every worker. The pool's warmup leader election does not
elect a leader for your external scheduling policy. Avoid competing manual and
scheduled writers: this example reapplies the scheduled target on the next check.
Coordinate worker startup and restarts with the controller, because starting a
pool can write its configured initial target to the store.

The example's shutdown drain is intended for its single-process pool. Do not copy
that drain into the shutdown of one worker of a shared pool: it would drain the
shared idle buffer while other workers may still replenish it. Use the documented
namespace retirement procedure when retiring an entire distributed pool.

## Test without a server

```bash
go test ./...
go vet ./...
```

Tests cover weekday and weekend boundaries, time-zone conversion, daylight-saving
transitions, and target updates through the real SDK's in-memory state store.
