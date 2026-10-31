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

//! Wire models for the lifecycle and execd APIs.
//!
//! Field names and enums mirror `specs/sandbox-lifecycle.yml` and
//! `specs/execd-api.yaml`. Types are intentionally permissive on input
//! (`Option` + `skip_serializing_if`) so callers only send what they set.

use std::collections::BTreeMap;

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

/// Resource limits, for example `{"cpu": "1", "memory": "2Gi"}`.
pub type ResourceLimits = BTreeMap<String, String>;
/// User-supplied sandbox metadata.
pub type Metadata = BTreeMap<String, String>;
/// Provider-specific creation extensions.
pub type Extensions = BTreeMap<String, String>;

/// High-level lifecycle state of a sandbox.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub enum SandboxState {
    /// Accepted, not yet running.
    #[serde(rename = "Pending")]
    Pending,
    /// Usable.
    #[serde(rename = "Running")]
    Running,
    /// Pause in progress.
    #[serde(rename = "Pausing")]
    Pausing,
    /// Frozen, state preserved.
    #[serde(rename = "Paused")]
    Paused,
    /// Resume in progress.
    #[serde(rename = "Resuming")]
    Resuming,
    /// Deletion accepted.
    #[serde(rename = "Stopping")]
    Stopping,
    /// Gone (`exit 0`).
    #[serde(rename = "Terminated")]
    Terminated,
    /// Gone (`exit != 0`).
    #[serde(rename = "Failed")]
    Failed,
    /// A state introduced by a newer server.
    #[serde(other)]
    #[default]
    Unknown,
}

/// Detailed status of a sandbox.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct SandboxStatus {
    /// Lifecycle state.
    pub state: SandboxState,
    /// Short machine-readable reason.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
    /// Human-readable detail.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
    /// When the state last changed.
    #[serde(
        rename = "lastTransitionAt",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub last_transition_at: Option<DateTime<Utc>>,
}

/// OCI image reference plus optional registry credentials.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ImageSpec {
    /// Image reference, for example `python:3.12`.
    pub uri: String,
    /// Registry credentials for private images.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub auth: Option<ImageAuth>,
}

/// Registry credentials.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ImageAuth {
    /// Registry username.
    pub username: String,
    /// Registry password or token.
    pub password: String,
}

/// Runtime platform constraint.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PlatformSpec {
    /// `linux` or `windows`.
    pub os: String,
    /// `amd64` or `arm64`.
    pub arch: String,
}

/// Storage mount for a sandbox.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Volume {
    /// Volume name.
    pub name: String,
    /// Host path backend.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub host: Option<HostMount>,
    /// Platform-managed volume backend.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub pvc: Option<PvcMount>,
    /// Alibaba Cloud OSS backend.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ossfs: Option<OssfsMount>,
    /// Mount path inside the sandbox.
    #[serde(rename = "mountPath")]
    pub mount_path: String,
    /// Mount read-only.
    #[serde(
        rename = "readOnly",
        default,
        skip_serializing_if = "std::ops::Not::not"
    )]
    pub read_only: bool,
    /// Sub-path of the backing volume.
    #[serde(rename = "subPath", default, skip_serializing_if = "Option::is_none")]
    pub sub_path: Option<String>,
}

/// Host path mount.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HostMount {
    /// Absolute host path.
    pub path: String,
}

/// Kubernetes PVC mount.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PvcMount {
    /// Existing claim name.
    #[serde(rename = "claimName")]
    pub claim_name: String,
    /// Create the claim when missing.
    #[serde(
        rename = "createIfNotExists",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub create_if_not_exists: Option<bool>,
    /// Delete the claim with the sandbox.
    #[serde(
        rename = "deleteOnSandboxTermination",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub delete_on_sandbox_termination: Option<bool>,
    /// Storage class for a created claim.
    #[serde(
        rename = "storageClass",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub storage_class: Option<String>,
    /// Requested storage size.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub storage: Option<String>,
    /// Access modes for a created claim.
    #[serde(
        rename = "accessModes",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub access_modes: Option<Vec<String>>,
}

