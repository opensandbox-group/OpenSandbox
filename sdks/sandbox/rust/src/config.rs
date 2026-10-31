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

//! Connection configuration for the lifecycle and data-plane clients.

use std::collections::BTreeMap;
use std::time::Duration;

use crate::error::{Error, Result};

/// Default OpenSandbox server address, used when no domain is configured.
pub const DEFAULT_DOMAIN: &str = "localhost:8080";
/// Default scheme used to reach the lifecycle server.
pub const DEFAULT_PROTOCOL: &str = "http";
/// Port the in-sandbox execd listens on.
pub const DEFAULT_EXECD_PORT: u16 = 44772;
/// Port the in-sandbox egress sidecar listens on.
pub const DEFAULT_EGRESS_PORT: u16 = 18080;
/// Default sandbox TTL, in seconds, applied by [`crate::sandbox::CreateOptions`].
pub const DEFAULT_TIMEOUT_SECONDS: u64 = 600;
/// Default timeout for non-streaming lifecycle/data-plane requests.
pub const DEFAULT_REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
/// Default budget for waiting for a sandbox to become usable.
pub const DEFAULT_READY_TIMEOUT: Duration = Duration::from_secs(30);
/// Default interval between readiness probes.
pub const DEFAULT_HEALTH_CHECK_INTERVAL: Duration = Duration::from_millis(200);
/// Default endpoint cache entry lifetime.
pub const DEFAULT_ENDPOINT_CACHE_TTL: Duration = Duration::from_secs(600);
/// Default endpoint cache capacity.
pub const DEFAULT_ENDPOINT_CACHE_SIZE: usize = 1024;

/// Authentication header read by the lifecycle server.
pub const API_KEY_HEADER: &str = "OPEN-SANDBOX-API-KEY";
/// Authentication header read by execd.
pub const EXECD_TOKEN_HEADER: &str = "X-EXECD-ACCESS-TOKEN";
/// Authentication header read by the egress sidecar.
pub const EGRESS_AUTH_HEADER: &str = "OPENSANDBOX-EGRESS-AUTH";
/// Response header reporting what backs an endpoint's sandbox.
pub const SANDBOX_ORIGIN_HEADER: &str = "Open-Sandbox-Origin";
/// Response header set by the gateway when a request was parked by wake-on-access.
pub const WAKE_MARKER_HEADER: &str = "OpenSandbox-Wake";
/// Wake marker value meaning "the request was never forwarded".
pub const WAKE_MARKER_EXPIRED: &str = "expired";

/// Retry policy shared by every request issued by the SDK.
///
/// The defaults mirror the other OpenSandbox SDKs: transient status codes are
/// retried for idempotent methods only, and a `503` carrying
/// `OpenSandbox-Wake: expired` is retried for any method because the gateway
/// guarantees the request was never forwarded.
#[derive(Debug, Clone)]
pub struct RetryConfig {
    /// Maximum number of generic retries (not counting wake retries).
    pub max_retries: u32,
    /// First backoff delay.
    pub initial_backoff: Duration,
    /// Upper bound for a single backoff delay.
    pub max_backoff: Duration,
    /// Exponential growth factor applied per attempt.
    pub multiplier: f64,
    /// Randomization applied to each delay, expressed as a fraction (0.25 = +-25%).
    pub jitter: f64,
    /// Status codes retried for idempotent methods.
    pub retryable_status_codes: Vec<u16>,
    /// Status codes retried for non-idempotent methods (POST/PATCH).
    pub retryable_status_codes_non_idempotent: Vec<u16>,
    /// Whether the OSEP-0024 wake contract is honored.
    pub wake_retry_enabled: bool,
    /// Maximum wake retries.
    pub wake_max_attempts: u32,
    /// Overall budget for wake retries. Zero means the SDK default.
    pub wake_overall_deadline: Duration,
}

impl Default for RetryConfig {
    fn default() -> Self {
        Self {
            max_retries: 3,
            initial_backoff: Duration::from_millis(500),
            max_backoff: Duration::from_secs(30),
            multiplier: 2.0,
            jitter: 0.25,
            retryable_status_codes: vec![429, 502, 503, 504],
            retryable_status_codes_non_idempotent: Vec::new(),
            wake_retry_enabled: true,
            wake_max_attempts: 5,
            wake_overall_deadline: Duration::from_secs(60),
        }
    }
}

impl RetryConfig {
    /// Disables every retry, including the wake contract.
    pub fn disabled() -> Self {
        Self {
            max_retries: 0,
            wake_retry_enabled: false,
            retryable_status_codes: Vec::new(),
            ..Self::default()
        }
    }

    pub(crate) fn is_retryable(&self, status: u16, idempotent: bool) -> bool {
        if idempotent {
            self.retryable_status_codes.contains(&status)
        } else {
            self.retryable_status_codes_non_idempotent.contains(&status)
        }
    }
}

