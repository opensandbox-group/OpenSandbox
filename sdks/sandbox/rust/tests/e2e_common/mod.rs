// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//! Shared setup for the live-server E2E suites (`tests/e2e_*.rs`).
//!
//! These suites talk to a real lifecycle server, execd, and egress sidecar
//! started by `scripts/rust-e2e.sh` — the same way the Python, Java, and Go
//! suites do. They are skipped unless the runner exports
//! `OPENSANDBOX_E2E=1`, so plain `cargo test` stays hermetic.
//!
//! Environment variables, shared with the other language E2E runners:
//!
//! * `OPENSANDBOX_TEST_DOMAIN` — lifecycle server `host:port`
//! * `OPENSANDBOX_TEST_PROTOCOL` — `http` (default) or `https`
//! * `OPENSANDBOX_TEST_API_KEY` — tenant API key
//! * `OPENSANDBOX_SANDBOX_DEFAULT_IMAGE` — sandbox image reference
//! * `OPENSANDBOX_TEST_USE_SERVER_PROXY` — route execd traffic via the server
//! * `RUN_CODE_INTERPRETER_E2E` — enable the Jupyter code-execution suite

#![allow(dead_code)]

use std::time::{Duration, SystemTime, UNIX_EPOCH};

use opensandbox::{ConnectionConfig, CreateOptions, NetworkPolicy, NetworkRule, Sandbox};

pub const DEFAULT_DOMAIN: &str = "localhost:8080";
pub const DEFAULT_PROTOCOL: &str = "http";
pub const DEFAULT_API_KEY: &str = "e2e-test";
pub const DEFAULT_IMAGE: &str = "opensandbox/code-interpreter:latest";

/// Per-request budget. Pulling the sandbox image on a cold runner is slow, so
/// this is deliberately larger than the SDK default.
pub const REQUEST_TIMEOUT: Duration = Duration::from_secs(180);
/// Budget for a sandbox to become ready after creation.
pub const READY_TIMEOUT: Duration = Duration::from_secs(180);
pub const POLL_INTERVAL: Duration = Duration::from_millis(500);
/// How long suite-level polling waits before giving up. Pause/resume and
/// interrupt can be slow on a cold Docker runner, so this is generous.
pub const WAIT_BUDGET: Duration = Duration::from_secs(120);

fn env_or(name: &str, default: &str) -> String {
    std::env::var(name).unwrap_or_else(|_| default.to_string())
}

fn env_flag(name: &str, default: &str) -> bool {
    matches!(
        env_or(name, default).trim().to_ascii_lowercase().as_str(),
        "1" | "true" | "yes" | "on"
    )
}

/// Whether the live-server suites should run at all.
pub fn enabled() -> bool {
    env_flag("OPENSANDBOX_E2E", "false")
}

/// Whether the Jupyter code-execution suite should run.
pub fn code_interpreter_enabled() -> bool {
    env_flag("RUN_CODE_INTERPRETER_E2E", "false")
}

pub fn image() -> String {
    env_or("OPENSANDBOX_SANDBOX_DEFAULT_IMAGE", DEFAULT_IMAGE)
}

pub fn connection_config() -> ConnectionConfig {
    ConnectionConfig::new()
        .with_domain(env_or("OPENSANDBOX_TEST_DOMAIN", DEFAULT_DOMAIN))
        .with_protocol(env_or("OPENSANDBOX_TEST_PROTOCOL", DEFAULT_PROTOCOL))
        .with_api_key(env_or("OPENSANDBOX_TEST_API_KEY", DEFAULT_API_KEY))
        .with_request_timeout(REQUEST_TIMEOUT)
        .with_server_proxy(env_flag("OPENSANDBOX_TEST_USE_SERVER_PROXY", "false"))
}

/// Unique value for metadata filters so concurrently running suites and
/// leftover sandboxes from previous runs never match each other.
pub fn unique_tag(prefix: &str) -> String {
    let millis = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|elapsed| elapsed.as_millis())
        .unwrap_or_default();
    format!("{prefix}-{millis}")
}

