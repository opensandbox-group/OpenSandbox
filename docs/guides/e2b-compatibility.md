---
title: E2B Compatibility (Scaffold)
description: Development notes and liveness check for the optional components/e2b-shim module. The module is a scaffold; no E2B operation is implemented or tested.
---

# E2B Compatibility (Scaffold)

`components/e2b-shim` is the optional component proposed by [OSEP-0027](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0027-optional-e2b-compatibility.md). This page describes the current development scaffold only.

::: warning Scaffold only
Gate 0 of OSEP-0027 is pending. The module starts an HTTP server and answers a liveness check. It does not implement any E2B lifecycle, command, or file operation, and it never calls the native OpenSandbox API or envd. No E2B SDK, envd, or Fast Sandbox combination has been tested. Do not treat this module as a deployment recipe or a compatibility commitment.
:::

## What the scaffold does

The binary has two service subcommands that share the same skeleton, plus `version` (build information) and `help`:

| Subcommand | Planned role (not implemented) | Current behavior |
|---|---|---|
| `adapter` | Translate E2B lifecycle requests to the native lifecycle API | Serve `/healthz` |
| `router` | Forward E2B command and file traffic to upstream envd | Serve `/healthz` |

HTTP behavior:

- `GET /healthz` and `HEAD /healthz` return `200`. The check needs no API key and its body contains no configuration. It only reports that the process is running; it does not check the native backend, envd readiness, or E2B compatibility. There is no `/readyz`.
- Other methods on `/healthz` return `405`.
- Every other path returns `404`, including `/`, `/v2/sandboxes`, `/sandboxes`, and paths under `/compat/e2b`.

## Build and run

```bash
cd components/e2b-shim
make build
./bin/e2b-shim --help
./bin/e2b-shim adapter --listen 127.0.0.1:8080
```

Both service commands default to `127.0.0.1:8080`. Set `--listen` to bind a different address. `SIGINT` and `SIGTERM` stop the server with a bounded graceful shutdown.
