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

//! Typed command/code execution events and result accumulation.

use std::pin::Pin;
use std::task::{Context, Poll};

use futures_util::Stream;
use serde_json::Value;

use crate::error::Result;
use crate::models::{
    ExecutionComplete, ExecutionError, ExecutionInit, ExecutionResult, OutputMessage,
};
use crate::sse::SseEvent;

/// One decoded execution event.
#[derive(Debug, Clone)]
pub enum ExecutionEvent {
    /// Execution started; carries the identifier used to interrupt it.
    Init(ExecutionInit),
    /// Standard output line.
    Stdout(OutputMessage),
    /// Standard error line.
    Stderr(OutputMessage),
    /// MIME-keyed result, produced by code execution.
    Result(ExecutionResult),
    /// Execution failed.
    Error(ExecutionError),
    /// Execution finished.
    Complete(ExecutionComplete),
    /// Event type not known to this SDK version. Preserved so callers can
    /// forward it.
    Other {
        /// Event type as reported on the wire.
        event: String,
        /// Raw payload.
        data: String,
    },
}

/// Parses one SSE/NDJSON event into an [`ExecutionEvent`].
///
/// execd has shipped two spellings for failures: a nested `error` object and
/// flat `ename`/`evalue`/`traceback` members. Both are accepted; non-JSON
/// payloads are surfaced as stdout so plain-text streams still work.
pub fn parse(event: &SseEvent) -> ExecutionEvent {
    let Ok(value) = serde_json::from_str::<Value>(&event.data) else {
        return ExecutionEvent::Stdout(OutputMessage {
            text: event.data.clone(),
            timestamp: 0,
        });
    };

    let kind = value
        .get("type")
        .and_then(Value::as_str)
        .unwrap_or(&event.event)
        .to_string();
    let timestamp = value
        .get("timestamp")
        .and_then(Value::as_i64)
        .unwrap_or_default();

    match kind.as_str() {
        "init" => ExecutionEvent::Init(ExecutionInit {
            id: value
                .get("text")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string(),
            timestamp,
        }),
        "stdout" => ExecutionEvent::Stdout(OutputMessage {
            text: value
                .get("text")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string(),
            timestamp,
        }),
        "stderr" => ExecutionEvent::Stderr(OutputMessage {
            text: value
                .get("text")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string(),
            timestamp,
        }),
        "result" => ExecutionEvent::Result(parse_result(&value, timestamp)),
        "error" => ExecutionEvent::Error(parse_error(&value, timestamp)),
        "execution_complete" => ExecutionEvent::Complete(ExecutionComplete {
            timestamp,
            execution_time: value
                .get("execution_time")
                .and_then(Value::as_i64)
                .unwrap_or_default(),
            exit_code: value
                .get("exit_code")
                .and_then(Value::as_i64)
                .map(|code| code as i32),
        }),
        "ping" => ExecutionEvent::Other {
            event: "ping".into(),
            data: event.data.clone(),
        },
        other => {
            // Unknown types with a `text` member are surfaced as stdout, which
            // matches the fallback used by the other SDKs.
            if let Some(text) = value.get("text").and_then(Value::as_str) {
                return ExecutionEvent::Stdout(OutputMessage {
                    text: text.to_string(),
                    timestamp,
                });
            }
            ExecutionEvent::Other {
                event: other.to_string(),
                data: event.data.clone(),
            }
        }
    }
}

fn parse_result(value: &Value, timestamp: i64) -> ExecutionResult {
    let mut results = std::collections::BTreeMap::new();
    match value.get("results") {
        Some(Value::Object(map)) => {
            for (mime, payload) in map {
                if let Some(text) = payload.as_str() {
                    results.insert(mime.clone(), text.to_string());
                }
            }
        }
        // Legacy shape: a bare `text` field means text/plain.
        _ => {
            if let Some(text) = value.get("text").and_then(Value::as_str) {
                results.insert("text/plain".into(), text.to_string());
            }
        }
    }
    ExecutionResult { results, timestamp }
}

