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

//! Lifecycle control-plane client (`/v1/sandboxes`, `/v1/snapshots`).

use std::collections::BTreeMap;
use std::sync::Arc;

use chrono::{DateTime, Utc};
use reqwest::header::HeaderMap;
use reqwest::Method;
use url::form_urlencoded;

use crate::config::{ConnectionConfig, API_KEY_HEADER, SANDBOX_ORIGIN_HEADER};
use crate::endpoint_cache::EndpointCache;
use crate::error::Result;
use crate::models::{
    CreateSandboxRequest, CreateSnapshotRequest, Endpoint, ListSandboxesResponse,
    ListSnapshotsResponse, Metadata, NetworkRule, PolicyStatusResponse, RenewExpirationRequest,
    RenewExpirationResponse, SandboxInfo, SandboxState, SnapshotInfo, SnapshotState,
};
use crate::transport::Http;

/// Filters for [`LifecycleClient::list_sandboxes`].
#[derive(Debug, Clone, Default)]
pub struct ListSandboxesQuery {
    /// Match any of these states.
    pub states: Vec<SandboxState>,
    /// Exact-match metadata filter.
    pub metadata: Option<Metadata>,
    /// 1-based page number.
    pub page: Option<u32>,
    /// Page size (server default 20, maximum 100).
    pub page_size: Option<u32>,
}

impl ListSandboxesQuery {
    fn to_query(&self) -> String {
        let mut serializer = form_urlencoded::Serializer::new(String::new());
        for state in &self.states {
            serializer.append_pair("state", state_name(state));
        }
        if let Some(metadata) = &self.metadata {
            if let Ok(json) = serde_json::to_string(metadata) {
                serializer.append_pair("metadata", &json);
            }
        }
        if let Some(page) = self.page {
            serializer.append_pair("page", &page.to_string());
        }
        if let Some(page_size) = self.page_size {
            serializer.append_pair("pageSize", &page_size.to_string());
        }
        let query = serializer.finish();
        if query.is_empty() {
            query
        } else {
            format!("?{query}")
        }
    }
}

/// Filters for [`LifecycleClient::list_snapshots`].
#[derive(Debug, Clone, Default)]
pub struct ListSnapshotsQuery {
    /// Only snapshots captured from this sandbox.
    pub sandbox_id: Option<String>,
    /// Only snapshots with this name.
    pub name: Option<String>,
    /// Match any of these states.
    pub states: Vec<SnapshotState>,
    /// 1-based page number.
    pub page: Option<u32>,
    /// Page size.
    pub page_size: Option<u32>,
}

impl ListSnapshotsQuery {
    fn to_query(&self) -> String {
        let mut serializer = form_urlencoded::Serializer::new(String::new());
        if let Some(sandbox_id) = &self.sandbox_id {
            serializer.append_pair("sandboxId", sandbox_id);
        }
        if let Some(name) = &self.name {
            serializer.append_pair("name", name);
        }
        for state in &self.states {
            serializer.append_pair("state", snapshot_state_name(state));
        }
        if let Some(page) = self.page {
            serializer.append_pair("page", &page.to_string());
        }
        if let Some(page_size) = self.page_size {
            serializer.append_pair("pageSize", &page_size.to_string());
        }
        let query = serializer.finish();
        if query.is_empty() {
            query
        } else {
            format!("?{query}")
        }
    }
}

/// Client for the OpenSandbox lifecycle API.
#[derive(Clone)]
pub struct LifecycleClient {
    http: Http,
    cache: Option<Arc<EndpointCache>>,
    use_server_proxy: bool,
}

impl LifecycleClient {
    /// Builds a client for the configured control plane.
    pub fn new(config: &ConnectionConfig) -> Result<Self> {
        let base = config.lifecycle_url();
        let http = Http::from_config(config, &base, API_KEY_HEADER)?;
        Ok(Self {
            http,
            cache: cache_from(config),
            use_server_proxy: config.use_server_proxy(),
        })
    }

    /// Creates a sandbox.
    pub async fn create_sandbox(&self, request: &CreateSandboxRequest) -> Result<SandboxInfo> {
        self.http
            .json(Method::POST, "/sandboxes", Some(request))
            .await
    }

    /// Fetches a sandbox by identifier.
    pub async fn get_sandbox(&self, sandbox_id: &str) -> Result<SandboxInfo> {
        self.http
            .json(
                Method::GET,
                &format!("/sandboxes/{}", segment(sandbox_id)),
                None::<&()>,
            )
            .await
    }

    /// Lists sandboxes with optional filters.
    pub async fn list_sandboxes(
        &self,
        query: &ListSandboxesQuery,
    ) -> Result<ListSandboxesResponse> {
        self.http
            .json(
                Method::GET,
                &format!("/sandboxes{}", query.to_query()),
                None::<&()>,
            )
            .await
    }

