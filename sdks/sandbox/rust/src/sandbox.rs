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

//! High-level sandbox facade: create/connect, lifecycle operations, and execd.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::{Duration, Instant};

use chrono::Utc;

use crate::config::{
    ConnectionConfig, DEFAULT_EXECD_PORT, DEFAULT_HEALTH_CHECK_INTERVAL, DEFAULT_READY_TIMEOUT,
    DEFAULT_TIMEOUT_SECONDS,
};
use crate::egress::EgressClient;
use crate::error::{Error, Result};
use crate::execd::ExecdClient;
use crate::execution::Execution;
use crate::lifecycle::LifecycleClient;
use crate::models::{
    CreateSandboxRequest, CreateSnapshotRequest, CredentialProxyConfig, Endpoint, ImageAuth,
    ImageSpec, Metadata, NetworkPolicy, PlatformSpec, RenewExpirationResponse, RunCommandRequest,
    SandboxInfo, SandboxLifecycle, SandboxState, SnapshotInfo, Volume,
};

/// Default entrypoint keeps a sandbox alive for interactive use.
pub fn default_entrypoint() -> Vec<String> {
    vec!["tail".into(), "-f".into(), "/dev/null".into()]
}

/// Default resource limits, matching the other SDKs.
pub fn default_resource_limits() -> crate::models::ResourceLimits {
    BTreeMap::from([
        ("cpu".to_string(), "1".to_string()),
        ("memory".to_string(), "2Gi".to_string()),
    ])
}

/// What backs a sandbox.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SandboxOrigin {
    /// Runs on a fast-sandbox golden-image template. Egress policy operations
    /// for these sandboxes go through the lifecycle control plane.
    Template,
    /// Created from an image or snapshot, or reported by an older server.
    Unknown,
}

impl SandboxOrigin {
    fn from_header(value: Option<&str>) -> Self {
        match value {
            Some("template") => Self::Template,
            _ => Self::Unknown,
        }
    }
}

/// Options for [`Sandbox::create`].
#[derive(Debug, Clone)]
pub struct CreateOptions {
    image: Option<String>,
    snapshot_id: Option<String>,
    template_id: Option<String>,
    image_auth: Option<ImageAuth>,
    entrypoint: Vec<String>,
    resource_limits: Option<crate::models::ResourceLimits>,
    resource_requests: Option<crate::models::ResourceLimits>,
    timeout_seconds: Option<u64>,
    manual_cleanup: bool,
    env: BTreeMap<String, String>,
    metadata: Metadata,
    network_policy: Option<NetworkPolicy>,
    volumes: Vec<Volume>,
    platform: Option<PlatformSpec>,
    extensions: BTreeMap<String, String>,
    lifecycle: Option<SandboxLifecycle>,
    credential_proxy: Option<CredentialProxyConfig>,
    secure_access: bool,
    skip_health_check: bool,
    ready_timeout: Duration,
    poll_interval: Duration,
}

impl CreateOptions {
    fn empty() -> Self {
        Self {
            image: None,
            snapshot_id: None,
            template_id: None,
            image_auth: None,
            entrypoint: Vec::new(),
            resource_limits: None,
            resource_requests: None,
            timeout_seconds: None,
            manual_cleanup: false,
            env: BTreeMap::new(),
            metadata: BTreeMap::new(),
            network_policy: None,
            volumes: Vec::new(),
            platform: None,
            extensions: BTreeMap::new(),
            lifecycle: None,
            credential_proxy: None,
            secure_access: false,
            skip_health_check: false,
            ready_timeout: DEFAULT_READY_TIMEOUT,
            poll_interval: DEFAULT_HEALTH_CHECK_INTERVAL,
        }
    }

    /// Creates from a container image.
    pub fn from_image(image: impl Into<String>) -> Self {
        Self {
            image: Some(image.into()),
            ..Self::empty()
        }
    }

    /// Restores from a previously captured snapshot.
    pub fn from_snapshot(snapshot_id: impl Into<String>) -> Self {
        Self {
            snapshot_id: Some(snapshot_id.into()),
            ..Self::empty()
        }
    }