/// OSS mount through ossfs.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct OssfsMount {
    /// Bucket name.
    pub bucket: String,
    /// OSS endpoint.
    pub endpoint: String,
    /// ossfs version.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub version: Option<String>,
    /// Extra mount options.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub options: Option<Vec<String>>,
    /// Access key id.
    #[serde(rename = "accessKeyId")]
    pub access_key_id: String,
    /// Access key secret.
    #[serde(rename = "accessKeySecret")]
    pub access_key_secret: String,
}

/// Egress policy applied at creation.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct NetworkPolicy {
    /// `allow` (default) or `deny`.
    #[serde(
        rename = "defaultAction",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub default_action: Option<String>,
    /// Ordered rules.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub egress: Option<Vec<NetworkRule>>,
}

/// One allow/deny egress rule.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NetworkRule {
    /// `allow` or `deny`.
    pub action: String,
    /// FQDN, wildcard domain, IP, or CIDR. Omit to scope by port only.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub target: Option<String>,
    /// TCP destination ports.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ports: Option<Vec<u16>>,
}

/// Enables credential-vault transparent proxying at startup.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CredentialProxyConfig {
    /// Whether the proxy is enabled.
    pub enabled: bool,
}

/// Command run before the sandbox entrypoint.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct LifecycleHook {
    /// argv of the hook.
    pub command: Vec<String>,
    /// Hook budget in seconds.
    #[serde(
        rename = "timeoutSeconds",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub timeout_seconds: Option<u32>,
}

/// Periodic command scheduled while the sandbox runs.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PeriodicLifecycleHook {
    /// Hook name.
    pub name: String,
    /// Cron-like schedule.
    pub schedule: String,
    /// argv of the hook.
    pub command: Vec<String>,
    /// Hook budget in seconds.
    #[serde(
        rename = "timeoutSeconds",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub timeout_seconds: Option<u32>,
}

/// Application-declared activity probe (OSEP-0024).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ActivityProbe {
    /// Probe port.
    pub port: u16,
    /// Absolute probe path.
    pub path: String,
    /// Probe budget in seconds (1-10).
    #[serde(
        rename = "timeoutSeconds",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub timeout_seconds: Option<u32>,
}

/// Idle behavior (OSEP-0024, fast-sandbox only).
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct IdlePolicy {
    /// Idle threshold in seconds (30-86400).
    #[serde(
        rename = "idleTimeoutSeconds",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub idle_timeout_seconds: Option<u64>,
    /// Action at the threshold; only `pause` exists.
    #[serde(rename = "onIdle", default, skip_serializing_if = "Option::is_none")]
    pub on_idle: Option<String>,
    /// Whether access may wake the sandbox. Defaults to true server-side.
    #[serde(
        rename = "wakeOnAccess",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub wake_on_access: Option<bool>,
    /// Activity probe used with auto-pause.
    #[serde(
        rename = "activeProbe",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub active_probe: Option<ActivityProbe>,
}

/// Renew-on-access behavior (OSEP-0009).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RenewalPolicy {
    /// Extension window in seconds (300-86400).
    #[serde(rename = "extendSeconds")]
    pub extend_seconds: u64,
}

/// Lifecycle configuration applied at creation.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct SandboxLifecycle {
    /// Pre-start hook.
    #[serde(rename = "preStart", default, skip_serializing_if = "Option::is_none")]
    pub pre_start: Option<LifecycleHook>,
    /// Periodic hooks.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub periodic: Option<Vec<PeriodicLifecycleHook>>,
    /// Idle policy.
    #[serde(
        rename = "idlePolicy",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub idle_policy: Option<IdlePolicy>,
    /// Renewal policy.
    #[serde(
        rename = "renewalPolicy",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub renewal_policy: Option<RenewalPolicy>,
}

