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

//! Live-server execd suite: streaming, sessions, background commands,
//! filesystem round trips, metrics, and code execution.
//!
//! Mirrors `tests/python/tests/test_command_stream_e2e.py` and the Go
//! `command_e2e_test.go` / `filesystem_e2e_test.go` suites; skipped unless
//! `scripts/rust-e2e.sh` exported `OPENSANDBOX_E2E=1`.

#[path = "e2e_common/mod.rs"]
mod e2e_common;

use std::time::{Duration, Instant};

use futures_util::StreamExt;
use opensandbox::{
    CreateSessionRequest, ExecutionEvent, FileMetadata, RunCodeRequest, RunCommandRequest,
    RunInSessionRequest, UploadEntry,
};

use e2e_common::{
    code_interpreter_enabled, create_sandbox, kill_quietly, require_e2e, unique_tag, WAIT_BUDGET,
};

/// Drains a stream into the plain-text accumulator, failing on transport or
/// execution errors.
async fn drain(
    stream: opensandbox::ExecutionStream,
) -> opensandbox::Result<opensandbox::Execution> {
    stream.collect().await
}

#[tokio::test]
async fn streaming_command_reports_lines_and_completion() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-stream")]).await;

    let mut stdout = Vec::new();
    let mut complete = None;
    let mut stream = sandbox
        .run_command(&RunCommandRequest::shell(
            "echo line-one && echo line-two && echo line-three",
        ))
        .await
        .expect("start command stream");
    while let Some(event) = stream.next().await {
        match event.expect("stream event") {
            ExecutionEvent::Stdout(message) => stdout.push(message.text),
            ExecutionEvent::Complete(done) => {
                complete = Some(done);
                break;
            }
            ExecutionEvent::Other { event, .. } => {
                // `status` and any future control events must never be
                // mistaken for program output.
                eprintln!("control event: {event}");
            }
            _ => {}
        }
    }
    drop(stream);

    let joined = stdout.join("\n");
    assert!(joined.contains("line-one"), "stdout: {joined:?}");
    assert!(joined.contains("line-two"), "stdout: {joined:?}");
    assert!(joined.contains("line-three"), "stdout: {joined:?}");
    assert!(
        !joined.contains("status"),
        "control text leaked: {joined:?}"
    );
    let complete = complete.expect("terminal complete event");
    // execd omits `exit_code` when it runs with SSE legacy compatibility, so a
    // successful completion without one still means exit code 0.
    assert_eq!(
        complete.exit_code.unwrap_or_default(),
        0,
        "exit_code: {:?}",
        complete.exit_code
    );

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn failed_command_reports_exit_code_and_stderr() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-failure")]).await;

    let execution = sandbox
        .exec("echo boom >&2; exit 3")
        .await
        .expect("run failing command");
    assert_eq!(execution.exit_code, Some(3), "execution: {execution:?}");
    assert!(!execution.success(), "a nonzero exit must not be a success");
    assert!(
        execution.stderr_text().contains("boom"),
        "stderr: {:?}",
        execution.stderr_text()
    );

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn file_upload_download_round_trip() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-files")]).await;
    let tag = unique_tag("rust-file");
    let path = format!("/tmp/opensandbox-rust-e2e-{tag}.txt");
    let content = format!("rust sdk e2e {tag}\nsecond line\n");

    sandbox
        .execd()
        .upload_file(UploadEntry::new(
            FileMetadata {
                path: path.clone(),
                ..Default::default()
            },
            content.clone().into_bytes(),
        ))
        .await
        .expect("upload file");

    let infos = sandbox
        .execd()
        .file_info(std::slice::from_ref(&path))
        .await
        .expect("file info");
    let info = infos
        .get(&path)
        .unwrap_or_else(|| panic!("no info for {path}"));
    assert_eq!(info.size, content.len() as i64, "size mismatch");

    let downloaded = sandbox
        .execd()
        .download_file(&path, None, None, None)
        .await
        .expect("download file");
    assert_eq!(String::from_utf8_lossy(&downloaded), content);

    // Line-range download: offset selects lines, and the first line must
    // survive byte for byte.
    let first_line = sandbox
        .execd()
        .download_file(&path, None, Some(0), Some(1))
        .await
        .expect("line-range download");
    let first_line = String::from_utf8_lossy(&first_line);
    assert!(
        first_line.starts_with(&format!("rust sdk e2e {tag}")),
        "line-range download returned {first_line:?}"
    );

    sandbox
        .execd()
        .delete_files(std::slice::from_ref(&path))
        .await
        .expect("delete file");

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn bash_session_preserves_state_between_runs() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-session")]).await;

    let session = sandbox
        .execd()
        .create_session(&CreateSessionRequest {
            cwd: Some("/tmp".to_string()),
        })
        .await
        .expect("create session");

    // Working directory set at session creation is reused by the next run.
    let pwd = drain(
        sandbox
            .execd()
            .run_in_session(
                &session.id,
                &RunInSessionRequest {
                    command: "pwd".to_string(),
                    ..Default::default()
                },
            )
            .await
            .expect("run pwd in session"),
    )
    .await
    .expect("collect pwd run");
    assert_eq!(pwd.exit_code, Some(0));
    assert!(
        pwd.text().trim().ends_with("/tmp"),
        "session cwd lost: {:?}",
        pwd.text()
    );

    // Exported variables persist across runs in the same session.
    let export = drain(
        sandbox
            .execd()
            .run_in_session(
                &session.id,
                &RunInSessionRequest {
                    command: "export RUST_E2E_MARK=marker-value".to_string(),
                    ..Default::default()
                },
            )
            .await
            .expect("run export in session"),
    )
    .await
    .expect("collect export run");
    assert_eq!(export.exit_code, Some(0));

    let readback = drain(
        sandbox
            .execd()
            .run_in_session(
                &session.id,
                &RunInSessionRequest {
                    command: "printf '%s' \"$RUST_E2E_MARK\"".to_string(),
                    ..Default::default()
                },
            )
            .await
            .expect("run readback in session"),
    )
    .await
    .expect("collect readback run");
    assert_eq!(
        readback.text(),
        "marker-value",
        "session env did not persist: {:?}",
        readback.text()
    );

    sandbox
        .execd()
        .delete_session(&session.id)
        .await
        .expect("delete session");

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn background_command_reports_status_and_logs() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-background")]).await;

    let mut request =
        RunCommandRequest::shell("printf 'bg-start\\n'; sleep 1; printf 'bg-done\\n'");
    request.background = true;
    let started = sandbox
        .execd()
        .run_command_collect(&request)
        .await
        .expect("start background command");
    assert!(
        !started.id.is_empty(),
        "background command must report its execution id"
    );

    let status = sandbox
        .execd()
        .command_status(&started.id)
        .await
        .expect("command status");
    assert_eq!(status.id, started.id);

    let deadline = Instant::now() + WAIT_BUDGET;
    let mut logs = String::new();
    while Instant::now() < deadline {
        let fetched = sandbox
            .execd()
            .command_logs(&started.id, None)
            .await
            .expect("command logs");
        if fetched.output.contains("bg-done") {
            logs = fetched.output;
            break;
        }
        tokio::time::sleep(Duration::from_millis(300)).await;
    }
    assert!(
        logs.contains("bg-start") && logs.contains("bg-done"),
        "logs never completed: {logs:?}"
    );

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn interrupt_stops_a_background_command() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-interrupt")]).await;

    let mut request = RunCommandRequest::shell("sleep 300");
    request.background = true;
    let started = sandbox
        .execd()
        .run_command_collect(&request)
        .await
        .expect("start sleeper");
    assert!(!started.id.is_empty());

    sandbox
        .execd()
        .interrupt_command(&started.id)
        .await
        .expect("interrupt command");

    let deadline = Instant::now() + WAIT_BUDGET;
    loop {
        let status = sandbox
            .execd()
            .command_status(&started.id)
            .await
            .expect("command status after interrupt");
        if !status.running {
            break;
        }
        assert!(
            Instant::now() < deadline,
            "command kept running after interrupt"
        );
        tokio::time::sleep(Duration::from_millis(500)).await;
    }

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn metrics_report_cpu_and_memory() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-metrics")]).await;

    let metrics = sandbox.execd().metrics().await.expect("metrics");
    assert!(
        metrics.cpu_count > 0.0,
        "expected a nonzero CPU count, got {}",
        metrics.cpu_count
    );
    assert!(
        metrics.mem_total_mib > 0.0,
        "expected nonzero total memory, got {}",
        metrics.mem_total_mib
    );

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn stateless_code_execution_keeps_output_verbatim() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-code")]).await;

    // A request without a context runs as a shell command (execd defaults the
    // language to `Command`), streaming stdout as arbitrary fragments. The
    // fragments-mode join must preserve the byte sequence: two printf halves
    // must read back as one word, not be split by an inserted newline.
    let execution = sandbox
        .execd()
        .execute_code_collect(&RunCodeRequest {
            context: None,
            code: "printf 'frag'; printf 'ments\\n'".to_string(),
        })
        .await
        .expect("stateless code execution");
    assert_eq!(execution.exit_code, Some(0), "execution: {execution:?}");
    assert_eq!(
        execution.text().trim_end(),
        "fragments",
        "fragment join corrupted the output: {:?}",
        execution.text()
    );

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn jupyter_code_execution_returns_structured_results() {
    require_e2e!();
    if !code_interpreter_enabled() {
        eprintln!(
            "skipping: set RUN_CODE_INTERPRETER_E2E=true to run Jupyter code-execution suites"
        );
        return;
    }
    let sandbox = create_sandbox(&[("e2e.suite", "rust-jupyter")]).await;

    let context = sandbox
        .execd()
        .create_context("python")
        .await
        .expect("create python context");
    let execution = sandbox
        .execd()
        .execute_code_collect(&RunCodeRequest {
            context: Some(context),
            code: "print('hello-from-jupyter')\n2 + 2".to_string(),
        })
        .await
        .expect("execute python code");

    assert!(
        execution.success(),
        "execution failed: {:?}",
        execution.error
    );
    assert!(
        execution.text().contains("hello-from-jupyter"),
        "stdout: {:?}",
        execution.text()
    );
    let result = execution
        .results
        .iter()
        .find_map(|result| result.text().map(str::to_string))
        .expect("jupyter run must return a text result");
    assert_eq!(result.trim(), "4", "expression result: {result:?}");

    kill_quietly(&sandbox).await;
}
