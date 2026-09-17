## OpenSandbox Python SDK – E2E Tests (uv)

This folder is a standalone e2e test project managed by **uv**.

### Setup

```bash
cd tests/python
uv sync
```

### Run tests

```bash
uv run pytest
```

Run a specific suite:

```bash
uv run pytest tests/test_sandbox_e2e.py
uv run pytest tests/test_sandbox_pool_e2e_sync.py tests/test_sandbox_pool_e2e_async.py
uv run pytest tests/test_credential_vault_e2e.py
```

Redis-backed pool E2E tests are skipped unless `OPENSANDBOX_TEST_REDIS_URL` is set,
for example `redis://127.0.0.1:6379/0`.

Credential Vault E2E tests require a reachable target service and
`OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP`. The repository E2E scripts start
the target service and run the Vault tests as part of each language's normal
E2E suite:

```bash
../../scripts/python-e2e.sh
```

### Fast-sandbox (fsb) integration env

`tests/test_fsb_e2e.py` converts the HTTP verify stages of
`scripts/fast-sandbox-env/integration-env.sh` (template create → gateway
ping → networkpolicy PATCH/DELETE convergence, lifecycle ops, pause/resume,
public snapshot round trip) into Python SDK calls. It is skipped unless the
fsb stack is up and `OPENSANDBOX_TEST_FSB_TEMPLATE_ID` points at the
golden-image template the env script builds, and it is excluded from the
default `make test` run:

```bash
./scripts/fast-sandbox-env/integration-env.sh up     # brings up the stack, prints/keeps the template id
cd tests/python
OPENSANDBOX_TEST_FSB_TEMPLATE_ID="$(cat "$WORK/template-id")" make test-fsb
```

#### Covered SDK surface

Template management (`SandboxManager`):

- [x] `create_template` — async build asserted Pending → Succeeded + manifestRef
- [x] `get_template`
- [x] `list_templates` — metadata filter (percent-encoded values)
- [x] `delete_template` — followed by a 404 re-read

Sandbox lifecycle (`SandboxManager` + `Sandbox`):

- [x] `Sandbox.create_from_template` — with `networkPolicy` + `metadata`; readiness via the signed gateway route
- [x] `Sandbox.create(snapshot_id=...)` — restore with the pool resource profile; server-reported `origin=template` routes egress through the control plane
- [x] `Sandbox.resume` — re-resolves endpoints, reads `OPEN-SANDBOX-ORIGIN`
- [x] `Sandbox.connect` — re-attach to a running sandbox; origin auto-detected from the server header; execd + policy operations served through the re-attached instance
- [x] `sandbox.pause` → `Paused` (durable-first window)
- [x] `sandbox.kill` (+ deletion re-read)
- [x] `SandboxManager.get_sandbox_info` / `list_sandbox_infos` / `patch_sandbox_metadata` (upsert + null delete) / `renew_sandbox`
- [x] `sandbox.origin` — template vs unknown, server header reconciliation

Snapshot management (`SandboxManager`):

- [x] `create_snapshot` — 202 + Creating; re-entry fence (409 or accepted → terminal)
- [x] `get_snapshot` — poll to Ready
- [x] `list_snapshots` — `sandboxId`-scoped, both rows Ready
- [x] `delete_snapshot`

Execd plane (`Sandbox` properties, exercised on template sandboxes):

- [x] `sandbox.commands.run` — stdout assertions + egress probes (`wget`)
- [x] `sandbox.files.write_file` / `read_file` — write/read round trip
- [x] `sandbox.get_metrics` — cpu/memory value ranges
- [x] `sandbox.is_healthy` — post-resume / post-restore

Egress policy (`Sandbox`):

- [x] `get_egress_policy` — convergence polling
- [x] `patch_egress_rules` — merge semantics verified **at HTTP level** (newly allowed target serves HTTPS)
- [x] `delete_egress_rules` — removal verified **at HTTP level** (deleted target stops resolving/serving)
- [x] policy persistence across pause/resume and after snapshot restore

State fidelity:

- [x] pause/resume: regular file, RAM-backed tmpfs file (`/dev/shm`), continuous guest uptime (same-VM resume, not a reboot)
- [x] snapshot restore: pre-snapshot file present in the restored guest

Not covered here (needs a different stack or out of scope): credential
vault (fsb has no egress sidecar), execd background/isolated-session
APIs, signed endpoint expiry.

### Foreground command stream completion

```bash
uv run --frozen pytest tests/test_command_stream_e2e.py
```

This Docker bridge matrix uses direct and server-proxied SDK connections, each
with and without a `dns+nft` egress sidecar. Configure the lifecycle server with
an execd image and an egress image, and make its published sandbox endpoints
reachable from the test runner. On Docker Desktop, one option is to run both
the lifecycle server and the test runner in Docker, use
`[docker].host_ip = "host.docker.internal"`, and point the SDK at the server's
published port on that hostname. Leave `[server].eip` unset for this topology.
The sandbox image must contain `python3`. Kubernetes runs skip this matrix.

The tests cover empty successful commands and approximately 4 MiB of interleaved
stdout/stderr with successful and nonzero exits. A delayed SDK callback exercises
buffering; per-stream line order, Unicode tails without final newlines, accumulated
logs, exit status, and terminal-event ordering must all be preserved through
HTTP response completion.

Each case emits a `COMMAND_STREAM_METRIC` JSON record. `total_ms` measures the
SDK command call; `terminal_to_return_ms` measures the interval from the SDK's
terminal-event callback to the call returning. These measurements support A/B
validation of command response latency, including #1277/#1661, without imposing
machine-speed thresholds on CI. Test sandboxes explicitly use a one-second
`EXECD_API_GRACE_SHUTDOWN`; compare the same test against baseline and candidate
execd images. Passing the correctness assertions alone does not establish that
the fixed tail delay has been removed.

### Notes about asyncio + shared Sandbox

These tests may reuse a single Sandbox instance across multiple test cases for speed.
To avoid `RuntimeError: Event loop is closed`, pytest-asyncio is configured to use a
**session-scoped event loop** in `pyproject.toml`.

### Handy shortcuts

```bash
make sync
make test
make test-sandbox
make test-pool
make lint
make fmt
```
