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
