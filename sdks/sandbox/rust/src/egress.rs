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

//! Data-plane client for the egress sidecar (port 18080).
//!
//! The sidecar enforces the sandbox egress policy and hosts the sandbox-local
//! credential vault. Requests are authenticated with
//! `OPENSANDBOX-EGRESS-AUTH`; the sidecar token arrives in the endpoint headers
//! returned by the lifecycle API.

use std::collections::BTreeMap;

use reqwest::Method;

use crate::config::{ConnectionConfig, EGRESS_AUTH_HEADER};
use crate::error::Result;
use crate::models::{
    CredentialBindingListResponse, CredentialBindingMetadata, CredentialListResponse,
    CredentialMetadata, CredentialVaultCreateRequest, CredentialVaultPatchRequest,
    CredentialVaultState, NetworkRule, PolicyStatusResponse,
};
use crate::transport::Http;

/// Client for the egress sidecar API.
#[derive(Clone)]
pub struct EgressClient {
    http: Http,
}

impl EgressClient {
    /// Builds a client for an endpoint URL and the headers returned by the
    /// lifecycle API.
    ///
    /// `token` is the sidecar access token; it becomes the
    /// `OPENSANDBOX-EGRESS-AUTH` header. Pass `None` when the token travels in
    /// `headers` (the usual case) or when the sidecar is unauthenticated.
    pub fn new(
        endpoint_url: &str,
        token: Option<&str>,
        headers: &BTreeMap<String, String>,
        config: &ConnectionConfig,
    ) -> Result<Self> {
        let http = Http::new(
            endpoint_url,
            EGRESS_AUTH_HEADER,
            token.map(str::to_string),
            &config.data_plane_headers(headers),
            config.retry(),
            config.request_timeout(),
        )?;
        Ok(Self { http })
    }

    // -- policy ------------------------------------------------------------

    /// Returns the enforced policy and sidecar metadata.
    pub async fn policy(&self) -> Result<PolicyStatusResponse> {
        self.http.json(Method::GET, "/policy", None::<&()>).await
    }

    /// Merges rules into the enforced policy. Existing rules stay unless
    /// overridden.
    pub async fn patch_policy(&self, rules: &[NetworkRule]) -> Result<PolicyStatusResponse> {
        self.http.json(Method::PATCH, "/policy", Some(&rules)).await
    }

    /// Removes rules matching the given targets. Unknown targets are ignored,
    /// and the current `defaultAction` is preserved.
    pub async fn delete_policy(&self, targets: &[String]) -> Result<PolicyStatusResponse> {
        self.http
            .json(Method::DELETE, "/policy", Some(&targets))
            .await
    }

    // -- credential vault --------------------------------------------------

    /// Creates the initial vault revision and activates it.
    pub async fn create_credential_vault(
        &self,
        request: &CredentialVaultCreateRequest,
    ) -> Result<CredentialVaultState> {
        self.http
            .json(Method::POST, "/credential-vault", Some(request))
            .await
    }

    /// Returns sanitized vault state. Plaintext values are never returned.
    pub async fn credential_vault(&self) -> Result<CredentialVaultState> {
        self.http
            .json(Method::GET, "/credential-vault", None::<&()>)
            .await
    }

    /// Atomically mutates credentials and bindings.
    pub async fn patch_credential_vault(
        &self,
        request: &CredentialVaultPatchRequest,
    ) -> Result<CredentialVaultState> {
        self.http
            .json(Method::PATCH, "/credential-vault", Some(request))
            .await
    }

    /// Deletes the sandbox-local vault.
    pub async fn delete_credential_vault(&self) -> Result<()> {
        self.http
            .empty(Method::DELETE, "/credential-vault", None::<&()>)
            .await
    }

    /// Lists sanitized credential metadata.
    pub async fn list_credentials(&self) -> Result<CredentialListResponse> {
        self.http
            .json(Method::GET, "/credential-vault/credentials", None::<&()>)
            .await
    }

    /// Returns sanitized metadata for one credential.
    pub async fn credential(&self, name: &str) -> Result<CredentialMetadata> {
        self.http
            .json(
                Method::GET,
                &format!("/credential-vault/credentials/{}", segment(name)),
                None::<&()>,
            )
            .await
    }

    /// Lists sanitized binding metadata.
    pub async fn list_bindings(&self) -> Result<CredentialBindingListResponse> {
        self.http
            .json(Method::GET, "/credential-vault/bindings", None::<&()>)
            .await
    }

    /// Returns sanitized metadata for one binding.
    pub async fn binding(&self, name: &str) -> Result<CredentialBindingMetadata> {
        self.http
            .json(
                Method::GET,
                &format!("/credential-vault/bindings/{}", segment(name)),
                None::<&()>,
            )
            .await
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
    fn encodes_credential_names() {
        assert_eq!(segment("github/token"), "github%2Ftoken");
    }
}
