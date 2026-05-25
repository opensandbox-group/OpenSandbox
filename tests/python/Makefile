.PHONY: sync sync-dev test test-k8s test-sandbox test-manager test-code test-pool lint fmt

sync:
	uv sync

sync-dev:
	uv sync --group dev

test:
	uv run pytest

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

lint:
	uv run ruff check tests

fmt:
	uv run ruff format tests