/// Body of `POST /v1/sandboxes`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CreateSandboxRequest {
    /// Container image.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub image: Option<ImageSpec>,
    /// Restore from a snapshot.
    #[serde(
        rename = "snapshotId",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub snapshot_id: Option<String>,
    /// Create from a fast-sandbox template.
    #[serde(
        rename = "templateId",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub template_id: Option<String>,
    /// Absolute TTL in seconds. Omit for no expiry.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub timeout: Option<u64>,
    /// Resource limits.
    #[serde(
        rename = "resourceLimits",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub resource_limits: Option<ResourceLimits>,
    /// Resource requests.
    #[serde(
        rename = "resourceRequests",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub resource_requests: Option<ResourceLimits>,
    /// Environment variables.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub env: Option<BTreeMap<String, String>>,
    /// Enable secure access.
    #[serde(
        rename = "secureAccess",
        default,
        skip_serializing_if = "std::ops::Not::not"
    )]
    pub secure_access: bool,
    /// User metadata.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub metadata: Option<Metadata>,
    /// Lifecycle configuration.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub lifecycle: Option<SandboxLifecycle>,
    /// Entrypoint argv.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub entrypoint: Option<Vec<String>>,
    /// Egress network policy.
    #[serde(
        rename = "networkPolicy",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub network_policy: Option<NetworkPolicy>,
    /// Credential proxy toggle.
    #[serde(
        rename = "credentialProxy",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub credential_proxy: Option<CredentialProxyConfig>,
    /// Volume mounts.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub volumes: Option<Vec<Volume>>,
    /// Provider-specific extensions.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub extensions: Option<Extensions>,
    /// Platform constraint.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub platform: Option<PlatformSpec>,
}

/// Confirmed pool allocation summary.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AllocationSummary {
    /// Allocation mode, currently only `pool`.
    pub mode: String,
    /// Pool reference.
    #[serde(rename = "poolRef")]
    pub pool_ref: String,
    /// Allocation state, currently only `allocated`.
    pub state: String,
}

/// Sandbox as returned by the lifecycle API.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SandboxInfo {
    /// Sandbox identifier.
    pub id: String,
    /// Source image, when created from an image.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub image: Option<ImageSpec>,
    /// Source snapshot, when created from a snapshot.
    #[serde(
        rename = "snapshotId",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub snapshot_id: Option<String>,
    /// Current status.
    #[serde(default)]
    pub status: SandboxStatus,
    /// User metadata.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub metadata: Option<Metadata>,
    /// Effective provider extensions.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub extensions: Option<Extensions>,
    /// Entrypoint argv.
    #[serde(default)]
    pub entrypoint: Vec<String>,
    /// Absolute expiry.
    #[serde(rename = "expiresAt", default, skip_serializing_if = "Option::is_none")]
    pub expires_at: Option<DateTime<Utc>>,
    /// Creation time.
    #[serde(rename = "createdAt", default, skip_serializing_if = "Option::is_none")]
    pub created_at: Option<DateTime<Utc>>,
    /// Platform constraint.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub platform: Option<PlatformSpec>,
    /// Pool allocation, when applicable.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub allocation: Option<AllocationSummary>,
}

/// Pagination metadata.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct PaginationInfo {
    /// Current page (1-based).
    #[serde(default)]
    pub page: u32,
    /// Items per page.
    #[serde(rename = "pageSize", default)]
    pub page_size: u32,
    /// Total matching items.
    #[serde(rename = "totalItems", default)]
    pub total_items: u32,
    /// Total pages.
    #[serde(rename = "totalPages", default)]
    pub total_pages: u32,
    /// Whether another page exists.
    #[serde(rename = "hasNextPage", default)]
    pub has_next_page: bool,
}

/// Paginated sandbox list.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct ListSandboxesResponse {
    /// Page items.
    #[serde(default)]
    pub items: Vec<SandboxInfo>,
    /// Pagination metadata.
    #[serde(default)]
    pub pagination: PaginationInfo,
}

/// Snapshot lifecycle state.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub enum SnapshotState {
    /// Being captured.
    #[serde(rename = "Creating")]
    Creating,
    /// Being removed.
    #[serde(rename = "Deleting")]
    Deleting,
    /// Usable.
    #[serde(rename = "Ready")]
    Ready,
    /// Capture failed.
    #[serde(rename = "Failed")]
    Failed,
    /// A state introduced by a newer server.
    #[serde(other)]
    #[default]
    Unknown,
}

/// Snapshot status.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct SnapshotStatus {
    /// Snapshot state.
    #[serde(default)]
    pub state: SnapshotState,
    /// Short reason.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
    /// Human-readable detail.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
    /// When the state last changed.
    #[serde(
        rename = "lastTransitionAt",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub last_transition_at: Option<DateTime<Utc>>,
}