    /// Creates from a fast-sandbox template. A timeout is required in this
    /// mode; workload shape comes from the template.
    pub fn from_template(template_id: impl Into<String>) -> Self {
        Self {
            template_id: Some(template_id.into()),
            ..Self::empty()
        }
    }

    /// Registry credentials for a private image.
    pub fn with_image_auth(
        mut self,
        username: impl Into<String>,
        password: impl Into<String>,
    ) -> Self {
        self.image_auth = Some(ImageAuth {
            username: username.into(),
            password: password.into(),
        });
        self
    }

    /// Overrides the entrypoint argv.
    pub fn with_entrypoint<I, S>(mut self, entrypoint: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        self.entrypoint = entrypoint.into_iter().map(Into::into).collect();
        self
    }

    /// Overrides resource limits (for example `cpu=500m`).
    pub fn with_resource_limit(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.resource_limits
            .get_or_insert_with(BTreeMap::new)
            .insert(key.into(), value.into());
        self
    }

    /// Sets resource requests (Kubernetes Burstable QoS).
    pub fn with_resource_request(
        mut self,
        key: impl Into<String>,
        value: impl Into<String>,
    ) -> Self {
        self.resource_requests
            .get_or_insert_with(BTreeMap::new)
            .insert(key.into(), value.into());
        self
    }

    /// Sets the TTL in seconds. Omit for the SDK default (600s).
    pub fn with_timeout_seconds(mut self, seconds: u64) -> Self {
        self.timeout_seconds = Some(seconds);
        self
    }

    /// Creates without a TTL; the sandbox lives until deleted.
    pub fn with_manual_cleanup(mut self) -> Self {
        self.manual_cleanup = true;
        self
    }

    /// Adds an environment variable.
    pub fn with_env(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.env.insert(key.into(), value.into());
        self
    }

    /// Adds metadata used for filtering.
    pub fn with_metadata(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.metadata.insert(key.into(), value.into());
        self
    }

    /// Applies an egress policy.
    pub fn with_network_policy(mut self, policy: NetworkPolicy) -> Self {
        self.network_policy = Some(policy);
        self
    }

    /// Mounts a volume.
    pub fn with_volume(mut self, volume: Volume) -> Self {
        self.volumes.push(volume);
        self
    }

    /// Constrains the runtime platform.
    pub fn with_platform(mut self, os: impl Into<String>, arch: impl Into<String>) -> Self {
        self.platform = Some(PlatformSpec {
            os: os.into(),
            arch: arch.into(),
        });
        self
    }

    /// Adds a provider-specific extension.
    pub fn with_extension(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.extensions.insert(key.into(), value.into());
        self
    }

    /// Applies lifecycle hooks and policies.
    pub fn with_lifecycle(mut self, lifecycle: SandboxLifecycle) -> Self {
        self.lifecycle = Some(lifecycle);
        self
    }

    /// Enables the credential-vault transparent proxy.
    pub fn with_credential_proxy(mut self) -> Self {
        self.credential_proxy = Some(CredentialProxyConfig { enabled: true });
        self
    }

    /// Requests secure access endpoints.
    pub fn with_secure_access(mut self) -> Self {
        self.secure_access = true;
        self
    }

    /// Skips the post-create health check.
    pub fn skip_health_check(mut self) -> Self {
        self.skip_health_check = true;
        self
    }

    /// Overrides the post-create readiness budget.
    pub fn with_ready_timeout(mut self, timeout: Duration) -> Self {
        self.ready_timeout = timeout;
        self
    }

    /// Overrides the readiness polling interval.
    pub fn with_poll_interval(mut self, interval: Duration) -> Self {
        self.poll_interval = interval;
        self
    }

