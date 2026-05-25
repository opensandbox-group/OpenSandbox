.PHONY: help install dev-install format lint type-check test test-cov clean docs build publish

# Default target
help: ## Show this help message
	@echo "Available commands:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-15s\033[0m %s\n", $$1, $$2}'

install: ## Install package dependencies
	uv sync

dev-install: generate-api ## Install package with development dependencies
	uv sync --all-extras

format: ## Format code with black and isort
	uv run black .
	uv run isort .

lint: ## Run linting with ruff
	uv run ruff check .

type-check: ## Run type checking with pyright
	uv run pyright

test: ## Run tests
	uv run pytest

test-cov: ## Run tests with coverage
	uv run pytest --cov=src/opensandbox --cov-report=html --cov-report=term

clean: ## Clean build artifacts
	rm -rf build/
	rm -rf dist/
	rm -rf *.egg-info/
	rm -rf .pytest_cache/
	rm -rf .coverage
	rm -rf htmlcov/
	find . -type d -name __pycache__ -exec rm -rf {} +
	find . -name "*.pyc" -delete

docs: ## Generate documentation
	cd docs && uv run sphinx-build -b html . _build/html

build: generate-api ## Build package with API generation
	uv build

publish: ## Publish to PyPI (requires authentication)
	uv publish

# Development workflow targets
check: format lint type-check ## Run all code quality checks

ci: generate-api dev-install check test ## Run CI pipeline locally

generate-api: ## Generate API clients from OpenAPI specs (using openapi-python-client)
	uv run python scripts/generate_api.py

clean-api: ## Clean generated API client code
	rm -rf src/opensandbox/api/execd/
	rm -rf src/opensandbox/api/egress/
	rm -rf src/opensandbox/api/lifecycle/

# Docker targets
docker-build: ## Build Docker image for development
	docker build -t opensandbox-python-dev .

docker-test: ## Run tests in Docker container
	docker run --rm -v $(PWD):/app opensandbox-python-dev make test
