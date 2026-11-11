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

//! Egress sidecar wire tests: policy, credential vault, and auth headers.

mod common;

use std::collections::BTreeMap;
use std::time::Duration;

use common::{MockServer, Reply};
use opensandbox::{
    ConnectionConfig, CreateOptions, Credential, CredentialAuth, CredentialBinding,
    CredentialMatch, CredentialVaultCreateRequest, CredentialVaultPatchRequest, EgressClient,
    InlineCredentialSource, NetworkRule, Sandbox,
};

fn config() -> ConnectionConfig {
    ConnectionConfig::new().with_protocol("http")
}

fn sidecar_headers() -> BTreeMap<String, String> {
    BTreeMap::from([(
        "OPENSANDBOX-EGRESS-AUTH".to_string(),
        "egress-secret".to_string(),
    )])
}

fn egress(server: &MockServer) -> EgressClient {
    EgressClient::new(&server.base_url(), None, &sidecar_headers(), &config()).unwrap()
}

const POLICY_BODY: &str = r#"{"status":"applied","mode":"allowlist","policy":{"defaultAction":"deny","egress":[{"action":"allow","target":"api.example.com","ports":[443]}]}}"#;

#[tokio::test]
async fn reads_and_mutates_the_policy() {
    let (server, mut requests) = MockServer::start(vec![
        Reply::ok_json(POLICY_BODY),
        Reply::ok_json(POLICY_BODY),
        Reply::ok_json(POLICY_BODY),
    ])
    .await;
    let client = egress(&server);

    let policy = client.policy().await.unwrap();
    assert_eq!(policy.status.as_deref(), Some("applied"));
    assert_eq!(
        policy.policy.as_ref().unwrap().default_action.as_deref(),
        Some("deny")
    );

    let rules = vec![NetworkRule {
        action: "allow".into(),
        target: Some("pypi.org".into()),
        ports: Some(vec![443]),
    }];
    client.patch_policy(&rules).await.unwrap();
    client
        .delete_policy(&["old.example.com".to_string()])
        .await
        .unwrap();

    let read = requests.recv().await.unwrap();
    assert_eq!(read.method, "GET");
    assert_eq!(read.path, "/policy");
    assert_eq!(
        read.header("opensandbox-egress-auth"),
        Some("egress-secret")
    );

    let patched = requests.recv().await.unwrap();
    assert_eq!(patched.method, "PATCH");
    assert_eq!(patched.path, "/policy");
    assert_eq!(patched.json()[0]["target"], "pypi.org");

    let deleted = requests.recv().await.unwrap();
    assert_eq!(deleted.method, "DELETE");
    assert_eq!(deleted.json(), serde_json::json!(["old.example.com"]));
}

#[tokio::test]
async fn creates_a_credential_vault_with_inline_sources() {
    let (server, mut requests) = MockServer::start(vec![Reply::ok_json(
        r#"{"revision":3,"credentials":[{"name":"github","sourceType":"inline","revision":3}],"bindings":[]}"#,
    )])
    .await;
    let client = egress(&server);

    let request = CredentialVaultCreateRequest {
        credentials: vec![Credential {
            name: "github".into(),
            source: InlineCredentialSource {
                // `source_type` is omitted on purpose: it must default to inline.
                source_type: String::new(),
                value: "ghp_secret".into(),
            },
        }],
        bindings: vec![CredentialBinding {
            name: "github-api".into(),
            r#match: CredentialMatch {
                schemes: Some(vec!["https".into()]),
                hosts: vec!["api.github.com".into()],
                ..Default::default()
            },
            auth: CredentialAuth {
                auth_type: "bearer".into(),
                credential: Some("github".into()),
                name: None,
                headers: None,
                substitutions: None,
            },
        }],
    };
    let state = client.create_credential_vault(&request).await.unwrap();
    assert_eq!(state.revision, 3);
    assert_eq!(state.credentials[0].source_type, "inline");

    let recorded = requests.recv().await.unwrap();
    assert_eq!(recorded.method, "POST");
    assert_eq!(recorded.path, "/credential-vault");
    let body = recorded.json();
    assert_eq!(body["credentials"][0]["source"]["type"], "inline");
    assert_eq!(body["credentials"][0]["source"]["value"], "ghp_secret");
    assert_eq!(body["bindings"][0]["match"]["hosts"][0], "api.github.com");
    assert_eq!(body["bindings"][0]["auth"]["type"], "bearer");
    assert_eq!(body["bindings"][0]["auth"]["credential"], "github");
}