/// Create options shared by every suite: short idle timers, small limits, and
/// the caller's metadata.
pub fn create_options(metadata: &[(&str, &str)]) -> CreateOptions {
    let mut options = CreateOptions::from_image(image())
        .with_timeout_seconds(600)
        .with_ready_timeout(READY_TIMEOUT)
        .with_poll_interval(POLL_INTERVAL)
        .with_env("EXECD_API_GRACE_SHUTDOWN", "3s")
        .with_env("EXECD_JUPYTER_IDLE_POLL_INTERVAL", "200ms")
        .with_resource_limit("cpu", "500m")
        .with_resource_limit("memory", "256Mi");
    for (key, value) in metadata {
        options = options.with_metadata(*key, *value);
    }
    options
}

/// Creates a sandbox, failing the test when creation does not succeed.
pub async fn create_sandbox(metadata: &[(&str, &str)]) -> Sandbox {
    match Sandbox::create(connection_config(), create_options(metadata)).await {
        Ok(sandbox) => sandbox,
        Err(error) => panic!("creating a sandbox failed: {error}"),
    }
}

/// Best-effort cleanup; a failed kill still gets reaped by the sandbox TTL.
pub async fn kill_quietly(sandbox: &Sandbox) {
    if let Err(error) = sandbox.kill().await {
        eprintln!("cleanup: killing {} failed: {error}", sandbox.id());
    }
}

/// Prints the skip notice and returns when the live server is not configured.
macro_rules! require_e2e {
    () => {
        if !$crate::e2e_common::enabled() {
            eprintln!("skipping: set OPENSANDBOX_E2E=1 and start scripts/rust-e2e.sh to run live-server suites");
            return;
        }
    };
}

pub(crate) use require_e2e;

/// Like [`require_e2e!`], but also requires the credential-vault echo target.
#[allow(unused_macros)]
macro_rules! require_vault {
    () => {
        $crate::e2e_common::require_e2e!();
        if $crate::e2e_common::vault_target_ip().is_none() {
            eprintln!(
                "skipping: set OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP (scripts/rust-e2e.sh exports it)"
            );
            return;
        }
    };
}

#[allow(unused_imports)]
pub(crate) use require_vault;

/// Hostname the vault e2e target answers on inside the egress proxy.
pub const VAULT_TARGET_HOST: &str = "credential-vault-e2e.opensandbox.test";

/// Hostname of the credential-vault echo target, overridable like the Go and
/// Python runners.
pub fn vault_target_host() -> String {
    env_or(
        "OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_HOST",
        VAULT_TARGET_HOST,
    )
}

/// IP of the credential-vault echo target. `None` disables the vault suite.
pub fn vault_target_ip() -> Option<String> {
    std::env::var("OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP")
        .ok()
        .map(|ip| ip.trim().to_string())
        .filter(|ip| !ip.is_empty())
}

/// Creates a sandbox with the credential-vault transparent proxy enabled and a
/// deny-default egress policy that only admits the echo target. Returns `None`
/// when the server does not run the `dns+nft` egress mode that the vault
/// proxy requires.
pub async fn create_vault_sandbox(metadata: &[(&str, &str)]) -> Option<Sandbox> {
    let target_ip = vault_target_ip().expect("vault suite requires the echo target IP");
    let target_host = vault_target_host();
    let mut options = CreateOptions::from_image(image())
        .with_manual_cleanup()
        .with_timeout_seconds(1800)
        .with_ready_timeout(READY_TIMEOUT)
        .with_poll_interval(POLL_INTERVAL)
        .with_resource_limit("cpu", "1")
        .with_resource_limit("memory", "2Gi")
        .with_credential_proxy()
        .with_network_policy(NetworkPolicy {
            default_action: Some("deny".to_string()),
            egress: Some(vec![
                NetworkRule {
                    action: "allow".to_string(),
                    target: Some(target_host),
                    ports: None,
                },
                NetworkRule {
                    action: "allow".to_string(),
                    target: Some(target_ip),
                    ports: None,
                },
            ]),
        });
    for (key, value) in metadata {
        options = options.with_metadata(*key, *value);
    }
    match Sandbox::create(connection_config(), options).await {
        Ok(sandbox) => Some(sandbox),
        Err(opensandbox::Error::Api(api))
            if api.status == 400 && api.message.contains("credentialProxy") =>
        {
            eprintln!("skipping: server egress mode does not support the credential proxy: {api}");
            None
        }
        Err(error) => panic!("creating the vault sandbox failed: {error}"),
    }
}
