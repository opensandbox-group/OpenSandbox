# OpenSandbox Rust SDK

Rust client library for the [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) API.

Covers the lifecycle control plane, execd, and the egress sidecar:

- **Lifecycle** — create, inspect, pause, resume, renew, and destroy sandboxes, resolve endpoints, manage snapshots and network policy
- **Execd** — run commands and code, manage bash sessions and files, monitor metrics inside a sandbox
- **Egress** — inspect and mutate the sandbox network policy and its sandbox-local Credential Vault

Highlights:

- High-level `Sandbox` facade that resolves the sandbox endpoint and carries its access token for you, plus low-level clients for every API area
- Line-by-line streaming for commands, code, and metrics, with a decoder that accepts both execd's NDJSON and standard SSE frames
- Typed errors with server `code`/`message`, `X-Request-Id`, `Retry-After`, and the OSEP-0024 wake marker
- Retry policy aligned with the other OpenSandbox SDKs: status-code retries only for idempotent methods, plus wake-contract replays for any method
- No hidden global state; every client is built from an explicit `ConnectionConfig`
- `Send + Sync + Clone` handles: move a `Sandbox` or client into a spawned task, or clone one per task

## Installation

The crate lives in the monorepo and is not published to crates.io yet.

```toml
[dependencies]
opensandbox = { path = "sdks/sandbox/rust" }
tokio = { version = "1", features = ["macros", "rt-multi-thread"] }
futures-util = "0.3" # only needed to call `.next()` on execution and metrics streams
```

You can also depend on the repository directly:

```toml
[dependencies]
opensandbox = { git = "https://github.com/opensandbox-group/OpenSandbox", branch = "main" }
```

The MSRV is Rust 1.75. Only an async runtime is required at compile time; the SDK builds its own `reqwest` clients and does not need a global runtime handle.

## Quick Start

Before running the example, start an OpenSandbox server. See the root [README.md](../../../README.md) for instructions. The default connection target is `http://localhost:8080`.

```rust
use opensandbox::{ConnectionConfig, CreateOptions, Sandbox};

#[tokio::main]
async fn main() -> opensandbox::Result<()> {
    // 1. Configure the connection. The API key can also come from
    //    OPEN_SANDBOX_API_KEY.
    let config = ConnectionConfig::new()
        .with_domain("localhost:8080")
        .with_api_key("your-api-key");

    // 2. Create a sandbox. Creation waits for the sandbox to report Running
    //    and for execd to answer /ping, and cleans the sandbox up if either
    //    step fails.
    let sandbox = Sandbox::create(
        config,
        CreateOptions::from_image("python:3.12").with_timeout_seconds(600),
    )
    .await?;

    // 3. Run a command and read the accumulated output.
    let execution = sandbox.exec("python -c 'print(1 + 1)'").await?;
    print!("{}", execution.text());
    println!("exit code: {:?}", execution.exit_code);

    // 4. Terminate the remote sandbox.
    sandbox.kill().await?;
    Ok(())
}
```

A runnable variant is in [examples/quickstart.rs](examples/quickstart.rs).

## Documentation

The full guide — low-level clients, retry behavior, usage examples for every API area, configuration tables, the API reference, streaming events, and error handling — lives in the docs:

- [Rust SDK](../../../docs/sdks/rust.md) in this repository
- [Rust SDK](https://open-sandbox.ai/sdks/rust) on the docs site

Scope and limitations are listed in [Scope and limitations](../../../docs/sdks/rust.md#scope-and-limitations).

## Development

```bash
cargo fmt --check
cargo clippy --all-targets -- -D warnings
cargo test --all-targets
```

The tests under `tests/` are wire-level integration tests that start a local mock HTTP server on a loopback port, so no live OpenSandbox deployment is required:

| Suite | Coverage |
| --- | --- |
| `lifecycle_wire` | Request mapping, endpoint cache and TTL, error mapping, retry metadata |
| `execd_wire` | SSE/NDJSON decoding across chunk boundaries, multipart upload order, Range vs line range, metrics |
| `sandbox_flow` | Create/connect/exec/kill sequences, token propagation, cleanup on failure |
| `retry_contract` | Idempotent retries, non-idempotent behavior, wake replays, disabled retries |
| `egress_wire` | Policy and credential vault mapping, auth headers, direct-mode key stripping |

`cargo run --example quickstart` exercises a real deployment.

## License

Apache 2.0.