    /// Builds the wire request, applying SDK defaults.
    pub fn to_request(&self) -> Result<CreateSandboxRequest> {
        if let Some(template_id) = &self.template_id {
            if template_id.is_empty() {
                return Err(Error::invalid_argument(
                    "templateId",
                    "template id is required",
                ));
            }
            if self.image.is_some() || self.snapshot_id.is_some() {
                return Err(Error::invalid_argument(
                    "templateId",
                    "templateId is mutually exclusive with image and snapshotId",
                ));
            }
            let timeout = self.timeout_seconds.ok_or_else(|| {
                Error::invalid_argument("timeout", "a timeout is required in template mode")
            })?;
            return Ok(CreateSandboxRequest {
                template_id: Some(template_id.clone()),
                timeout: Some(timeout),
                metadata: non_empty(self.metadata.clone()),
                network_policy: self.network_policy.clone(),
                extensions: non_empty(self.extensions.clone()),
                lifecycle: self.lifecycle.clone(),
                ..CreateSandboxRequest::default()
            });
        }

        match (&self.image, &self.snapshot_id) {
            (Some(_), Some(_)) | (None, None) => {
                return Err(Error::invalid_argument(
                    "image/snapshotId",
                    "exactly one of image or snapshotId is required",
                ))
            }
            _ => {}
        }

        let entrypoint = if self.entrypoint.is_empty() {
            default_entrypoint()
        } else {
            self.entrypoint.clone()
        };
        let limits = self
            .resource_limits
            .clone()
            .unwrap_or_else(default_resource_limits);
        let timeout = if self.manual_cleanup {
            None
        } else {
            Some(self.timeout_seconds.unwrap_or(DEFAULT_TIMEOUT_SECONDS))
        };

        Ok(CreateSandboxRequest {
            image: self.image.as_ref().map(|uri| ImageSpec {
                uri: uri.clone(),
                auth: self.image_auth.clone(),
            }),
            snapshot_id: self.snapshot_id.clone(),
            template_id: None,
            timeout,
            resource_limits: Some(limits),
            resource_requests: self.resource_requests.clone(),
            env: non_empty(self.env.clone()),
            secure_access: self.secure_access,
            metadata: non_empty(self.metadata.clone()),
            lifecycle: self.lifecycle.clone(),
            entrypoint: Some(entrypoint),
            network_policy: self.network_policy.clone(),
            credential_proxy: self.credential_proxy.clone(),
            volumes: if self.volumes.is_empty() {
                None
            } else {
                Some(self.volumes.clone())
            },
            extensions: non_empty(self.extensions.clone()),
            platform: self.platform.clone(),
        })
    }
}

fn non_empty<T>(map: BTreeMap<String, T>) -> Option<BTreeMap<String, T>> {
    if map.is_empty() {
        None
    } else {
        Some(map)
    }
}

/// Options for waiting until a connected sandbox is usable.
#[derive(Debug, Clone)]
pub struct ReadyOptions {
    /// Overall budget.
    pub timeout: Duration,
    /// Interval between probe attempts.
    pub polling_interval: Duration,
    /// Skip the execd `/ping` probe.
    pub skip_health_check: bool,
}

impl Default for ReadyOptions {
    fn default() -> Self {
        Self {
            timeout: DEFAULT_READY_TIMEOUT,
            polling_interval: DEFAULT_HEALTH_CHECK_INTERVAL,
            skip_health_check: false,
        }
    }
}

/// A live sandbox handle.
///
/// Cloning is cheap; every handle points at the same sandbox. After
/// [`Sandbox::pause`] the execd client is stale: call
/// [`Sandbox::resume_sandbox`] to obtain a handle bound to the new endpoint.
#[derive(Clone)]
pub struct Sandbox {
    id: String,
    config: Arc<ConnectionConfig>,
    lifecycle: LifecycleClient,
    execd: ExecdClient,
    origin: SandboxOrigin,
}

impl Sandbox {
    /// Creates a sandbox and waits until execd answers.
    pub async fn create(config: ConnectionConfig, options: CreateOptions) -> Result<Self> {
        let lifecycle = LifecycleClient::new(&config)?;
        let request = options.to_request()?;
        let created = lifecycle.create_sandbox(&request).await?;
        Self::finish(config, lifecycle, created, &options).await
    }

    /// Connects to an existing sandbox by identifier.
    pub async fn connect(config: ConnectionConfig, sandbox_id: impl Into<String>) -> Result<Self> {
        Self::connect_with(config, sandbox_id, ReadyOptions::default()).await
    }