/// Snapshot metadata.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SnapshotInfo {
    /// Snapshot identifier.
    pub id: String,
    /// Source sandbox.
    #[serde(rename = "sandboxId")]
    pub sandbox_id: String,
    /// Optional name.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    /// Current status.
    #[serde(default)]
    pub status: SnapshotStatus,
    /// Creation time.
    #[serde(rename = "createdAt", default)]
    pub created_at: Option<DateTime<Utc>>,
}

/// Body of `POST /v1/sandboxes/{id}/snapshots`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CreateSnapshotRequest {
    /// Optional human-readable name.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
}

/// Paginated snapshot list.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct ListSnapshotsResponse {
    /// Page items.
    #[serde(default)]
    pub items: Vec<SnapshotInfo>,
    /// Pagination metadata.
    #[serde(default)]
    pub pagination: PaginationInfo,
}

/// Public endpoint for a service port inside a sandbox.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Endpoint {
    /// `host:port` or a full URL.
    pub endpoint: String,
    /// Headers that must accompany data-plane requests (for example the execd
    /// access token).
    #[serde(default)]
    pub headers: BTreeMap<String, String>,
    /// Value of `Open-Sandbox-Origin`, when the server sent one.
    #[serde(skip)]
    pub origin: Option<String>,
}

/// Body of `POST /v1/sandboxes/{id}/renew-expiration`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RenewExpirationRequest {
    /// New absolute expiry.
    #[serde(rename = "expiresAt")]
    pub expires_at: DateTime<Utc>,
}

/// Response of `POST /v1/sandboxes/{id}/renew-expiration`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RenewExpirationResponse {
    /// New absolute expiry.
    #[serde(rename = "expiresAt")]
    pub expires_at: DateTime<Utc>,
}

/// Response of the network policy endpoints.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct PolicyStatusResponse {
    /// Enforcement status.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub status: Option<String>,
    /// Policy mode.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub mode: Option<String>,
    /// Where the policy is enforced.
    #[serde(
        rename = "enforcementMode",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub enforcement_mode: Option<String>,
    /// Why the status is what it is.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
    /// Effective policy.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub policy: Option<NetworkPolicy>,
}

// ---------------------------------------------------------------------------
// execd models
// ---------------------------------------------------------------------------

/// One stdout/stderr line.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct OutputMessage {
    /// Line text, without the trailing newline.
    pub text: String,
    /// Server-side timestamp in milliseconds.
    #[serde(default)]
    pub timestamp: i64,
}

/// One MIME-keyed execution result.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct ExecutionResult {
    /// MIME type to payload, for example `text/plain` to `4`.
    #[serde(default)]
    pub results: BTreeMap<String, String>,
    /// Server-side timestamp in milliseconds.
    #[serde(default)]
    pub timestamp: i64,
}

impl ExecutionResult {
    /// Returns the `text/plain` payload, when present.
    pub fn text(&self) -> Option<&str> {
        self.results.get("text/plain").map(String::as_str)
    }
}

/// A failed command or code execution.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct ExecutionError {
    /// Error class name.
    #[serde(default)]
    pub name: String,
    /// Error value/message.
    #[serde(default)]
    pub value: String,
    /// Server-side timestamp in milliseconds.
    #[serde(default)]
    pub timestamp: i64,
    /// Traceback lines.
    #[serde(default)]
    pub traceback: Vec<String>,
    /// Exit status when the command ran and failed. `-1` means killed by a
    /// signal; absent when the process never started.
    #[serde(default)]
    pub exit_code: Option<i32>,
}

/// Terminal event of a foreground execution.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct ExecutionComplete {
    /// Server-side timestamp in milliseconds.
    #[serde(default)]
    pub timestamp: i64,
    /// Wall-clock duration in milliseconds.
    #[serde(default)]
    pub execution_time: i64,
    /// Exit status of a successful foreground command. Absent for background
    /// launches, code execution, and older servers.
    #[serde(default)]
    pub exit_code: Option<i32>,
}

/// Initial event carrying the execution identifier.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct ExecutionInit {
    /// Execution identifier.
    #[serde(rename = "text", default)]
    pub id: String,
    /// Server-side timestamp in milliseconds.
    #[serde(default)]
    pub timestamp: i64,
}