/// Connection settings for one OpenSandbox control plane.
///
/// `ConnectionConfig` is cheap to clone and is shared by every client derived
/// from it (lifecycle, execd, egress).
#[derive(Debug, Clone)]
pub struct ConnectionConfig {
    domain: Option<String>,
    protocol: Option<String>,
    api_key: Option<String>,
    use_server_proxy: bool,
    request_timeout: Duration,
    headers: BTreeMap<String, String>,
    auth_header: String,
    retry: RetryConfig,
    endpoint_host_rewrite: Vec<(String, String)>,
    endpoint_cache_ttl: Duration,
    endpoint_cache_size: usize,
    endpoint_cache_disabled: bool,
}

impl Default for ConnectionConfig {
    fn default() -> Self {
        Self::new()
    }
}

impl ConnectionConfig {
    /// Creates a configuration that resolves its domain, protocol, and API key
    /// from the environment (`OPEN_SANDBOX_DOMAIN`, `OPEN_SANDBOX_PROTOCOL`,
    /// `OPEN_SANDBOX_API_KEY`), falling back to the SDK defaults.
    pub fn new() -> Self {
        Self {
            domain: None,
            protocol: None,
            api_key: None,
            use_server_proxy: false,
            request_timeout: DEFAULT_REQUEST_TIMEOUT,
            headers: BTreeMap::new(),
            auth_header: API_KEY_HEADER.to_string(),
            retry: RetryConfig::default(),
            endpoint_host_rewrite: Vec::new(),
            endpoint_cache_ttl: DEFAULT_ENDPOINT_CACHE_TTL,
            endpoint_cache_size: DEFAULT_ENDPOINT_CACHE_SIZE,
            endpoint_cache_disabled: false,
        }
    }

    /// Sets the lifecycle server address, for example `localhost:8080` or
    /// `https://sandbox.internal`.
    pub fn with_domain(mut self, domain: impl Into<String>) -> Self {
        self.domain = Some(domain.into());
        self
    }

    /// Sets the scheme used for requests. Ignored when the domain already
    /// carries a scheme.
    pub fn with_protocol(mut self, protocol: impl Into<String>) -> Self {
        self.protocol = Some(protocol.into());
        self
    }

    /// Sets the tenant API key sent as `OPEN-SANDBOX-API-KEY`.
    pub fn with_api_key(mut self, api_key: impl Into<String>) -> Self {
        self.api_key = Some(api_key.into());
        self
    }

    /// Routes execd/egress traffic through the lifecycle server proxy instead
    /// of connecting to the sandbox directly.
    pub fn with_server_proxy(mut self, enabled: bool) -> Self {
        self.use_server_proxy = enabled;
        self
    }

    /// Sets the timeout for non-streaming requests. Streaming calls are only
    /// bounded by response headers, matching the other SDKs.
    pub fn with_request_timeout(mut self, timeout: Duration) -> Self {
        self.request_timeout = timeout;
        self
    }

    /// Adds a header applied to every request.
    pub fn with_header(mut self, name: impl Into<String>, value: impl Into<String>) -> Self {
        self.headers.insert(name.into(), value.into());
        self
    }

    /// Overrides the lifecycle authentication header name.
    pub fn with_auth_header(mut self, name: impl Into<String>) -> Self {
        self.auth_header = name.into();
        self
    }

    /// Replaces the retry policy.
    pub fn with_retry(mut self, retry: RetryConfig) -> Self {
        self.retry = retry;
        self
    }

    /// Rewrites endpoint hosts before connecting, useful when the server
    /// reports an address that is not reachable from the client.
    pub fn with_endpoint_host_rewrite(
        mut self,
        from: impl Into<String>,
        to: impl Into<String>,
    ) -> Self {
        self.endpoint_host_rewrite.push((from.into(), to.into()));
        self
    }

    /// Overrides the endpoint cache lifetime.
    pub fn with_endpoint_cache_ttl(mut self, ttl: Duration) -> Self {
        self.endpoint_cache_ttl = ttl;
        self
    }

    /// Overrides the endpoint cache capacity.
    pub fn with_endpoint_cache_size(mut self, size: usize) -> Self {
        self.endpoint_cache_size = size;
        self
    }

    /// Disables endpoint caching entirely.
    pub fn without_endpoint_cache(mut self) -> Self {
        self.endpoint_cache_disabled = true;
        self
    }

    /// Returns the configured domain, falling back to `OPEN_SANDBOX_DOMAIN`.
    pub fn domain(&self) -> String {
        self.domain
            .clone()
            .or_else(|| std::env::var("OPEN_SANDBOX_DOMAIN").ok())
            .filter(|v| !v.is_empty())
            .unwrap_or_else(|| DEFAULT_DOMAIN.to_string())
    }

    /// Returns the configured scheme, falling back to `OPEN_SANDBOX_PROTOCOL`.
    pub fn protocol(&self) -> String {
        self.protocol
            .clone()
            .or_else(|| std::env::var("OPEN_SANDBOX_PROTOCOL").ok())
            .filter(|v| !v.is_empty())
            .unwrap_or_else(|| DEFAULT_PROTOCOL.to_string())
    }