    /// Requests deletion. Returns once deletion is accepted; the runtime may
    /// still be terminating.
    pub async fn delete_sandbox(&self, sandbox_id: &str) -> Result<()> {
        self.http
            .empty(
                Method::DELETE,
                &format!("/sandboxes/{}", segment(sandbox_id)),
                None::<&()>,
            )
            .await
    }

    /// Pauses a sandbox (state is preserved).
    pub async fn pause_sandbox(&self, sandbox_id: &str) -> Result<()> {
        self.http
            .empty(
                Method::POST,
                &format!("/sandboxes/{}/pause", segment(sandbox_id)),
                None::<&()>,
            )
            .await
    }

    /// Resumes a paused sandbox.
    pub async fn resume_sandbox(&self, sandbox_id: &str) -> Result<()> {
        self.http
            .empty(
                Method::POST,
                &format!("/sandboxes/{}/resume", segment(sandbox_id)),
                None::<&()>,
            )
            .await
    }

    /// Moves the absolute expiration (may shorten or extend).
    pub async fn renew_expiration(
        &self,
        sandbox_id: &str,
        expires_at: DateTime<Utc>,
    ) -> Result<RenewExpirationResponse> {
        let body = RenewExpirationRequest { expires_at };
        self.http
            .json(
                Method::POST,
                &format!("/sandboxes/{}/renew-expiration", segment(sandbox_id)),
                Some(&body),
            )
            .await
    }

    /// Applies a JSON merge patch to sandbox metadata. A `None` value deletes
    /// the key.
    pub async fn patch_metadata(
        &self,
        sandbox_id: &str,
        patch: &BTreeMap<String, Option<String>>,
    ) -> Result<SandboxInfo> {
        self.http
            .json(
                Method::PATCH,
                &format!("/sandboxes/{}/metadata", segment(sandbox_id)),
                Some(patch),
            )
            .await
    }

    /// Resolves (and caches) the endpoint for a sandbox port.
    ///
    /// `use_server_proxy` becomes the `use_server_proxy` query parameter. When
    /// it is `None` the server applies its own default and the result is not
    /// cached, because the cache key cannot be determined.
    pub async fn endpoint(
        &self,
        sandbox_id: &str,
        port: u16,
        use_server_proxy: Option<bool>,
    ) -> Result<Endpoint> {
        let proxy = use_server_proxy.unwrap_or(self.use_server_proxy);
        if let Some(cache) = &self.cache {
            if use_server_proxy.is_some() {
                if let Some(hit) = cache.get(sandbox_id, port, proxy) {
                    return Ok(hit);
                }
            }
        }

        let mut path = format!("/sandboxes/{}/endpoints/{}", segment(sandbox_id), port);
        if let Some(proxy) = use_server_proxy {
            path.push_str(&format!("?use_server_proxy={proxy}"));
        }

        let (mut endpoint, headers): (Endpoint, HeaderMap) = self
            .http
            .json_with_headers(Method::GET, &path, None::<&()>)
            .await?;
        endpoint.origin = headers
            .get(SANDBOX_ORIGIN_HEADER)
            .and_then(|value| value.to_str().ok())
            .filter(|value| !value.is_empty())
            .map(str::to_string);

        if let Some(cache) = &self.cache {
            if use_server_proxy.is_some() {
                cache.put(sandbox_id, port, proxy, endpoint.clone());
            }
        }
        Ok(endpoint)
    }

    /// Resolves a signed endpoint (OSEP-0011) that expires at `expires`
    /// (Unix seconds).
    pub async fn signed_endpoint(
        &self,
        sandbox_id: &str,
        port: u16,
        expires: i64,
    ) -> Result<Endpoint> {
        let path = format!(
            "/sandboxes/{}/endpoints/{}?expires={}",
            segment(sandbox_id),
            port,
            expires
        );
        let (mut endpoint, headers): (Endpoint, HeaderMap) = self
            .http
            .json_with_headers(Method::GET, &path, None::<&()>)
            .await?;
        endpoint.origin = headers
            .get(SANDBOX_ORIGIN_HEADER)
            .and_then(|value| value.to_str().ok())
            .map(str::to_string);
        Ok(endpoint)
    }

    /// Drops cached endpoints for a sandbox (call after pause/resume/delete).
    pub fn invalidate_endpoints(&self, sandbox_id: &str) {
        if let Some(cache) = &self.cache {
            cache.invalidate_sandbox(sandbox_id);
        }
    }

    /// Reads the effective egress policy.
    pub async fn get_network_policy(&self, sandbox_id: &str) -> Result<PolicyStatusResponse> {
        self.http
            .json(
                Method::GET,
                &format!("/sandboxes/{}/networkpolicy", segment(sandbox_id)),
                None::<&()>,
            )
            .await
    }