#[tokio::test]
async fn patches_the_vault_with_an_optimistic_guard() {
    let (server, mut requests) = MockServer::start(vec![Reply::ok_json(
        r#"{"revision":4,"credentials":[],"bindings":[]}"#,
    )])
    .await;
    let client = egress(&server);

    let request = CredentialVaultPatchRequest {
        expected_revision: Some(3),
        credentials: None,
        bindings: None,
    };
    client.patch_credential_vault(&request).await.unwrap();

    let recorded = requests.recv().await.unwrap();
    assert_eq!(recorded.method, "PATCH");
    assert_eq!(
        recorded.json(),
        serde_json::json!({ "expectedRevision": 3 })
    );
}

#[tokio::test]
async fn reads_sanitized_vault_entries() {
    let (server, mut requests) = MockServer::start(vec![
        Reply::ok_json(
            r#"{"revision":1,"credentials":[{"name":"token","sourceType":"inline","revision":1}]}"#,
        ),
        Reply::ok_json(
            r#"{"revision":1,"bindings":[{"name":"svc","revision":1,"match":{"hosts":["api.example.com"]},"auth":{"type":"apiKey","name":"X-Key"}}]}"#,
        ),
        Reply::empty(204),
    ])
    .await;
    let client = egress(&server);

    let credentials = client.list_credentials().await.unwrap();
    assert_eq!(credentials.credentials[0].name, "token");

    let bindings = client.list_bindings().await.unwrap();
    let binding = &bindings.bindings[0];
    assert_eq!(binding.name, "svc");
    assert_eq!(
        binding.match_rule.as_ref().unwrap().hosts,
        vec!["api.example.com"]
    );
    assert_eq!(binding.auth.as_ref().unwrap().auth_type, "apiKey");

    client.delete_credential_vault().await.unwrap();

    assert_eq!(
        requests.recv().await.unwrap().path,
        "/credential-vault/credentials"
    );
    assert_eq!(
        requests.recv().await.unwrap().path,
        "/credential-vault/bindings"
    );
    assert_eq!(requests.recv().await.unwrap().path, "/credential-vault");
}

#[tokio::test]
async fn direct_mode_never_forwards_the_tenant_api_key() {
    let (server, mut requests) = MockServer::start(vec![Reply::ok_json(POLICY_BODY)]).await;
    let config = config()
        .with_api_key("tenant-key")
        .with_header("OPEN-SANDBOX-API-KEY", "tenant-key");
    let client = EgressClient::new(&server.base_url(), None, &sidecar_headers(), &config).unwrap();

    client.policy().await.unwrap();
    let recorded = requests.recv().await.unwrap();
    assert!(recorded.header("open-sandbox-api-key").is_none());
    assert_eq!(
        recorded.header("opensandbox-egress-auth"),
        Some("egress-secret")
    );
}

#[tokio::test]
async fn sandbox_resolves_the_sidecar_on_the_egress_port() {
    let (server, mut requests) = MockServer::start_with(|server| {
        vec![
            Reply::ok_json(r#"{"id":"sbx-e","status":{"state":"Running"}}"#),
            Reply::ok_json(r#"{"id":"sbx-e","status":{"state":"Running"}}"#),
            Reply::ok_json(format!(
                r#"{{"endpoint":"{server}","headers":{{"X-EXECD-ACCESS-TOKEN":"tok"}}}}"#
            )),
            Reply::empty(200),
            Reply::ok_json(format!(
                r#"{{"endpoint":"{server}","headers":{{"OPENSANDBOX-EGRESS-AUTH":"egress-token"}}}}"#
            )),
            Reply::ok_json(POLICY_BODY),
        ]
    })
    .await;

    let config = ConnectionConfig::new()
        .with_domain(server.domain())
        .with_protocol("http");
    let sandbox = Sandbox::create(
        config,
        CreateOptions::from_image("python:3.12").with_poll_interval(Duration::from_millis(5)),
    )
    .await
    .unwrap();

    let egress = sandbox.egress().await.unwrap();
    let policy = egress.policy().await.unwrap();
    assert_eq!(policy.mode.as_deref(), Some("allowlist"));

    let mut paths = Vec::new();
    for _ in 0..6 {
        paths.push(requests.recv().await.unwrap().path);
    }
    assert_eq!(
        paths,
        vec![
            "/v1/sandboxes".to_string(),
            "/v1/sandboxes/sbx-e".to_string(),
            "/v1/sandboxes/sbx-e/endpoints/44772?use_server_proxy=false".to_string(),
            "/ping".to_string(),
            "/v1/sandboxes/sbx-e/endpoints/18080?use_server_proxy=false".to_string(),
            "/policy".to_string(),
        ]
    );
}