    /// Connects with explicit readiness options.
    pub async fn connect_with(
        config: ConnectionConfig,
        sandbox_id: impl Into<String>,
        options: ReadyOptions,
    ) -> Result<Self> {
        let sandbox_id = sandbox_id.into();
        if sandbox_id.is_empty() {
            return Err(Error::invalid_argument(
                "sandboxId",
                "sandbox id is required",
            ));
        }
        let lifecycle = LifecycleClient::new(&config)?;
        let deadline = Instant::now() + options.timeout;

        loop {
            match Self::resolve(&config, &lifecycle, &sandbox_id).await {
                Ok((execd, origin)) => {
                    let sandbox = Self {
                        id: sandbox_id.clone(),
                        config: Arc::new(config.clone()),
                        lifecycle,
                        execd,
                        origin,
                    };
                    if !options.skip_health_check {
                        sandbox
                            .wait_until_ready(options.timeout, options.polling_interval)
                            .await?;
                    }
                    return Ok(sandbox);
                }
                Err(err) => {
                    // A pod that exists but has no IP yet is retryable.
                    let retryable = err
                        .api()
                        .map(|api| {
                            api.is_not_found() && api.code == "KUBERNETES::POD_IP_NOT_AVAILABLE"
                        })
                        .unwrap_or(false);
                    if !retryable || Instant::now() >= deadline {
                        return Err(err);
                    }
                    tokio::time::sleep(options.polling_interval).await;
                }
            }
        }
    }

    /// Resumes a paused sandbox and reconnects to it.
    pub async fn resume(config: ConnectionConfig, sandbox_id: impl Into<String>) -> Result<Self> {
        let sandbox_id = sandbox_id.into();
        let lifecycle = LifecycleClient::new(&config)?;
        lifecycle.resume_sandbox(&sandbox_id).await?;
        lifecycle.invalidate_endpoints(&sandbox_id);
        Self::connect(config, sandbox_id).await
    }

    async fn finish(
        config: ConnectionConfig,
        lifecycle: LifecycleClient,
        created: SandboxInfo,
        options: &CreateOptions,
    ) -> Result<Self> {
        let id = created.id.clone();

        let result = async {
            wait_for_running(
                &lifecycle,
                &id,
                options.ready_timeout,
                options.poll_interval,
            )
            .await?;
            let (execd, origin) = Self::resolve(&config, &lifecycle, &id).await?;
            let sandbox = Self {
                id: id.clone(),
                config: Arc::new(config.clone()),
                lifecycle: lifecycle.clone(),
                execd,
                origin,
            };
            if !options.skip_health_check {
                sandbox
                    .wait_until_ready(options.ready_timeout, options.poll_interval)
                    .await?;
            }
            Ok(sandbox)
        }
        .await;

        if result.is_err() {
            // Best-effort cleanup so a failed create does not leak a sandbox.
            let _ = lifecycle.delete_sandbox(&id).await;
        }
        result
    }

    async fn resolve(
        config: &ConnectionConfig,
        lifecycle: &LifecycleClient,
        sandbox_id: &str,
    ) -> Result<(ExecdClient, SandboxOrigin)> {
        let endpoint = lifecycle
            .endpoint(
                sandbox_id,
                DEFAULT_EXECD_PORT,
                Some(config.use_server_proxy()),
            )
            .await?;
        let origin = SandboxOrigin::from_header(endpoint.origin.as_deref());

        let base = config.endpoint_url(&endpoint.endpoint)?;
        let headers = endpoint_headers(config, &endpoint);
        let execd = ExecdClient::new(&base, &headers, config)?;
        Ok((execd, origin))
    }

    /// Sandbox identifier.
    pub fn id(&self) -> &str {
        &self.id
    }

    /// What backs this sandbox.
    pub fn origin(&self) -> &SandboxOrigin {
        &self.origin
    }

    /// Lifecycle client bound to this control plane.
    pub fn lifecycle(&self) -> &LifecycleClient {
        &self.lifecycle
    }

    /// execd client bound to this sandbox.
    pub fn execd(&self) -> &ExecdClient {
        &self.execd
    }

    /// Connection configuration in use.
    pub fn config(&self) -> &ConnectionConfig {
        &self.config
    }

    /// Fetches fresh sandbox information.
    pub async fn info(&self) -> Result<SandboxInfo> {
        self.lifecycle.get_sandbox(&self.id).await
    }

    /// Requests termination. Returns once deletion is accepted.
    pub async fn kill(&self) -> Result<()> {
        self.lifecycle.invalidate_endpoints(&self.id);
        self.lifecycle.delete_sandbox(&self.id).await
    }