    /// Merges egress rules into the policy.
    pub async fn patch_network_policy(
        &self,
        sandbox_id: &str,
        rules: &[NetworkRule],
    ) -> Result<PolicyStatusResponse> {
        self.http
            .json(
                Method::PATCH,
                &format!("/sandboxes/{}/networkpolicy", segment(sandbox_id)),
                Some(&rules),
            )
            .await
    }

    /// Removes egress rules by target.
    pub async fn delete_network_policy(
        &self,
        sandbox_id: &str,
        targets: &[String],
    ) -> Result<PolicyStatusResponse> {
        self.http
            .json(
                Method::DELETE,
                &format!("/sandboxes/{}/networkpolicy", segment(sandbox_id)),
                Some(&targets),
            )
            .await
    }

    /// Captures a snapshot of a sandbox. The operation is asynchronous; poll
    /// [`LifecycleClient::get_snapshot`] until it is `Ready`.
    pub async fn create_snapshot(
        &self,
        sandbox_id: &str,
        request: &CreateSnapshotRequest,
    ) -> Result<SnapshotInfo> {
        self.http
            .json(
                Method::POST,
                &format!("/sandboxes/{}/snapshots", segment(sandbox_id)),
                Some(request),
            )
            .await
    }

    /// Lists snapshots.
    pub async fn list_snapshots(
        &self,
        query: &ListSnapshotsQuery,
    ) -> Result<ListSnapshotsResponse> {
        self.http
            .json(
                Method::GET,
                &format!("/snapshots{}", query.to_query()),
                None::<&()>,
            )
            .await
    }

    /// Fetches a snapshot.
    pub async fn get_snapshot(&self, snapshot_id: &str) -> Result<SnapshotInfo> {
        self.http
            .json(
                Method::GET,
                &format!("/snapshots/{}", segment(snapshot_id)),
                None::<&()>,
            )
            .await
    }

    /// Deletes a snapshot.
    pub async fn delete_snapshot(&self, snapshot_id: &str) -> Result<()> {
        self.http
            .empty(
                Method::DELETE,
                &format!("/snapshots/{}", segment(snapshot_id)),
                None::<&()>,
            )
            .await
    }
}

fn cache_from(config: &ConnectionConfig) -> Option<Arc<EndpointCache>> {
    if config.endpoint_cache_disabled() {
        None
    } else {
        Some(Arc::new(EndpointCache::new(
            config.endpoint_cache_ttl(),
            config.endpoint_cache_size(),
        )))
    }
}

fn state_name(state: &SandboxState) -> &'static str {
    match state {
        SandboxState::Pending => "Pending",
        SandboxState::Running => "Running",
        SandboxState::Pausing => "Pausing",
        SandboxState::Paused => "Paused",
        SandboxState::Resuming => "Resuming",
        SandboxState::Stopping => "Stopping",
        SandboxState::Terminated => "Terminated",
        SandboxState::Failed => "Failed",
        SandboxState::Unknown => "Unknown",
    }
}

fn snapshot_state_name(state: &SnapshotState) -> &'static str {
    match state {
        SnapshotState::Creating => "Creating",
        SnapshotState::Deleting => "Deleting",
        SnapshotState::Ready => "Ready",
        SnapshotState::Failed => "Failed",
        SnapshotState::Unknown => "Unknown",
    }
}

fn segment(value: &str) -> String {
    let mut out = String::with_capacity(value.len());
    for byte in value.as_bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b'~') {
            out.push(*byte as char);
        } else {
            out.push_str(&format!("%{byte:02X}"));
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn encodes_path_segments() {
        assert_eq!(segment("abc-123"), "abc-123");
        assert_eq!(segment("a/b"), "a%2Fb");
        assert_eq!(segment("ü"), "%C3%BC");
    }

    #[test]
    fn builds_sandbox_query() {
        let query = ListSandboxesQuery {
            states: vec![SandboxState::Running, SandboxState::Paused],
            metadata: Some(BTreeMap::from([("team".to_string(), "infra".to_string())])),
            page: Some(2),
            page_size: Some(50),
        };
        let encoded = query.to_query();
        assert!(encoded.starts_with('?'));
        assert!(encoded.contains("state=Running"));
        assert!(encoded.contains("state=Paused"));
        assert!(encoded.contains("page=2"));
        assert!(encoded.contains("pageSize=50"));
        assert!(encoded.contains("metadata="));
    }

    #[test]
    fn empty_query_has_no_prefix() {
        assert_eq!(ListSandboxesQuery::default().to_query(), "");
    }

    #[test]
    fn snapshot_query_includes_filters() {
        let query = ListSnapshotsQuery {
            sandbox_id: Some("sbx".into()),
            states: vec![SnapshotState::Ready],
            ..Default::default()
        };
        let encoded = query.to_query();
        assert!(encoded.contains("sandboxId=sbx"));
        assert!(encoded.contains("state=Ready"));
    }
}
