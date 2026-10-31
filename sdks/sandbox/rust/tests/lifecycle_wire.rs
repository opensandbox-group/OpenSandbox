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

//! Lifecycle control-plane wire tests.

mod common;

use std::time::Duration;

use common::{MockServer, Reply};
use opensandbox::{
    ConnectionConfig, CreateOptions, HostMount, LifecycleClient, NetworkPolicy, RetryConfig, Volume,
};

fn config(server: &MockServer) -> ConnectionConfig {
    ConnectionConfig::new()
        .with_domain(server.domain())
        .with_protocol("http")
        .with_api_key("test-key")
}

const ENDPOINT_BODY: &str =
    r#"{"endpoint":"127.0.0.1:44772","headers":{"X-EXECD-ACCESS-TOKEN":"execd-token"}}"#;

#[tokio::test]
async fn create_sandbox_applies_sdk_defaults_and_api_key() {
    let (server, mut requests) = MockServer::start(vec![Reply::ok_json(
        r#"{"id":"sbx-1","status":{"state":"Pending"},"entrypoint":["tail","-f","/dev/null"]}"#,
    )])
    .await;

    let client = LifecycleClient::new(&config(&server)).unwrap();
    let request = CreateOptions::from_image("python:3.12")
        .to_request()
        .unwrap();
    let info = client.create_sandbox(&request).await.unwrap();
    assert_eq!(info.id, "sbx-1");

    let recorded = requests.recv().await.unwrap();
    assert_eq!(recorded.method, "POST");
    assert_eq!(recorded.path, "/v1/sandboxes");
    assert_eq!(recorded.header("open-sandbox-api-key"), Some("test-key"));
    assert_eq!(recorded.header("content-type"), Some("application/json"));
    assert!(recorded
        .header("user-agent")
        .unwrap()
        .starts_with("OpenSandbox-Rust-SDK/"));

    let body = recorded.json();
    assert_eq!(body["image"]["uri"], "python:3.12");
    assert_eq!(body["timeout"], 600);
    assert_eq!(
        body["entrypoint"],
        serde_json::json!(["tail", "-f", "/dev/null"])
    );
    assert_eq!(body["resourceLimits"]["cpu"], "1");
    assert_eq!(body["resourceLimits"]["memory"], "2Gi");
    assert!(body.get("snapshotId").is_none());
    assert!(body.get("templateId").is_none());
    assert!(body.get("secureAccess").is_none());
}

/// Regression: camelCase wire names must survive the round trip for mounts,
/// policy, and status timestamps.
#[tokio::test]
async fn camel_case_wire_names_survive_mounts_and_status() {
    let (server, mut requests) = MockServer::start(vec![Reply::ok_json(
        r#"{"id":"sbx-2","status":{"state":"Running","lastTransitionAt":"2026-01-02T03:04:05Z"},"entrypoint":[],"createdAt":"2026-01-02T03:04:05Z"}"#,
    )])
    .await;

    let client = LifecycleClient::new(&config(&server)).unwrap();
    let request = CreateOptions::from_image("python:3.12")
        .with_network_policy(NetworkPolicy {
            default_action: Some("deny".into()),
            egress: None,
        })
        .with_volume(Volume {
            name: "data".into(),
            host: Some(HostMount {
                path: "/srv/data".into(),
            }),
            pvc: None,
            ossfs: None,
            mount_path: "/data".into(),
            read_only: true,
            sub_path: Some("nested".into()),
        })
        .to_request()
        .unwrap();

    let info = client.create_sandbox(&request).await.unwrap();
    assert!(
        info.status.last_transition_at.is_some(),
        "lastTransitionAt must be decoded"
    );

    let body = requests.recv().await.unwrap().json();
    assert_eq!(body["networkPolicy"]["defaultAction"], "deny");
    assert_eq!(body["volumes"][0]["mountPath"], "/data");
    assert_eq!(body["volumes"][0]["readOnly"], true);
    assert_eq!(body["volumes"][0]["subPath"], "nested");
    assert!(body["volumes"][0].get("read_only").is_none());
    assert!(body["volumes"][0].get("sub_path").is_none());
    assert!(body.get("network_policy").is_none());
}

#[tokio::test]
async fn resolves_endpoint_with_origin_header_and_caches_it() {
    let (server, mut requests) = MockServer::start_with(|_| {
        vec![
            Reply::ok_json(ENDPOINT_BODY).with_header("Open-Sandbox-Origin", "template"),
            Reply::ok_json(ENDPOINT_BODY),
        ]
    })
    .await;

    let client = LifecycleClient::new(&config(&server)).unwrap();
    let first = client.endpoint("sbx-1", 44772, Some(false)).await.unwrap();
    assert_eq!(first.endpoint, "127.0.0.1:44772");
    assert_eq!(first.headers["X-EXECD-ACCESS-TOKEN"], "execd-token");
    assert_eq!(first.origin.as_deref(), Some("template"));

    let second = client.endpoint("sbx-1", 44772, Some(false)).await.unwrap();
    assert_eq!(second.endpoint, first.endpoint);

    let recorded = requests.recv().await.unwrap();
    assert_eq!(recorded.method, "GET");
    assert_eq!(
        recorded.path,
        "/v1/sandboxes/sbx-1/endpoints/44772?use_server_proxy=false"
    );
    assert!(
        requests.try_recv().is_err(),
        "a cache hit must not reach the server"
    );

    // Pause/resume invalidates the cached endpoint.
    client.invalidate_endpoints("sbx-1");
    client.endpoint("sbx-1", 44772, Some(false)).await.unwrap();
    let refetched = requests.recv().await.unwrap();
    assert_eq!(refetched.path, recorded.path);
}