    /// Pauses the sandbox, preserving its state.
    pub async fn pause(&self) -> Result<()> {
        self.lifecycle.invalidate_endpoints(&self.id);
        self.lifecycle.pause_sandbox(&self.id).await
    }

    /// Resumes this sandbox and returns a handle bound to the new endpoint.
    pub async fn resume_sandbox(&self) -> Result<Sandbox> {
        Self::resume((*self.config).clone(), self.id.clone()).await
    }

    /// Moves the absolute expiration to `now + duration`.
    pub async fn renew(&self, duration: Duration) -> Result<RenewExpirationResponse> {
        let expires_at = Utc::now()
            + chrono::Duration::from_std(duration)
                .map_err(|_| Error::invalid_argument("duration", "duration is too large"))?;
        self.lifecycle.renew_expiration(&self.id, expires_at).await
    }

    /// Applies a metadata merge patch.
    pub async fn patch_metadata(
        &self,
        patch: &BTreeMap<String, Option<String>>,
    ) -> Result<SandboxInfo> {
        self.lifecycle.patch_metadata(&self.id, patch).await
    }

    /// Resolves the endpoint for an arbitrary sandbox port.
    pub async fn endpoint(&self, port: u16) -> Result<Endpoint> {
        self.lifecycle
            .endpoint(&self.id, port, Some(self.config.use_server_proxy()))
            .await
    }

    /// Resolves a signed endpoint expiring at `expires` (Unix seconds).
    pub async fn signed_endpoint(&self, port: u16, expires: i64) -> Result<Endpoint> {
        self.lifecycle
            .signed_endpoint(&self.id, port, expires)
            .await
    }

    /// Captures a snapshot of this sandbox.
    pub async fn snapshot(&self, name: Option<&str>) -> Result<SnapshotInfo> {
        let request = CreateSnapshotRequest {
            name: name.map(str::to_string),
        };
        self.lifecycle.create_snapshot(&self.id, &request).await
    }

    /// Verifies that execd answers.
    pub async fn ping(&self) -> Result<()> {
        self.execd.ping().await
    }

    /// Whether execd currently answers. Transport errors are reported as
    /// `false` instead of being returned.
    pub async fn is_healthy(&self) -> bool {
        self.ping().await.is_ok()
    }

    /// Resolves the egress sidecar and returns a client for it.
    ///
    /// Template-backed sandboxes have no sandbox-side sidecar; use
    /// [`Sandbox::lifecycle`] network-policy calls for those.
    pub async fn egress(&self) -> Result<EgressClient> {
        let endpoint = self
            .lifecycle
            .endpoint(
                &self.id,
                crate::config::DEFAULT_EGRESS_PORT,
                Some(self.config.use_server_proxy()),
            )
            .await?;
        let base = self.config.endpoint_url(&endpoint.endpoint)?;
        EgressClient::new(
            &base,
            None,
            &endpoint_headers(&self.config, &endpoint),
            &self.config,
        )
    }

    /// Polls execd until it answers or the budget is exhausted.
    pub async fn wait_until_ready(
        &self,
        timeout: Duration,
        polling_interval: Duration,
    ) -> Result<()> {
        let deadline = Instant::now() + timeout;
        loop {
            match self.execd.ping().await {
                Ok(()) => return Ok(()),
                Err(_) if Instant::now() >= deadline => {
                    return Err(Error::ReadyTimeout {
                        sandbox_id: self.id.clone(),
                        elapsed: timeout,
                    });
                }
                Err(_) => tokio::time::sleep(polling_interval).await,
            }
        }
    }

    /// Runs a shell command and returns its accumulated output.
    pub async fn exec(&self, command: impl Into<String>) -> Result<Execution> {
        self.execd
            .run_command_collect(&RunCommandRequest::shell(command))
            .await
    }

    /// Runs a command and returns its raw event stream.
    pub async fn run_command(
        &self,
        request: &RunCommandRequest,
    ) -> Result<crate::execution::ExecutionStream> {
        self.execd.run_command(request).await
    }

    /// Whether this sandbox is template-backed.
    pub fn is_template_backed(&self) -> bool {
        self.origin == SandboxOrigin::Template
    }
}

