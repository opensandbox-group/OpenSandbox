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