    /// Returns the configured API key, falling back to `OPEN_SANDBOX_API_KEY`.
    pub fn api_key(&self) -> Option<String> {
        self.api_key
            .clone()
            .or_else(|| std::env::var("OPEN_SANDBOX_API_KEY").ok())
            .filter(|v| !v.is_empty())
    }

    /// Whether data-plane traffic goes through the lifecycle server proxy.
    pub fn use_server_proxy(&self) -> bool {
        self.use_server_proxy
    }

    /// Timeout applied to non-streaming requests.
    pub fn request_timeout(&self) -> Duration {
        self.request_timeout
    }

    /// Custom headers applied to every request.
    pub fn headers(&self) -> &BTreeMap<String, String> {
        &self.headers
    }

    /// Merges connection-level and endpoint-level headers for data-plane
    /// requests, with the endpoint headers taking precedence.
    ///
    /// Direct-mode traffic terminates inside the sandbox, so the tenant API key
    /// is stripped: it must never be forwarded into the sandbox. In server-proxy
    /// mode the key is what authenticates the proxy, so it is kept.
    pub(crate) fn data_plane_headers(
        &self,
        endpoint: &BTreeMap<String, String>,
    ) -> BTreeMap<String, String> {
        let mut headers = self.headers.clone();
        for (name, value) in endpoint {
            headers.insert(name.clone(), value.clone());
        }
        if !self.use_server_proxy {
            headers.retain(|name, _| !name.eq_ignore_ascii_case(API_KEY_HEADER));
        }
        headers
    }

    /// Header name carrying the lifecycle API key.
    pub fn auth_header(&self) -> &str {
        &self.auth_header
    }

    /// Retry policy.
    pub fn retry(&self) -> &RetryConfig {
        &self.retry
    }

    /// Endpoint cache TTL.
    pub fn endpoint_cache_ttl(&self) -> Duration {
        self.endpoint_cache_ttl
    }

    /// Endpoint cache capacity.
    pub fn endpoint_cache_size(&self) -> usize {
        self.endpoint_cache_size
    }

    /// Whether endpoint caching is disabled.
    pub fn endpoint_cache_disabled(&self) -> bool {
        self.endpoint_cache_disabled
    }

    /// Builds `{protocol}://{domain}`, or returns the domain unchanged when it
    /// already includes a scheme.
    pub fn base_url(&self) -> String {
        let domain = self.domain();
        if domain.starts_with("http://") || domain.starts_with("https://") {
            domain.trim_end_matches('/').to_string()
        } else {
            format!(
                "{}://{}",
                self.protocol(),
                domain.trim_end_matches('/').trim_start_matches("//")
            )
        }
    }

    /// Builds the lifecycle API root (`{base}/v1`).
    pub fn lifecycle_url(&self) -> String {
        format!("{}/v1", self.base_url())
    }

    /// Applies configured host rewrites to an endpoint reported by the server.
    pub fn rewrite_endpoint_url(&self, endpoint: &str) -> String {
        let mut rewritten = endpoint.to_string();
        for (from, to) in &self.endpoint_host_rewrite {
            if !from.is_empty() {
                rewritten = rewritten.replace(from, to);
            }
        }
        rewritten
    }

    /// Normalizes an endpoint into an absolute URL.
    pub fn endpoint_url(&self, endpoint: &str) -> Result<String> {
        let rewritten = self.rewrite_endpoint_url(endpoint);
        if rewritten.starts_with("http://") || rewritten.starts_with("https://") {
            Ok(rewritten)
        } else if rewritten.is_empty() {
            Err(Error::invalid_argument("endpoint", "endpoint is empty"))
        } else {
            Ok(format!("{}://{}", self.protocol(), rewritten))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base_url_adds_scheme() {
        let cfg = ConnectionConfig::new()
            .with_domain("localhost:8080")
            .with_protocol("http");
        assert_eq!(cfg.base_url(), "http://localhost:8080");
        assert_eq!(cfg.lifecycle_url(), "http://localhost:8080/v1");
    }

    #[test]
    fn base_url_keeps_existing_scheme() {
        let cfg = ConnectionConfig::new().with_domain("https://sandbox.example.com/");
        assert_eq!(cfg.base_url(), "https://sandbox.example.com");
        assert_eq!(cfg.lifecycle_url(), "https://sandbox.example.com/v1");
    }

    #[test]
    fn endpoint_url_and_rewrite() {
        let cfg = ConnectionConfig::new()
            .with_domain("localhost:8080")
            .with_endpoint_host_rewrite("127.0.0.1", "sandbox.local");
        assert_eq!(
            cfg.endpoint_url("127.0.0.1:44772").unwrap(),
            "http://sandbox.local:44772"
        );
        assert_eq!(
            cfg.endpoint_url("https://1.2.3.4:8080").unwrap(),
            "https://1.2.3.4:8080"
        );
    }

    #[test]
    fn retry_defaults_match_other_sdks() {
        let retry = RetryConfig::default();
        assert_eq!(retry.max_retries, 3);
        assert_eq!(retry.retryable_status_codes, vec![429, 502, 503, 504]);
        assert!(retry.retryable_status_codes_non_idempotent.is_empty());
        assert!(retry.wake_retry_enabled);
    }
}
