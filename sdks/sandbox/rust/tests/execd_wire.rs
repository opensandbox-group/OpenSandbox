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

//! execd data-plane wire tests: command streaming, files, and metrics.

mod common;

use std::collections::BTreeMap;

use common::{MockServer, Reply};
use futures_util::StreamExt;
use opensandbox::{
    ConnectionConfig, Error, ExecdClient, ExecutionEvent, FileMetadata, RunCommandRequest,
    UploadEntry,
};

fn config() -> ConnectionConfig {
    ConnectionConfig::new().with_protocol("http")
}

fn execd_headers() -> BTreeMap<String, String> {
    BTreeMap::from([(
        "X-EXECD-ACCESS-TOKEN".to_string(),
        "execd-secret".to_string(),
    )])
}

fn execd(server: &MockServer) -> ExecdClient {
    ExecdClient::new(&server.base_url(), &execd_headers(), &config()).unwrap()
}

#[tokio::test]
async fn streams_command_events_across_chunk_boundaries() {
    // The second chunk splits a JSON line in half, which must be buffered
    // until the newline arrives.
    let (server, mut requests) = MockServer::start(vec![Reply::sse([
        "{\"type\":\"init\",\"text\":\"exec-7\",\"timestamp\":1}\n\n",
        "{\"type\":\"stdout\",\"text\":\"hel",
        "lo\",\"timestamp\":2}\n\n",
        "{\"type\":\"stdout\",\"text\":\"world\",\"timestamp\":3}\n\n",
        "{\"type\":\"error\",\"error\":{\"ename\":\"CommandError\",\"evalue\":\"boom\",\
         \"traceback\":[\"line 1\"],\"exit_code\":3},\"timestamp\":4}\n\n",
        "{\"type\":\"execution_complete\",\"execution_time\":9,\"exit_code\":3,\"timestamp\":5}\n\n",
    ])])
    .await;

    let execution = execd(&server)
        .run_command_collect(&RunCommandRequest::shell("fail now"))
        .await
        .unwrap();

    assert_eq!(execution.id, "exec-7");
    assert_eq!(execution.text(), "hello\nworld");
    assert_eq!(execution.exit_code, Some(3));
    assert!(!execution.success());
    let error = execution.error.expect("error event");
    assert_eq!(error.name, "CommandError");
    assert_eq!(error.value, "boom");
    assert_eq!(error.traceback, vec!["line 1"]);
    assert_eq!(execution.complete.unwrap().execution_time, 9);

    let recorded = requests.recv().await.unwrap();
    assert_eq!(recorded.method, "POST");
    assert_eq!(recorded.path, "/command");
    assert_eq!(
        recorded.header("x-execd-access-token"),
        Some("execd-secret")
    );
    assert_eq!(recorded.header("accept"), Some("text/event-stream"));
    assert_eq!(recorded.header("content-type"), Some("application/json"));
    assert_eq!(
        recorded.json(),
        serde_json::json!({ "command": "fail now" })
    );
}

#[tokio::test]
async fn typed_stream_surfaces_events_in_order() {
    let (server, _requests) = MockServer::start(vec![Reply::sse([
        "{\"type\":\"init\",\"text\":\"exec-1\"}\n\n",
        "{\"type\":\"stdout\",\"text\":\"ready\"}\n\n",
        "{\"type\":\"execution_complete\",\"exit_code\":0}\n\n",
    ])])
    .await;

    let mut stream = execd(&server)
        .run_command(&RunCommandRequest::argv(["echo", "ready"]))
        .await
        .unwrap();

    assert!(matches!(
        stream.next_event().await.unwrap().unwrap(),
        ExecutionEvent::Init(_)
    ));
    match stream.next_event().await.unwrap().unwrap() {
        ExecutionEvent::Stdout(message) => assert_eq!(message.text, "ready"),
        other => panic!("expected stdout, got {other:?}"),
    }
    assert!(matches!(
        stream.next_event().await.unwrap().unwrap(),
        ExecutionEvent::Complete(_)
    ));
}

#[tokio::test]
async fn background_command_stops_at_the_startup_ack() {
    // A background launch acknowledges with an execution_complete that has no
    // exit code; anything after it belongs to the log stream.
    let (server, _requests) = MockServer::start(vec![Reply::sse([
        "{\"type\":\"init\",\"text\":\"exec-bg\"}\n\n",
        "{\"type\":\"execution_complete\",\"timestamp\":2}\n\n",
        "{\"type\":\"stdout\",\"text\":\"late output\"}\n\n",
    ])])
    .await;

    let execution = execd(&server)
        .run_command_collect(&RunCommandRequest::shell("sleep 60").in_background())
        .await
        .unwrap();

    assert_eq!(execution.id, "exec-bg");
    assert!(execution.complete.is_some());
    // The acknowledgement carries no exit code; like the other SDKs the SDK
    // infers 0 for an error-free completion.
    assert_eq!(execution.exit_code, Some(0));
    assert!(execution.stdout.is_empty());
}

#[tokio::test]
async fn command_logs_returns_the_tail_cursor() {
    let (server, mut requests) = MockServer::start(vec![
        Reply::text(200, "line1\nline2\n").with_header("EXECD-COMMANDS-TAIL-CURSOR", "42")
    ])
    .await;

    let logs = execd(&server)
        .command_logs("exec-7", Some(5))
        .await
        .unwrap();
    assert_eq!(logs.output, "line1\nline2\n");
    assert_eq!(logs.cursor, 42);

    let recorded = requests.recv().await.unwrap();
    assert_eq!(recorded.path, "/command/exec-7/logs?cursor=5");
}