fn parse_error(value: &Value, timestamp: i64) -> ExecutionError {
    let nested = value.get("error");
    let pick = |nested_key: &str, flat_key: &str| -> Option<Value> {
        nested
            .and_then(|n| n.get(nested_key))
            .or_else(|| value.get(flat_key))
            .cloned()
    };

    let name = pick("ename", "ename")
        .and_then(|v| v.as_str().map(str::to_string))
        .unwrap_or_default();
    let message = pick("evalue", "evalue")
        .and_then(|v| v.as_str().map(str::to_string))
        .unwrap_or_default();
    let traceback = pick("traceback", "traceback")
        .and_then(|v| {
            v.as_array().map(|lines| {
                lines
                    .iter()
                    .filter_map(|line| line.as_str().map(str::to_string))
                    .collect::<Vec<_>>()
            })
        })
        .unwrap_or_default();
    let exit_code = nested
        .and_then(|n| n.get("exit_code"))
        .or_else(|| value.get("exit_code"))
        .and_then(Value::as_i64)
        .map(|code| code as i32);

    // Older servers omit `error.exit_code` and put the status in `evalue`.
    let exit_code = exit_code.or_else(|| message.parse::<i32>().ok());

    ExecutionError {
        name,
        value: message,
        timestamp,
        traceback,
        exit_code,
    }
}

/// Accumulated result of an execution.
#[derive(Debug, Clone, Default)]
pub struct Execution {
    /// Execution identifier from the `init` event.
    pub id: String,
    /// Ordered stdout lines.
    pub stdout: Vec<OutputMessage>,
    /// Ordered stderr lines.
    pub stderr: Vec<OutputMessage>,
    /// MIME-keyed results.
    pub results: Vec<ExecutionResult>,
    /// Failure detail, when the execution failed.
    pub error: Option<ExecutionError>,
    /// Terminal event, when the stream reported completion.
    pub complete: Option<ExecutionComplete>,
    /// Process exit code. `None` when it could not be determined.
    pub exit_code: Option<i32>,
}

impl Execution {
    /// Creates an empty accumulator.
    pub fn new() -> Self {
        Self::default()
    }

    /// Whether the execution completed with exit code 0 and no error.
    pub fn success(&self) -> bool {
        self.error.is_none() && self.exit_code.unwrap_or_default() == 0
    }

    /// Joins stdout lines, inserting a newline between lines that did not
    /// already end with one (matching the other SDKs).
    pub fn text(&self) -> String {
        let mut out = String::new();
        for (index, message) in self.stdout.iter().enumerate() {
            if index > 0 {
                let previous = &self.stdout[index - 1].text;
                if !previous.ends_with('\n') && !previous.ends_with('\r') {
                    out.push('\n');
                }
            }
            out.push_str(&message.text);
        }
        out
    }

    /// Stderr joined the same way as [`Execution::text`].
    pub fn stderr_text(&self) -> String {
        let mut out = String::new();
        for (index, message) in self.stderr.iter().enumerate() {
            if index > 0 {
                let previous = &self.stderr[index - 1].text;
                if !previous.ends_with('\n') && !previous.ends_with('\r') {
                    out.push('\n');
                }
            }
            out.push_str(&message.text);
        }
        out
    }

    /// Applies one event to the accumulator.
    pub fn apply(&mut self, event: &ExecutionEvent) {
        match event {
            ExecutionEvent::Init(init) => self.id = init.id.clone(),
            ExecutionEvent::Stdout(message) => self.stdout.push(message.clone()),
            ExecutionEvent::Stderr(message) => self.stderr.push(message.clone()),
            ExecutionEvent::Result(result) => self.results.push(result.clone()),
            ExecutionEvent::Error(error) => {
                self.error = Some(error.clone());
                if let Some(code) = error.exit_code {
                    self.exit_code = Some(code);
                }
            }
            ExecutionEvent::Complete(complete) => {
                self.complete = Some(complete.clone());
                if self.exit_code.is_none() {
                    self.exit_code = complete.exit_code.or_else(|| {
                        if self.error.is_none() {
                            Some(0)
                        } else {
                            None
                        }
                    });
                }
            }
            ExecutionEvent::Other { .. } => {}
        }
    }
}

/// Stream of [`ExecutionEvent`]s produced by a command, session run, or code
/// execution.
pub struct ExecutionStream {
    inner: Pin<Box<dyn Stream<Item = Result<ExecutionEvent>> + Send>>,
}

impl ExecutionStream {
    pub(crate) fn new<S>(stream: S) -> Self
    where
        S: Stream<Item = Result<ExecutionEvent>> + Send + 'static,
    {
        Self {
            inner: Box::pin(stream),
        }
    }

    /// Reads the next event.
    pub async fn next_event(&mut self) -> Option<Result<ExecutionEvent>> {
        use futures_util::StreamExt;
        self.inner.next().await
    }