#[tokio::test]
async fn endpoint_cache_expires_after_ttl() {
    let (server, mut requests) = MockServer::start(vec![
        Reply::ok_json(ENDPOINT_BODY),
        Reply::ok_json(ENDPOINT_BODY),
    ])
    .await;
    let config = config(&server).with_endpoint_cache_ttl(Duration::from_millis(40));
    let client = LifecycleClient::new(&config).unwrap();

    client.endpoint("sbx-1", 44772, Some(false)).await.unwrap();
    requests.recv().await.unwrap();

    client.endpoint("sbx-1", 44772, Some(false)).await.unwrap();
    assert!(requests.try_recv().is_err());

    tokio::time::sleep(Duration::from_millis(80)).await;
    client.endpoint("sbx-1", 44772, Some(false)).await.unwrap();
    let recorded = requests.recv().await.unwrap();
    assert_eq!(
        recorded.path,
        "/v1/sandboxes/sbx-1/endpoints/44772?use_server_proxy=false"
    );
}

#[tokio::test]
async fn unsigned_endpoint_omits_the_proxy_flag() {
    let (server, mut requests) = MockServer::start(vec![Reply::ok_json(ENDPOINT_BODY)]).await;
    let client = LifecycleClient::new(&config(&server)).unwrap();

    let endpoint = client.endpoint("sbx-1", 8080, None).await.unwrap();
    assert_eq!(endpoint.endpoint, "127.0.0.1:44772");
    assert!(endpoint.origin.is_none());

    let recorded = requests.recv().await.unwrap();
    assert_eq!(recorded.path, "/v1/sandboxes/sbx-1/endpoints/8080");
}

#[tokio::test]
async fn maps_structured_errors_with_request_id() {
    let (server, _requests) = MockServer::start(vec![Reply::json(
        404,
        r#"{"code":"SANDBOX_NOT_FOUND","message":"no such sandbox"}"#,
    )
    .with_header("X-Request-Id", "req-42")])
    .await;
    let client = LifecycleClient::new(&config(&server)).unwrap();

    let err = client.get_sandbox("missing").await.unwrap_err();
    let api = err.api().expect("api error");
    assert_eq!(api.status, 404);
    assert!(api.is_not_found());
    assert_eq!(api.code, "SANDBOX_NOT_FOUND");
    assert_eq!(api.message, "no such sandbox");
    assert_eq!(api.request_id.as_deref(), Some("req-42"));
    assert_eq!(api.retry_after, None);
    assert!(!api.wake_expired);
}

#[tokio::test]
async fn falls_back_to_raw_body_for_non_json_errors() {
    let (server, _requests) = MockServer::start(vec![Reply::text(502, "upstream exploded")]).await;
    let client =
        LifecycleClient::new(&config(&server).with_retry(RetryConfig::disabled())).unwrap();

    let err = client.get_sandbox("sbx-1").await.unwrap_err();
    let api = err.api().expect("api error");
    assert_eq!(api.status, 502);
    assert_eq!(api.code, "Bad Gateway");
    assert_eq!(api.message, "upstream exploded");
}

#[tokio::test]
async fn captures_retry_after_and_wake_marker() {
    let (server, _requests) = MockServer::start(vec![Reply::json(
        503,
        r#"{"code":"WAKE_PENDING","message":"waking up"}"#,
    )
    .with_header("OpenSandbox-Wake", "expired")
    .with_header("Retry-After", "1")])
    .await;
    let client =
        LifecycleClient::new(&config(&server).with_retry(RetryConfig::disabled())).unwrap();

    let err = client.get_sandbox("sbx-1").await.unwrap_err();
    let api = err.api().expect("api error");
    assert!(api.wake_expired);
    assert_eq!(api.retry_after, Some(Duration::from_secs(1)));
}

#[tokio::test]
async fn pause_resume_and_renew_use_the_documented_paths() {
    let (server, mut requests) = MockServer::start(vec![
        Reply::empty(202),
        Reply::empty(202),
        Reply::ok_json(r#"{"expiresAt":"2026-10-29T12:00:00Z"}"#),
        Reply::ok_json(
            r#"{"id":"sbx-1","status":{"state":"Running"},"metadata":{"team":"infra"}}"#,
        ),
        Reply::empty(204),
    ])
    .await;
    let client = LifecycleClient::new(&config(&server)).unwrap();

    client.pause_sandbox("sbx-1").await.unwrap();
    client.resume_sandbox("sbx-1").await.unwrap();
    client
        .renew_expiration("sbx-1", "2026-10-29T12:00:00Z".parse().unwrap())
        .await
        .unwrap();
    let mut patch = std::collections::BTreeMap::new();
    patch.insert("owner".to_string(), Some("alice".to_string()));
    let patched = client.patch_metadata("sbx-1", &patch).await.unwrap();
    assert_eq!(patched.metadata.unwrap()["team"], "infra");
    client.delete_sandbox("sbx-1").await.unwrap();

    let paths: Vec<String> = (0..5)
        .map(|_| requests.try_recv().expect("request").path)
        .collect();
    assert_eq!(
        paths,
        vec![
            "/v1/sandboxes/sbx-1/pause",
            "/v1/sandboxes/sbx-1/resume",
            "/v1/sandboxes/sbx-1/renew-expiration",
            "/v1/sandboxes/sbx-1/metadata",
            "/v1/sandboxes/sbx-1",
        ]
    );
}