/// Body of `POST /command`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct RunCommandRequest {
    /// Shell command text. Exactly one of `command`/`argv` is used.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub command: Option<String>,
    /// Literal argv; index 0 is the executable.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub argv: Option<Vec<String>>,
    /// Working directory.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cwd: Option<String>,
    /// Start asynchronously.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub background: bool,
    /// Maximum duration in **milliseconds**. Zero means no timeout.
    #[serde(default, skip_serializing_if = "is_zero_i64")]
    pub timeout: i64,
    /// Run as this uid.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub uid: Option<i32>,
    /// Run as this gid.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub gid: Option<i32>,
    /// Extra environment variables.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub envs: Option<BTreeMap<String, String>>,
}

fn is_zero_i64(value: &i64) -> bool {
    *value == 0
}

impl RunCommandRequest {
    /// Runs a shell command.
    pub fn shell(command: impl Into<String>) -> Self {
        Self {
            command: Some(command.into()),
            ..Self::default()
        }
    }

    /// Runs an argv without a shell.
    pub fn argv<I, S>(argv: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        Self {
            argv: Some(argv.into_iter().map(Into::into).collect()),
            ..Self::default()
        }
    }

    /// Sets the working directory.
    pub fn with_cwd(mut self, cwd: impl Into<String>) -> Self {
        self.cwd = Some(cwd.into());
        self
    }

    /// Starts the command in the background.
    pub fn in_background(mut self) -> Self {
        self.background = true;
        self
    }

    /// Sets the timeout in milliseconds.
    pub fn with_timeout_millis(mut self, millis: i64) -> Self {
        self.timeout = millis;
        self
    }

    /// Runs the command as `uid`/`gid`.
    pub fn with_user(mut self, uid: i32, gid: i32) -> Self {
        self.uid = Some(uid);
        self.gid = Some(gid);
        self
    }

    /// Adds an environment variable.
    pub fn with_env(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.envs
            .get_or_insert_with(BTreeMap::new)
            .insert(key.into(), value.into());
        self
    }
}

/// Status of a command execution.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CommandStatusResponse {
    /// Execution identifier.
    #[serde(default)]
    pub id: String,
    /// Accumulated output.
    #[serde(default)]
    pub content: String,
    /// Whether the command is still running.
    #[serde(default)]
    pub running: bool,
    /// Exit status, once finished.
    #[serde(default)]
    pub exit_code: Option<i32>,
    /// Error detail, when the command failed.
    #[serde(default)]
    pub error: Option<String>,
    /// Start time.
    #[serde(rename = "started_at", default)]
    pub started_at: Option<DateTime<Utc>>,
    /// Finish time.
    #[serde(rename = "finished_at", default)]
    pub finished_at: Option<DateTime<Utc>>,
}

/// Background command log snapshot.
#[derive(Debug, Clone, Default)]
pub struct CommandLogsResponse {
    /// Log text.
    pub output: String,
    /// Cursor to pass to the next incremental read.
    pub cursor: i64,
}

/// Bash session handle.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Session {
    /// Session identifier.
    #[serde(rename = "session_id", default)]
    pub id: String,
}

/// Body of `POST /session`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CreateSessionRequest {
    /// Initial working directory.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cwd: Option<String>,
}

/// Body of `POST /session/{id}/run`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct RunInSessionRequest {
    /// Shell command.
    pub command: String,
    /// Working directory.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cwd: Option<String>,
    /// Maximum duration in milliseconds.
    #[serde(default, skip_serializing_if = "is_zero_i64")]
    pub timeout: i64,
}

/// Code execution context.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CodeContext {
    /// Context identifier.
    #[serde(default)]
    pub id: String,
    /// Language, for example `python`.
    pub language: String,
}

/// Body of `POST /code/context`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CreateContextRequest {
    /// Language for the new context.
    pub language: String,
}

/// Body of `POST /code`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct RunCodeRequest {
    /// Target context. Omit to run in a transient context.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub context: Option<CodeContext>,
    /// Source code.
    pub code: String,
}

