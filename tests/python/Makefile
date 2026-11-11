.PHONY: sync sync-dev test test-k8s test-sandbox test-manager test-code test-pool test-credential-vault test-fsb test-fsb-idle-pause lint fmt

sync:
	uv sync

sync-dev:
	uv sync --group dev

test:
	uv run pytest --ignore=tests/test_execd_init_e2e.py --ignore=tests/test_execd_hardening_e2e.py --ignore=tests/test_fsb_e2e.py --ignore=tests/test_fsb_idle_pause_e2e.py

test-kubernetes-mini:
	uv run pytest \
		tests/test_sandbox_e2e_sync.py \
		tests/test_sandbox_manager_e2e_sync.py

test-sandbox:
	uv run pytest tests/test_sandbox_e2e.py

test-manager:
	uv run pytest tests/test_sandbox_manager_e2e.py

test-code:
	uv run pytest tests/test_code_interpreter_e2e.py

test-pool:
	uv run pytest tests/test_sandbox_pool_e2e_sync.py tests/test_sandbox_pool_e2e_async.py

test-credential-vault:
	uv run pytest tests/test_credential_vault_e2e.py

# fast-sandbox (fsb) integration env: requires the stack from
# scripts/fast-sandbox-env/integration-env.sh up plus
# OPENSANDBOX_TEST_FSB_TEMPLATE_ID; skipped everywhere else.
test-fsb:
	uv run pytest tests/test_fsb_e2e.py

# OSEP-0024 idle-pause/wake suite: requires the stack brought up with
# IDLE_PAUSE=1 plus OPENSANDBOX_TEST_FSB_TEMPLATE_ID and
# OPENSANDBOX_TEST_IDLE_PAUSE=1; skipped everywhere else.
test-fsb-idle-pause:
	uv run pytest tests/test_fsb_idle_pause_e2e.py

lint:
	uv run ruff check tests benchmarks

fmt:
	uv run ruff format tests benchmarks
