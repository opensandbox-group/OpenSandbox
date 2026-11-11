# OpenSandbox Rust SDK – E2E Tests

This folder contains live-server end-to-end tests for the Rust SDK. They talk
to a real lifecycle server, execd, and egress sidecar, and are **not** run by
default so that `cargo test` stays hermetic.

## Setup

The tests reuse the SDK crate and its dev-dependencies, so no extra dependency
installation is needed:

```bash
cd sdks/sandbox/rust
cargo build --tests
```

## Run tests

The fastest way to run the full live-server suite is the same script used in
CI:

```bash
bash scripts/rust-e2e.sh
```

To run them manually against an already-running server:

```bash
export OPENSANDBOX_E2E=1
export OPENSANDBOX_TEST_DOMAIN=localhost:8080
export OPENSANDBOX_TEST_PROTOCOL=http
export OPENSANDBOX_TEST_API_KEY=e2e-test
export OPENSANDBOX_SANDBOX_DEFAULT_IMAGE=opensandbox/code-interpreter:latest

cd sdks/sandbox/rust
cargo test --test e2e_lifecycle --test e2e_execd --test e2e_vault -- --test-threads=1
```

Run a specific suite:

```bash
cargo test --test e2e_lifecycle -- --test-threads=1
cargo test --test e2e_execd -- --test-threads=1
cargo test --test e2e_vault -- --test-threads=1
```

## Optional environment variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `OPENSANDBOX_TEST_DOMAIN` | `localhost:8080` | Lifecycle server `host:port` |
| `OPENSANDBOX_TEST_PROTOCOL` | `http` | `http` or `https` |
| `OPENSANDBOX_TEST_API_KEY` | `e2e-test` | Tenant API key |
| `OPENSANDBOX_SANDBOX_DEFAULT_IMAGE` | `opensandbox/code-interpreter:latest` | Sandbox image |
| `OPENSANDBOX_TEST_USE_SERVER_PROXY` | `false` | Route execd traffic through the server |
| `RUN_CODE_INTERPRETER_E2E` | `false` | Enable the Jupyter code-execution suite |
| `OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP` | unset | Echo target IP; the vault suite is skipped without it |
| `OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_HOST` | `credential-vault-e2e.opensandbox.test` | Echo target hostname |

## Code interpreter E2E

The Jupyter code-execution suite is skipped unless
`RUN_CODE_INTERPRETER_E2E=true` is set. `scripts/rust-e2e.sh` forwards the
variable from the CI job, so local runs can opt in the same way:

```bash
RUN_CODE_INTERPRETER_E2E=true bash scripts/rust-e2e.sh
```
