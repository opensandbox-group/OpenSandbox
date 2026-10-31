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

//! End-to-end sandbox facade tests against a scripted control plane.

mod common;

use std::time::Duration;

use common::{drain, MockServer, Reply};
use opensandbox::{ConnectionConfig, CreateOptions, Error, ReadyOptions, Sandbox, SandboxOrigin};

fn config(server: &MockServer) -> ConnectionConfig {
    ConnectionConfig::new()
        .with_domain(server.domain())
        .with_protocol("http")
        .with_api_key("test-key")
}

fn command_reply() -> Reply {
    Reply::sse([
        "{\"type\":\"init\",\"text\":\"exec-1\"}\n\n",
        "{\"type\":\"stdout\",\"text\":\"hi\"}\n\n",
        "{\"type\":\"execution_complete\",\"exit_code\":0}\n\n",
    ])
}

#[tokio::test]
async fn create_waits_for_running_then_binds_execd() {
    let (server, mut requests) = MockServer::start_with(|server| {
        vec![
            Reply::ok_json(r#"{"id":"sbx-1","status":{"state":"Pending"}}"#),
            Reply::ok_json(r#"{"id":"sbx-1","status":{"state":"Running"}}"#),
            endpoint_reply_for(server),
            Reply::empty(200),
            command_reply(),
            Reply::empty(204),
        ]
    })
    .await;

    let sandbox = Sandbox::create(
        config(&server),
        CreateOptions::from_image("python:3.12")
            .with_env("MODE", "test")
            .with_poll_interval(Duration::from_millis(5)),
    )
    .await
    .unwrap();

    assert_eq!(sandbox.id(), "sbx-1");
    assert_eq!(sandbox.origin(), &SandboxOrigin::Template);
    assert!(sandbox.is_template_backed());

    let execution = sandbox.exec("echo hi").await.unwrap();
    assert_eq!(execution.text(), "hi");
    assert_eq!(execution.exit_code, Some(0));
    assert!(execution.success());

    sandbox.kill().await.unwrap();

    let mut paths = Vec::new();
    for _ in 0..6 {
        let request = requests.recv().await.expect("recorded request");
        paths.push((request.method, request.path));
    }
    assert_eq!(
        paths,
        vec![
            ("POST".into(), "/v1/sandboxes".into()),
            ("GET".into(), "/v1/sandboxes/sbx-1".into()),
            (
                "GET".into(),
                "/v1/sandboxes/sbx-1/endpoints/44772?use_server_proxy=false".into()
            ),
            ("GET".into(), "/ping".into()),
            ("POST".into(), "/command".into()),
            ("DELETE".into(), "/v1/sandboxes/sbx-1".into()),
        ]
    );
}

// The closure form of `start_with` yields `&str`, so build the endpoint reply
// from the domain directly.
fn endpoint_reply_for(server: &str) -> Reply {
    Reply::ok_json(format!(
        r#"{{"endpoint":"{server}","headers":{{"X-EXECD-ACCESS-TOKEN":"execd-token"}}}}"#
    ))
    .with_header("Open-Sandbox-Origin", "template")
}

#[tokio::test]
async fn create_propagates_the_exec_token_to_execd() {
    let (server, mut requests) = MockServer::start_with(|server| {
        vec![
            Reply::ok_json(r#"{"id":"sbx-2","status":{"state":"Running"}}"#),
            Reply::ok_json(r#"{"id":"sbx-2","status":{"state":"Running"}}"#),
            endpoint_reply_for(server),
            Reply::empty(200),
            Reply::empty(200),
        ]
    })
    .await;

    let sandbox = Sandbox::create(
        config(&server),
        CreateOptions::from_image("python:3.12").with_poll_interval(Duration::from_millis(5)),
    )
    .await
    .unwrap();
    sandbox.ping().await.unwrap();

    let ping = {
        let mut last = None;
        for _ in 0..5 {
            last = Some(requests.recv().await.expect("request"));
        }
        last.unwrap()
    };
    assert_eq!(ping.path, "/ping");
    assert_eq!(ping.header("x-execd-access-token"), Some("execd-token"));
    assert!(
        ping.header("open-sandbox-api-key").is_none(),
        "direct mode must not leak the tenant API key into the sandbox"
    );
}

#[tokio::test]
async fn create_deletes_the_sandbox_when_exec_never_answers() {
    let (server, mut requests) = MockServer::start_with(|server| {
        let mut replies = vec![
            Reply::ok_json(r#"{"id":"sbx-3","status":{"state":"Running"}}"#),
            Reply::ok_json(r#"{"id":"sbx-3","status":{"state":"Running"}}"#),
            Reply::ok_json(format!(
                r#"{{"endpoint":"{server}","headers":{{"X-EXECD-ACCESS-TOKEN":"tok"}}}}"#
            )),
        ];
        replies.extend(
            (0..100).map(|_| Reply::json(500, r#"{"code":"NOT_READY","message":"starting"}"#)),
        );
        replies
    })
    .await;

    let err = Sandbox::create(
        config(&server),
        CreateOptions::from_image("python:3.12")
            .with_poll_interval(Duration::from_millis(10))
            .with_ready_timeout(Duration::from_millis(120)),
    )
    .await
    .unwrap_err();
    assert!(
        matches!(err, Error::ReadyTimeout { .. }),
        "unexpected error: {err}"
    );

    let requests = drain(&mut requests);
    assert!(
        requests
            .iter()
            .any(|request| request.method == "DELETE" && request.path == "/v1/sandboxes/sbx-3"),
        "a failed create must clean up the sandbox: {requests:#?}"
    );
}

#[tokio::test]
async fn create_reports_terminal_states() {
    let (server, _requests) = MockServer::start(vec![
        Reply::ok_json(r#"{"id":"sbx-4","status":{"state":"Failed","reason":"ImagePullBackOff"}}"#),
        Reply::ok_json(
            r#"{"id":"sbx-4","status":{"state":"Failed","reason":"ImagePullBackOff","message":"pull failed"}}"#,
        ),
        Reply::empty(204),
    ])
    .await;

    let err = Sandbox::create(
        config(&server),
        CreateOptions::from_image("missing:latest").with_poll_interval(Duration::from_millis(5)),
    )
    .await
    .unwrap_err();

    match err {
        Error::TerminalState {
            sandbox_id, state, ..
        } => {
            assert_eq!(sandbox_id, "sbx-4");
            assert_eq!(state, "Failed");
        }
        other => panic!("unexpected error: {other}"),
    }
}

#[tokio::test]
async fn connect_retries_while_the_pod_has_no_ip() {
    let (server, mut requests) = MockServer::start_with(|server| {
        vec![
            Reply::json(
                404,
                r#"{"code":"KUBERNETES::POD_IP_NOT_AVAILABLE","message":"waiting for pod ip"}"#,
            ),
            Reply::ok_json(format!(
                r#"{{"endpoint":"{server}","headers":{{"X-EXECD-ACCESS-TOKEN":"tok"}}}}"#
            )),
            Reply::empty(200),
        ]
    })
    .await;

    let sandbox = Sandbox::connect_with(
        config(&server),
        "sbx-5",
        ReadyOptions {
            timeout: Duration::from_secs(5),
            polling_interval: Duration::from_millis(5),
            skip_health_check: false,
        },
    )
    .await
    .unwrap();

    assert_eq!(sandbox.id(), "sbx-5");
    let first = requests.recv().await.unwrap();
    let second = requests.recv().await.unwrap();
    assert_eq!(first.path, second.path);
    assert_eq!(requests.recv().await.unwrap().path, "/ping");
}

#[tokio::test]
async fn connect_does_not_retry_unrelated_404s() {
    let (server, mut requests) = MockServer::start(vec![Reply::json(
        404,
        r#"{"code":"SANDBOX_NOT_FOUND","message":"gone"}"#,
    )])
    .await;

    let err = Sandbox::connect(config(&server), "sbx-6")
        .await
        .unwrap_err();
    assert_eq!(err.api().unwrap().code, "SANDBOX_NOT_FOUND");
    assert!(requests.try_recv().is_ok());
    assert!(requests.try_recv().is_err(), "only one attempt expected");
}

#[tokio::test]
async fn connect_rejects_an_empty_sandbox_id() {
    let (server, _requests) = MockServer::start(Vec::new()).await;
    let err = Sandbox::connect(config(&server), "").await.unwrap_err();
    assert!(matches!(err, Error::InvalidArgument { .. }));
}

/// The sharing contract other SDKs also rely on: handles move into spawned
/// tasks and can be cloned per task, while streams stay cheap to poll.
#[test]
fn handles_are_send_sync_and_cloneable() {
    use opensandbox::{EgressClient, ExecdClient, ExecutionStream, LifecycleClient};

    fn assert_handle<T: Send + Sync + Clone>() {}
    fn assert_stream<T: Send + Unpin>() {}

    assert_handle::<Sandbox>();
    assert_handle::<LifecycleClient>();
    assert_handle::<ExecdClient>();
    assert_handle::<EgressClient>();
    assert_stream::<ExecutionStream>();
}