/// File metadata.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct FileInfo {
    /// Absolute path.
    pub path: String,
    /// `file`, `directory`, or `symlink`.
    #[serde(rename = "type", default)]
    pub kind: String,
    /// Size in bytes.
    #[serde(default)]
    pub size: i64,
    /// Last modification time.
    #[serde(rename = "modified_at", default)]
    pub modified_at: Option<DateTime<Utc>>,
    /// Creation time.
    #[serde(rename = "created_at", default)]
    pub created_at: Option<DateTime<Utc>>,
    /// Owning user.
    #[serde(default)]
    pub owner: String,
    /// Owning group.
    #[serde(default)]
    pub group: String,
    /// Permission bits.
    #[serde(default)]
    pub mode: u32,
}

/// Ownership and mode for one path.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Permission {
    /// Owning user.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub owner: Option<String>,
    /// Owning group.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub group: Option<String>,
    /// Octal digits encoded as a decimal integer, for example `755`.
    pub mode: u32,
}

/// One move/rename operation.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MoveItem {
    /// Source path.
    pub src: String,
    /// Destination path.
    pub dest: String,
}

/// One literal replacement operation.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ReplaceItem {
    /// Text to find.
    pub old: String,
    /// Replacement text.
    pub new: String,
}

/// Per-path replacement count.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct ReplaceResult {
    /// Number of replacements performed.
    #[serde(rename = "replacedCount", default)]
    pub replaced_count: u32,
}

/// Metadata sent alongside an upload.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct FileMetadata {
    /// Destination path inside the sandbox.
    pub path: String,
    /// Owning user.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub owner: Option<String>,
    /// Owning group.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub group: Option<String>,
    /// Octal digits encoded as a decimal integer.
    #[serde(default, skip_serializing_if = "is_zero_u32")]
    pub mode: u32,
}

fn is_zero_u32(value: &u32) -> bool {
    *value == 0
}

/// System resource usage.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Metrics {
    /// Logical CPU count.
    #[serde(default)]
    pub cpu_count: f64,
    /// CPU utilization percentage.
    #[serde(default)]
    pub cpu_used_pct: f64,
    /// Total memory in MiB.
    #[serde(default)]
    pub mem_total_mib: f64,
    /// Used memory in MiB.
    #[serde(default)]
    pub mem_used_mib: f64,
    /// Sample timestamp in milliseconds.
    #[serde(default)]
    pub timestamp: i64,
}

// ---------------------------------------------------------------------------
// Egress credential-vault models
// ---------------------------------------------------------------------------

/// Write-only credential material.
///
/// `type` defaults to `inline`, the only source kind the sidecar accepts today,
/// so the natural zero-value form works.
#[derive(Debug, Clone, Deserialize)]
pub struct InlineCredentialSource {
    /// Source kind. Defaults to `inline`.
    #[serde(rename = "type", default = "inline_source_type")]
    pub source_type: String,
    /// Plaintext secret. Never returned by the sidecar.
    pub value: String,
}

impl Serialize for InlineCredentialSource {
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;

        let mut state = serializer.serialize_struct("InlineCredentialSource", 2)?;
        let source_type = if self.source_type.is_empty() {
            "inline"
        } else {
            self.source_type.as_str()
        };
        state.serialize_field("type", source_type)?;
        state.serialize_field("value", &self.value)?;
        state.end()
    }
}

fn inline_source_type() -> String {
    "inline".to_string()
}

/// One credential stored in the sandbox-local vault.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Credential {
    /// Credential name, referenced by bindings.
    pub name: String,
    /// Write-only material.
    pub source: InlineCredentialSource,
}

/// Which requests a binding applies to.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CredentialMatch {
    /// `https` or `http`. The port is derived from the scheme.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub schemes: Option<Vec<String>>,
    /// Deprecated and ignored by the sidecar.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ports: Option<Vec<u16>>,
    /// Host patterns.
    #[serde(default)]
    pub hosts: Vec<String>,
    /// HTTP methods.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub methods: Option<Vec<String>>,
    /// Request path patterns.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub paths: Option<Vec<String>>,
}

/// One header injected by a `customHeaders` binding.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CustomHeaderEntry {
    /// Header name.
    pub name: String,
    /// Credential to read the value from.
    pub credential: String,
}