#[tokio::test]
async fn uploads_files_with_a_metadata_part_before_each_file() {
    let (server, mut requests) = MockServer::start(vec![Reply::empty(200)]).await;
    let entry = UploadEntry::new(
        FileMetadata {
            path: "/tmp/a.txt".into(),
            mode: 644,
            ..Default::default()
        },
        b"hello".to_vec(),
    )
    .with_file_name("a.txt");

    execd(&server).upload_files(&[entry]).await.unwrap();

    let recorded = requests.recv().await.unwrap();
    assert_eq!(recorded.method, "POST");
    assert_eq!(recorded.path, "/files/upload");
    let content_type = recorded.header("content-type").unwrap().to_string();
    assert!(
        content_type.starts_with("multipart/form-data; boundary="),
        "unexpected content type: {content_type}"
    );

    let body = recorded.text();
    assert!(body.contains("name=\"metadata\""));
    assert!(body.contains("filename=\"metadata\""));
    assert!(body.contains("\"path\":\"/tmp/a.txt\""));
    assert!(body.contains("\"mode\":644"));
    assert!(body.contains("name=\"file\""));
    assert!(body.contains("filename=\"a.txt\""));
    assert!(body.contains("hello"));
    assert!(
        body.find("name=\"metadata\"").unwrap() < body.find("name=\"file\"").unwrap(),
        "metadata must precede the file part"
    );
}

#[tokio::test]
async fn download_sends_range_header_or_line_offsets() {
    let (server, mut requests) = MockServer::start(vec![
        Reply::Simple {
            status: 206,
            headers: vec![("content-type".into(), "application/octet-stream".into())],
            body: b"abcd".to_vec(),
        },
        Reply::text(200, "tail\n"),
    ])
    .await;
    let client = execd(&server);

    let bytes = client
        .download_file("/tmp/a.txt", Some("bytes=0-3"), None, None)
        .await
        .unwrap();
    assert_eq!(&bytes[..], b"abcd");
    let recorded = requests.recv().await.unwrap();
    assert_eq!(recorded.path, "/files/download?path=%2Ftmp%2Fa.txt");
    assert_eq!(recorded.header("range"), Some("bytes=0-3"));

    let bytes = client
        .download_file("/tmp/a.txt", None, Some(10), Some(20))
        .await
        .unwrap();
    assert_eq!(&bytes[..], b"tail\n");
    let recorded = requests.recv().await.unwrap();
    assert_eq!(
        recorded.path,
        "/files/download?path=%2Ftmp%2Fa.txt&offset=10&limit=20"
    );
    assert!(recorded.header("range").is_none());
}

#[tokio::test]
async fn download_rejects_mixed_range_styles_before_sending() {
    let (server, mut requests) = MockServer::start(Vec::new()).await;
    let err = execd(&server)
        .download_file("/tmp/a.txt", Some("bytes=0-1"), Some(0), None)
        .await
        .unwrap_err();
    assert!(matches!(err, Error::InvalidArgument { .. }));
    assert!(requests.try_recv().is_err());
}

#[tokio::test]
async fn parses_metrics_with_wire_field_names() {
    let (server, mut requests) = MockServer::start(vec![Reply::ok_json(
        r#"{"cpu_count":4,"cpu_used_pct":12.5,"mem_total_mib":2048,"mem_used_mib":512,"timestamp":7}"#,
    )])
    .await;

    let metrics = execd(&server).metrics().await.unwrap();
    assert_eq!(metrics.cpu_count, 4.0);
    assert_eq!(metrics.cpu_used_pct, 12.5);
    assert_eq!(metrics.mem_total_mib, 2048.0);
    assert_eq!(metrics.mem_used_mib, 512.0);
    assert_eq!(metrics.timestamp, 7);
    assert_eq!(requests.recv().await.unwrap().path, "/metrics");
}

#[tokio::test]
async fn watch_metrics_streams_unpin_samples() {
    let (server, mut requests) = MockServer::start(vec![Reply::sse([
        "data: {\"cpu_count\":2,\"cpu_used_pct\":50,\"timestamp\":1}\n\n",
        "data: {\"cpu_count\":2,\"cpu_used_pct\":75,\"timestamp\":2}\n\n",
    ])])
    .await;

    // Polling without pinning the stream is part of the public contract:
    // this stops compiling if `watch_metrics` loses `Unpin`.
    let mut samples = execd(&server).watch_metrics().await.unwrap();
    let first = samples.next().await.unwrap().unwrap();
    assert_eq!(first.cpu_used_pct, 50.0);
    assert_eq!(first.timestamp, 1);
    let second = samples.next().await.unwrap().unwrap();
    assert_eq!(second.cpu_used_pct, 75.0);
    assert!(samples.next().await.is_none());

    let recorded = requests.recv().await.unwrap();
    assert_eq!(recorded.path, "/metrics/watch");
    assert_eq!(recorded.header("accept"), Some("text/event-stream"));
}

#[tokio::test]
async fn execd_errors_are_mapped_to_api_errors() {
    let (server, _requests) = MockServer::start(vec![Reply::json(
        404,
        r#"{"code":"EXECUTION_NOT_FOUND","message":"unknown execution"}"#,
    )])
    .await;

    let err = execd(&server).command_status("exec-9").await.unwrap_err();
    let api = err.api().expect("api error");
    assert_eq!(api.status, 404);
    assert_eq!(api.code, "EXECUTION_NOT_FOUND");
}