impl std::fmt::Debug for Sandbox {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Sandbox")
            .field("id", &self.id)
            .field("origin", &self.origin)
            .finish_non_exhaustive()
    }
}

async fn wait_for_running(
    lifecycle: &LifecycleClient,
    sandbox_id: &str,
    timeout: Duration,
    polling_interval: Duration,
) -> Result<()> {
    let started = Instant::now();
    let deadline = started + timeout;
    loop {
        let info = lifecycle.get_sandbox(sandbox_id).await?;
        match info.status.state {
            SandboxState::Running => return Ok(()),
            SandboxState::Failed | SandboxState::Terminated => {
                return Err(Error::TerminalState {
                    sandbox_id: sandbox_id.to_string(),
                    state: format!("{:?}", info.status.state),
                    reason: info
                        .status
                        .reason
                        .or(info.status.message)
                        .unwrap_or_default(),
                })
            }
            _ => {}
        }
        if Instant::now() >= deadline {
            return Err(Error::RunningTimeout {
                sandbox_id: sandbox_id.to_string(),
                elapsed: started.elapsed(),
            });
        }
        tokio::time::sleep(polling_interval.max(Duration::from_millis(50))).await;
    }
}

fn has_header(headers: &BTreeMap<String, String>, name: &str) -> bool {
    headers.keys().any(|key| key.eq_ignore_ascii_case(name))
}

/// Builds the header set for a data-plane client bound to `endpoint`.
///
/// In server-proxy mode the tenant API key authenticates the proxy, so it is
/// added unless the caller already supplied it. Direct-mode clients strip it
/// themselves (see `ConnectionConfig::data_plane_headers`).
fn endpoint_headers(config: &ConnectionConfig, endpoint: &Endpoint) -> BTreeMap<String, String> {
    let mut headers = endpoint.headers.clone();
    if config.use_server_proxy()
        && !has_header(&headers, crate::config::API_KEY_HEADER)
        && !has_header(config.headers(), crate::config::API_KEY_HEADER)
    {
        if let Some(api_key) = config.api_key() {
            headers.insert(crate::config::API_KEY_HEADER.to_string(), api_key);
        }
    }
    headers
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn image_options_apply_defaults() {
        let request = CreateOptions::from_image("python:3.12")
            .to_request()
            .unwrap();
        assert_eq!(
            request.entrypoint.as_ref().unwrap(),
            &vec![
                "tail".to_string(),
                "-f".to_string(),
                "/dev/null".to_string()
            ]
        );
        assert_eq!(request.timeout, Some(DEFAULT_TIMEOUT_SECONDS));
        assert_eq!(
            request
                .resource_limits
                .as_ref()
                .unwrap()
                .get("memory")
                .unwrap(),
            "2Gi"
        );
        assert!(request.image.is_some());
        assert!(request.snapshot_id.is_none());
    }

    #[test]
    fn manual_cleanup_omits_timeout() {
        let request = CreateOptions::from_image("python:3.12")
            .with_manual_cleanup()
            .to_request()
            .unwrap();
        assert!(request.timeout.is_none());
    }

    #[test]
    fn image_and_snapshot_are_exclusive() {
        let mut options = CreateOptions::from_image("python:3.12");
        options.snapshot_id = Some("snap-1".into());
        assert!(options.to_request().is_err());
    }

    #[test]
    fn template_mode_requires_timeout() {
        let options = CreateOptions::from_template("net-tools");
        assert!(options.to_request().is_err());
        let request = options
            .with_timeout_seconds(900)
            .with_metadata("team", "infra")
            .to_request()
            .unwrap();
        assert_eq!(request.timeout, Some(900));
        assert!(request.entrypoint.is_none());
        assert!(request.resource_limits.is_none());
    }

    #[test]
    fn template_mode_rejects_image() {
        let mut options = CreateOptions::from_template("net-tools").with_timeout_seconds(900);
        options.image = Some("python:3.12".into());
        assert!(options.to_request().is_err());
    }

    #[test]
    fn header_lookup_is_case_insensitive() {
        let headers = BTreeMap::from([("open-sandbox-api-key".to_string(), "k".to_string())]);
        assert!(has_header(&headers, "OPEN-SANDBOX-API-KEY"));
    }
}
