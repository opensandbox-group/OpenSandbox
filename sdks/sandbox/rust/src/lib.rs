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

//! Rust SDK for [OpenSandbox](https://open-sandbox.ai).
//!
//! The SDK talks to three APIs:
//!
//! * the **lifecycle** control plane (`/v1/...`) to create, inspect, pause,
//!   resume, renew, and delete sandboxes, and to resolve endpoints,
//! * **execd**, running inside the sandbox, to execute commands and code,
//!   transfer files, manage bash sessions, and read metrics, and
//! * the **egress** sidecar, to inspect and mutate the sandbox network policy
//!   and its credential vault.
//!
//! ```no_run
//! use opensandbox::{CreateOptions, ConnectionConfig, Sandbox};
//!
//! # async fn run() -> opensandbox::Result<()> {
//! let config = ConnectionConfig::new().with_domain("localhost:8080");
//! let sandbox = Sandbox::create(
//!     config,
//!     CreateOptions::from_image("python:3.12").with_timeout_seconds(600),
//! )
//! .await?;
//!
//! let execution = sandbox.exec("python -c 'print(1 + 1)'").await?;
//! assert_eq!(execution.exit_code, Some(0));
//! println!("{}", execution.text());
//!
//! sandbox.kill().await?;
//! # Ok(())
//! # }
//! ```
//!
//! ## Streaming
//!
//! [`ExecdClient::run_command`] returns an [`execution::ExecutionStream`] for
//! line-by-line consumption; [`ExecdClient::run_command_collect`] drains it
//! into an [`execution::Execution`]. execd emits newline-delimited JSON, and
//! the decoder in [`sse`] accepts both that and standard SSE frames.
//!
//! ## Endpoint resolution and proxy modes
//!
//! Data-plane traffic goes directly to the sandbox by default. With
//! [`ConnectionConfig::with_server_proxy`] it is routed through the lifecycle
//! server, which requires the tenant API key as `OPEN-SANDBOX-API-KEY`; the
//! SDK adds it for you and never forwards it to the sandbox in direct mode.

#![warn(missing_docs)]
#![warn(rust_2018_idioms)]

mod endpoint_cache;
mod transport;

pub mod config;
pub mod egress;
pub mod error;
pub mod execd;
pub mod execution;
pub mod lifecycle;
pub mod models;
pub mod sandbox;
pub mod sse;

pub use config::{
    ConnectionConfig, RetryConfig, API_KEY_HEADER, DEFAULT_DOMAIN, DEFAULT_EGRESS_PORT,
    DEFAULT_EXECD_PORT, DEFAULT_HEALTH_CHECK_INTERVAL, DEFAULT_PROTOCOL, DEFAULT_READY_TIMEOUT,
    DEFAULT_REQUEST_TIMEOUT, DEFAULT_TIMEOUT_SECONDS, EGRESS_AUTH_HEADER, EXECD_TOKEN_HEADER,
    SANDBOX_ORIGIN_HEADER, WAKE_MARKER_EXPIRED, WAKE_MARKER_HEADER,
};
pub use egress::EgressClient;
pub use error::{ApiError, Error, Result};
pub use execd::{ExecdClient, UploadEntry};
pub use execution::{Execution, ExecutionEvent, ExecutionStream};
pub use lifecycle::{LifecycleClient, ListSandboxesQuery, ListSnapshotsQuery};
pub use models::*;
pub use sandbox::{
    default_entrypoint, default_resource_limits, CreateOptions, ReadyOptions, Sandbox,
    SandboxOrigin,
};
pub use sse::{decode_all, SseDecoder, SseEvent};

/// SDK version reported in the `User-Agent` header.
pub const VERSION: &str = env!("CARGO_PKG_VERSION");
