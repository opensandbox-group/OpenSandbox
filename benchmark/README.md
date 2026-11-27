# OpenSandbox Benchmark

This directory hosts the standalone OpenSandbox benchmark harness (`opensandbox-bench`) and archived milestone/release baseline reports, as specified in [OSEP-0034](https://github.com/opensandbox-group/OpenSandbox/pull/3065).

## Layout

- `tools/`: Standalone Go module (`cmd/opensandbox-bench`) implementing both the local orchestration CLI and the in-cluster worker subcommands (`opensandbox-bench worker ...`), plus embedded reference benchmark profiles.
- `reports/`: Archived milestone and pre-release baseline reports organized by `<tag-or-date>-<short-commit>/` (`report.json`, `summary.md`, and sampled `traces/`).

## Quick Start

```bash
cd benchmark/tools
make build

# Validate the reference profile
./bin/opensandbox-bench profile validate

# Cross-compile static Linux worker binary
make build-linux

# Run offline comparison between two reports
./bin/opensandbox-bench compare ../reports/<base>/report.json ../reports/<cand>/report.json
```

## Running Benchmarks In-Cluster (`opensandbox-bench run`)

`opensandbox-bench run` uses the Kubernetes API Server as its sole network hop: it launches an isolated, per-invocation Runner Pod in `environment.benchNamespace`, injects a static Linux `opensandbox-bench` binary and profile YAML over `pods/exec` tar streams, executes `worker run` inside the cluster network, and pulls the resulting `run-bundle/` back to `--output`.

```bash
./bin/opensandbox-bench run \
  --kubeconfig ~/.kube/config \
  --profile my-profile.yaml \
  --suite schedule-burst \
  --api-key "$OPEN_SANDBOX_API_KEY" \
  --output ../reports/v1.3.0-abc1234
```

### Prerequisites & RBAC

- A `kubeconfig` with permissions in `environment.benchNamespace` to manage `namespaces` (`get`, `create`), `pods` (`get`, `create`, `delete`), and `pods/exec` (`create`), plus `nodes` (`get`) for cluster architecture detection (`amd64`/`arm64`).
- If `--provision` / `--teardown` are passed, additional cluster permissions are required to install/remove the target runtime stack.

### Key Flags & Operational Behavior

- **Per-invocation Runner Pod & cleanup**: Each invocation creates a uniquely named Pod (`opensandbox-bench-runner-<id>`) with soft node anti-affinity avoiding `fast-sandbox.io/firecracker-node=true` nodes. The Pod is automatically deleted on exit or cancellation (using an independent cleanup context) unless `--keep-runner` is specified.
- **Worker binary**: By default, `run` detects the Runner Pod node architecture and cross-compiles a static Linux binary (`CGO_ENABLED=0`) from `benchmark/tools` (override with `--module-dir`). Pass `--worker-binary <path>` to skip local compilation and inject a pre-built binary.
- **Target server**: Defaults to `opensandbox-server.<environment.systemNamespace>.svc:8080` derived from the active profile; override with `--server`.
- **Credential handling**: When `--api-key` is supplied, it is injected into the Runner Pod as a `0600` file (`/tmp/.opensandbox-api-key`) and passed via `--api-key-file` rather than on the `pods/exec` command line.
- **Output bundle**: `--output` must point to a non-existent or empty non-symlink directory so artifacts (`report.json`, `summary.md`, `traces/*.json`) from earlier runs are never mixed in. On worker failure, `run` still attempts a best-effort pull to preserve partial diagnostics. If `--output` is omitted, a warning is logged and the in-pod bundle is discarded when the Runner Pod is deleted.

See [`tools/Makefile`](./tools/Makefile) for all build and test targets.