    /// Drains the stream into an [`Execution`], stopping at the terminal event.
    pub async fn collect(mut self) -> Result<Execution> {
        let mut execution = Execution::new();
        while let Some(event) = self.next_event().await {
            let event = event?;
            let complete = matches!(event, ExecutionEvent::Complete(_));
            execution.apply(&event);
            if complete {
                break;
            }
        }
        Ok(execution)
    }
}

impl Stream for ExecutionStream {
    type Item = Result<ExecutionEvent>;

    fn poll_next(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Option<Self::Item>> {
        self.inner.as_mut().poll_next(cx)
    }
}

impl std::fmt::Debug for ExecutionStream {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("ExecutionStream")
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sse::decode_all;

    fn events(body: &str) -> Vec<ExecutionEvent> {
        decode_all(body.as_bytes()).iter().map(parse).collect()
    }

    #[test]
    fn accumulates_stdout_and_exit_code() {
        let body = concat!(
            "{\"type\":\"init\",\"text\":\"exec-1\",\"timestamp\":1}\n\n",
            "{\"type\":\"stdout\",\"text\":\"hello\",\"timestamp\":2}\n\n",
            "{\"type\":\"stdout\",\"text\":\"world\",\"timestamp\":3}\n\n",
            "{\"type\":\"execution_complete\",\"execution_time\":12,\"exit_code\":0,\"timestamp\":4}\n\n",
        );
        let mut execution = Execution::new();
        for event in events(body) {
            execution.apply(&event);
        }
        assert_eq!(execution.id, "exec-1");
        assert_eq!(execution.text(), "hello\nworld");
        assert_eq!(execution.exit_code, Some(0));
        assert!(execution.success());
    }

    #[test]
    fn prefers_error_exit_code() {
        let body = "{\"type\":\"error\",\"error\":{\"ename\":\"CommandError\",\"evalue\":\"boom\",\"traceback\":[\"a\"],\"exit_code\":7},\"timestamp\":9}\n\n";
        let mut execution = Execution::new();
        for event in events(body) {
            execution.apply(&event);
        }
        assert_eq!(execution.exit_code, Some(7));
        assert!(!execution.success());
        assert_eq!(execution.error.as_ref().unwrap().traceback, vec!["a"]);
    }

    #[test]
    fn falls_back_to_flat_error_shape() {
        let body =
            "{\"type\":\"error\",\"ename\":\"CommandError\",\"evalue\":\"3\",\"timestamp\":1}\n\n";
        let mut execution = Execution::new();
        for event in events(body) {
            execution.apply(&event);
        }
        assert_eq!(execution.error.as_ref().unwrap().name, "CommandError");
        assert_eq!(execution.exit_code, Some(3));
    }

    #[test]
    fn parses_legacy_result_text() {
        let body = "{\"type\":\"result\",\"text\":\"42\",\"timestamp\":1}\n\n";
        let events = events(body);
        let ExecutionEvent::Result(result) = &events[0] else {
            panic!("expected result");
        };
        assert_eq!(result.text(), Some("42"));
    }

    #[test]
    fn parses_mime_results() {
        let body =
            "{\"type\":\"result\",\"results\":{\"text/plain\":\"4\",\"text/html\":\"<b>4</b>\"},\"timestamp\":1}\n\n";
        let events = events(body);
        let ExecutionEvent::Result(result) = &events[0] else {
            panic!("expected result");
        };
        assert_eq!(result.text(), Some("4"));
        assert_eq!(result.results.len(), 2);
    }

    #[test]
    fn unknown_events_do_not_break_the_stream() {
        let body = "{\"type\":\"ping\",\"timestamp\":1}\n\n{\"type\":\"future_event\",\"x\":1}\n\n";
        let events = events(body);
        assert!(matches!(events[0], ExecutionEvent::Other { .. }));
        assert!(matches!(events[1], ExecutionEvent::Other { .. }));
    }

    #[test]
    fn unknown_events_with_text_become_stdout() {
        let body = "{\"type\":\"new_event\",\"text\":\"forwarded\",\"timestamp\":1}\n\n";
        let events = events(body);
        let ExecutionEvent::Stdout(message) = &events[0] else {
            panic!("expected stdout");
        };
        assert_eq!(message.text, "forwarded");
    }

    #[test]
    fn non_json_payload_becomes_stdout() {
        let body = "data: plain text\n\n";
        let events = events(body);
        let ExecutionEvent::Stdout(message) = &events[0] else {
            panic!("expected stdout");
        };
        assert_eq!(message.text, "plain text");
    }
}