/// One scoped literal placeholder replacement.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CredentialSubstitution {
    /// Credential injected at the placeholder.
    pub credential: String,
    /// Literal placeholder to replace.
    pub placeholder: String,
    /// Surfaces where the replacement applies (`path`, `query`, `header`,
    /// `body`).
    pub r#in: Vec<String>,
}

/// How a binding injects credential material.
///
/// `auth_type` is one of `bearer`, `basic`, `apiKey`, `customHeaders`, or
/// `passthrough`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CredentialAuth {
    /// Injection strategy.
    #[serde(rename = "type")]
    pub auth_type: String,
    /// Credential referenced by `bearer`/`basic`/`apiKey`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub credential: Option<String>,
    /// Header or user name, depending on the strategy.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    /// Header rules for `customHeaders`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub headers: Option<Vec<CustomHeaderEntry>>,
    /// Placeholder rules.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub substitutions: Option<Vec<CredentialSubstitution>>,
}

/// A sandbox-local binding between a request selector and a credential.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CredentialBinding {
    /// Binding name.
    pub name: String,
    /// Request selector.
    pub r#match: CredentialMatch,
    /// Injection rules.
    pub auth: CredentialAuth,
}

/// Body of `POST /credential-vault`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CredentialVaultCreateRequest {
    /// Initial credentials.
    #[serde(default)]
    pub credentials: Vec<Credential>,
    /// Initial bindings.
    #[serde(default)]
    pub bindings: Vec<CredentialBinding>,
}

/// Atomic credential changes for a vault patch.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CredentialMutationSet {
    /// New credentials.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub add: Option<Vec<Credential>>,
    /// Full replacements by name.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub replace: Option<Vec<Credential>>,
    /// Names to remove.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub delete: Option<Vec<String>>,
}

/// Atomic binding changes for a vault patch.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CredentialBindingMutationSet {
    /// New bindings.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub add: Option<Vec<CredentialBinding>>,
    /// Full replacements by name.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub replace: Option<Vec<CredentialBinding>>,
    /// Names to remove.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub delete: Option<Vec<String>>,
}

/// Body of `PATCH /credential-vault`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CredentialVaultPatchRequest {
    /// Optimistic concurrency guard.
    #[serde(
        rename = "expectedRevision",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub expected_revision: Option<i64>,
    /// Credential mutations.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub credentials: Option<CredentialMutationSet>,
    /// Binding mutations.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub bindings: Option<CredentialBindingMutationSet>,
}

/// Sanitized credential metadata.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CredentialMetadata {
    /// Credential name.
    pub name: String,
    /// Source kind.
    #[serde(rename = "sourceType", default)]
    pub source_type: String,
    /// Vault revision that last changed it.
    #[serde(default)]
    pub revision: i64,
}

/// Sanitized auth metadata; never includes credential references or values.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CredentialAuthMetadata {
    /// Injection strategy.
    #[serde(rename = "type", default)]
    pub auth_type: String,
    /// Header or user name, when applicable.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
}

/// Sanitized binding metadata.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CredentialBindingMetadata {
    /// Binding name.
    pub name: String,
    /// Vault revision that last changed it.
    #[serde(default)]
    pub revision: i64,
    /// Request selector.
    #[serde(rename = "match", default, skip_serializing_if = "Option::is_none")]
    pub match_rule: Option<CredentialMatch>,
    /// Auth metadata.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub auth: Option<CredentialAuthMetadata>,
}

/// Sanitized vault state.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CredentialVaultState {
    /// Current revision.
    #[serde(default)]
    pub revision: i64,
    /// Credentials in the vault.
    #[serde(default)]
    pub credentials: Vec<CredentialMetadata>,
    /// Bindings in the vault.
    #[serde(default)]
    pub bindings: Vec<CredentialBindingMetadata>,
}

/// Response of `GET /credential-vault/credentials`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CredentialListResponse {
    /// Current revision.
    #[serde(default)]
    pub revision: i64,
    /// Credentials in the vault.
    #[serde(default)]
    pub credentials: Vec<CredentialMetadata>,
}

/// Response of `GET /credential-vault/bindings`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CredentialBindingListResponse {
    /// Current revision.
    #[serde(default)]
    pub revision: i64,
    /// Bindings in the vault.
    #[serde(default)]
    pub bindings: Vec<CredentialBindingMetadata>,
}
