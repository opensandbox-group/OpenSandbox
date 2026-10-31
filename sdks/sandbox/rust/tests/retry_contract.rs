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

//! Retry and wake-on-access contract tests.

mod common;

use std::collections::BTreeMap;
use std::time::Duration;

use common::{MockServer, Reply};
use opensandbox::{
    ConnectionConfig, CreateSandboxRequest, ExecdClient, LifecycleClient, RetryConfig,
    RunCommandRequest,
};

fn fast_retry(max_retries: u32) -> RetryConfig {
    RetryConfig {
        max_retries,
        initial_backoff: Duration::from_millis(5),
        max_backoff: Duration::from_millis(10),
        jitter: 0.0,
        ..RetryConfig::default()
    }
}

fn config(server: &MockServer, retry: RetryConfig) -> ConnectionConfig {
    ConnectionConfig::new()
        .with_domain(server.domain())
        .with_protocol("http")
        .with_retry(retry)
}

fn execd(server: &MockServer, retry: RetryConfig) -> ExecdClient {
    ExecdClient::new(
        &server.base_url(),
        &BTreeMap::new(),
        &ConnectionConfig::new().with_retry(retry),
    )
    .unwrap()
}

#[tokio::test]
async fn get_retries_transient_failures() {
    let (server, mut requests) = MockServer::start(vec![
        Reply::json(503, r#"{"code":"UNAVAILABLE","message":"warming up"}"#),
        Reply::ok_json(r#"{"id":"sbx-1","status":{"state":"Running"}}"#),
    ])
    .await;
    let client = LifecycleClient::new(&config(&server, fast_retry(3))).unwrap();

    let info = client.get_sandbox("sbx-1").await.unwrap();
    assert_eq!(info.id, "sbx-1");

    let first = requests.recv().await.unwrap();
    let second = requests.recv().await.unwrap();
    assert_eq!(first.path, "/v1/sandboxes/sbx-1");
    assert_eq!(second.path, first.path);
    assert!(requests.try_recv().is_err());
}

#[tokio::test]
async fn get_gives_up_after_max_retries() {
    let (server, mut requests) = MockServer::start(vec![
        Reply::json(503, r#"{"code":"UNAVAILABLE","message":"still down"}"#),
        Reply::json(503, r#"{"code":"UNAVAILABLE","message":"still down"}"#),
        Reply::json(503, r#"{"code":"UNAVAILABLE","message":"still down"}"#),
        Reply::ok_json(r#"{"id":"sbx-1","status":{"state":"Running"}}"#),
    ])
    .await;
    let client = LifecycleClient::new(&config(&server, fast_retry(2))).unwrap();

    let err = client.get_sandbox("sbx-1").await.unwrap_err();
    assert_eq!(err.api().unwrap().status, 503);
    assert!(requests.try_recv().is_ok(), "first attempt");
    assert!(requests.try_recv().is_ok(), "first retry");
    assert!(requests.try_recv().is_ok(), "second retry");
    assert!(requests.try_recv().is_err(), "no attempt past max_retries");
    assert_eq!(server.queued(), 1, "the success reply must stay unused");
}

#[tokio::test]
async fn post_is_not_generically_retried() {
    let (server, mut requests) = MockServer::start(vec![
        Reply::json(503, r#"{"code":"UNAVAILABLE","message":"try later"}"#),
        Reply::ok_json(r#"{"id":"sbx-1","status":{"state":"Running"}}"#),
    ])
    .await;
    let client = LifecycleClient::new(&config(&server, fast_retry(3))).unwrap();

    let err = client
        .create_sandbox(&CreateSandboxRequest::default())
        .await
        .unwrap_err();
    assert_eq!(err.api().unwrap().status, 503);
    assert!(requests.try_recv().is_ok());
    assert!(
        requests.try_recv().is_err(),
        "POST must not be replayed without a wake marker"
    );
}

#[tokio::test]
async fn wake_marker_is_replayed_for_post() {
    let (server, mut requests) = MockServer::start(vec![
        Reply::json(503, r#"{"code":"WAKE_PENDING","message":"waking"}"#)
            .with_header("OpenSandbox-Wake", "expired")
            .with_header("Retry-After", "0"),
        Reply::ok_json(r#"{"id":"sbx-2","status":{"state":"Running"}}"#),
    ])
    .await;
    let client = LifecycleClient::new(&config(&server, fast_retry(0))).unwrap();

    let info = client
        .create_sandbox(&CreateSandboxRequest::default())
        .await
        .unwrap();
    assert_eq!(info.id, "sbx-2");
    assert!(requests.try_recv().is_ok());
    assert!(requests.try_recv().is_ok(), "wake retry must be replayed");
}

#[tokio::test]
async fn streams_honor_the_wake_contract_but_not_generic_retries() {
    // First request: parked by wake-on-access, so it is safe to replay.
    let (server, mut requests) = MockServer::start(vec![
        Reply::json(503, r#"{"code":"WAKE_PENDING","message":"waking"}"#)
            .with_header("OpenSandbox-Wake", "expired")
            .with_header("Retry-After", "0"),
        Reply::sse([
            "{\"type\":\"init\",\"text\":\"exec-1\"}\n\n",
            "{\"type\":\"execution_complete\",\"exit_code\":0}\n\n",
        ]),
    ])
    .await;
    let execution = execd(&server, fast_retry(3))
        .run_command_collect(&RunCommandRequest::shell("echo hi"))
        .await
        .unwrap();
    assert_eq!(execution.id, "exec-1");
    assert_eq!(execution.exit_code, Some(0));
    assert!(requests.try_recv().is_ok());
    assert!(requests.try_recv().is_ok(), "wake retry applies to streams");

    // Second request: a plain 503 is never replayed for a command, because a
    // replay could duplicate the execution.
    let (server, mut requests) = MockServer::start(vec![
        Reply::json(503, r#"{"code":"UNAVAILABLE","message":"restarting"}"#),
        Reply::sse(["{\"type\":\"execution_complete\",\"exit_code\":0}\n\n"]),
    ])
    .await;
    let err = execd(&server, fast_retry(3))
        .run_command(&RunCommandRequest::shell("echo hi"))
        .await
        .unwrap_err();
    assert_eq!(err.api().unwrap().status, 503);
    assert!(requests.try_recv().is_ok());
    assert!(
        requests.try_recv().is_err(),
        "a streamed POST must not be replayed"
    );
}

#[tokio::test]
async fn disabling_retries_makes_the_first_error_final() {
    let (server, mut requests) = MockServer::start(vec![
        Reply::json(503, r#"{"code":"UNAVAILABLE","message":"down"}"#),
        Reply::ok_json(r#"{"id":"sbx-1","status":{"state":"Running"}}"#),
    ])
    .await;
    let client = LifecycleClient::new(&config(&server, RetryConfig::disabled())).unwrap();

    assert!(client.get_sandbox("sbx-1").await.is_err());
    assert!(requests.try_recv().is_ok());
    assert!(requests.try_recv().is_err());
}
