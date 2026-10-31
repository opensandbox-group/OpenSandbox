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

//! Incremental Server-Sent Events decoder.
//!
//! execd writes two shapes on the same endpoint:
//!
//! * standard SSE frames (`event: stdout`, `data: {...}`, blank line), and
//! * newline-delimited JSON where each line is a complete event object.
//!
//! Both are normalized into [`SseEvent`]. The decoder is chunk-boundary safe:
//! a line split across two network reads is buffered until it is complete.

use std::collections::VecDeque;

use futures_util::stream::{Stream, StreamExt};
use serde_json::Value;

use crate::error::{Error, Result};

/// One decoded event from an execd or lifecycle stream.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct SseEvent {
    /// Event type. Empty when the frame had no `event:` field and the payload
    /// carried no `type` member.
    pub event: String,
    /// Payload with multi-line `data:` fields joined by `\n`.
    pub data: String,
    /// Optional `id:` field.
    pub id: String,
}

impl SseEvent {
    /// Parses the payload as JSON, returning `None` when it is not JSON.
    pub fn json(&self) -> Option<Value> {
        serde_json::from_str(&self.data).ok()
    }
}

/// Stateful decoder that turns byte chunks into events.
#[derive(Debug, Default)]
pub struct SseDecoder {
    buffer: Vec<u8>,
    event: String,
    id: String,
    data: Vec<String>,
}

impl SseDecoder {
    /// Creates an empty decoder.
    pub fn new() -> Self {
        Self::default()
    }

    /// Feeds a chunk and returns every event completed by it.
    pub fn push(&mut self, chunk: &[u8]) -> Vec<SseEvent> {
        self.buffer.extend_from_slice(chunk);
        let mut events = Vec::new();

        while let Some(newline) = self.buffer.iter().position(|byte| *byte == b'\n') {
            let mut line: Vec<u8> = self.buffer.drain(..=newline).collect();
            line.pop();
            if line.last() == Some(&b'\r') {
                line.pop();
            }
            let line = String::from_utf8_lossy(&line).to_string();
            self.line(&line, &mut events);
        }

        events
    }

    /// Flushes a trailing event that was not terminated by a blank line.
    pub fn finish(&mut self) -> Option<SseEvent> {
        if !self.buffer.is_empty() {
            let line = String::from_utf8_lossy(&self.buffer).trim_end().to_string();
            self.buffer.clear();
            let mut events = Vec::new();
            self.line(&line, &mut events);
            if let Some(event) = events.pop() {
                return Some(event);
            }
        }
        self.dispatch()
    }

    fn line(&mut self, line: &str, events: &mut Vec<SseEvent>) {
        if line.is_empty() {
            if let Some(event) = self.dispatch() {
                events.push(event);
            }
            return;
        }

        // Comment / keep-alive frame.
        if line.starts_with(':') {
            return;
        }

        // NDJSON: a complete JSON object on a single line is an event on its own.
        if line.starts_with('{') {
            if let Ok(value) = serde_json::from_str::<Value>(line) {
                let event = value
                    .get("type")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_string();
                events.push(SseEvent {
                    event,
                    data: line.to_string(),
                    id: String::new(),
                });
                return;
            }
        }

        let (field, value) = match line.split_once(':') {
            Some((field, value)) => (field, value.strip_prefix(' ').unwrap_or(value)),
            None => (line, ""),
        };

        match field {
            "data" => self.data.push(value.to_string()),
            "event" => self.event = value.to_string(),
            "id" => self.id = value.to_string(),
            "retry" => {}
            _ => {}
        }
    }

    fn dispatch(&mut self) -> Option<SseEvent> {
        if self.data.is_empty() {
            self.event.clear();
            self.id.clear();
            return None;
        }
        let event = SseEvent {
            event: std::mem::take(&mut self.event),
            data: self.data.join("\n"),
            id: std::mem::take(&mut self.id),
        };
        self.data.clear();
        Some(event)
    }
}

/// Decodes an entire buffer, useful for tests and non-streaming callers.
pub fn decode_all(bytes: &[u8]) -> Vec<SseEvent> {
    let mut decoder = SseDecoder::new();
    let mut events = decoder.push(bytes);
    if let Some(last) = decoder.finish() {
        events.push(last);
    }
    events
}

/// Converts a streaming HTTP response into a stream of events.
///
/// The returned stream is `Unpin`, so callers can poll it directly with
/// [`futures_util::StreamExt::next`] without pinning it first.
pub fn into_stream(
    response: reqwest::Response,
) -> impl Stream<Item = Result<SseEvent>> + Send + Unpin + 'static {
    struct State {
        decoder: SseDecoder,
        pending: VecDeque<SseEvent>,
        body: std::pin::Pin<
            Box<dyn Stream<Item = std::result::Result<bytes::Bytes, reqwest::Error>> + Send>,
        >,
        done: bool,
    }

    let state = State {
        decoder: SseDecoder::new(),
        pending: VecDeque::new(),
        body: Box::pin(response.bytes_stream()),
        done: false,
    };

    Box::pin(futures_util::stream::unfold(
        state,
        |mut state| async move {
            loop {
                if let Some(event) = state.pending.pop_front() {
                    return Some((Ok(event), state));
                }
                if state.done {
                    return None;
                }
                match state.body.next().await {
                    Some(Ok(chunk)) => {
                        let events = state.decoder.push(&chunk);
                        state.pending.extend(events);
                    }
                    Some(Err(err)) => {
                        state.done = true;
                        return Some((Err(Error::Http(err)), state));
                    }
                    None => {
                        state.done = true;
                        if let Some(event) = state.decoder.finish() {
                            state.pending.push_back(event);
                        }
                    }
                }
            }
        },
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decodes_standard_sse() {
        let input = b"event: stdout\ndata: {\"type\":\"stdout\",\"text\":\"hi\"}\n\n";
        let events = decode_all(input);
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].event, "stdout");
        assert_eq!(events[0].data, "{\"type\":\"stdout\",\"text\":\"hi\"}");
    }

    #[test]
    fn decodes_ndjson() {
        let input = b"{\"type\":\"stdout\",\"text\":\"a\"}\n\n{\"type\":\"execution_complete\",\"exit_code\":0}\n";
        let events = decode_all(input);
        assert_eq!(events.len(), 2);
        assert_eq!(events[0].event, "stdout");
        assert_eq!(events[1].event, "execution_complete");
        assert_eq!(
            events[1].data,
            "{\"type\":\"execution_complete\",\"exit_code\":0}"
        );
    }

    #[test]
    fn handles_chunk_boundaries() {
        let mut decoder = SseDecoder::new();
        assert!(decoder.push(b"event: out").is_empty());
        assert!(decoder.push(b"put\ndata: par").is_empty());
        let events = decoder.push(b"tial\n\n");
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].event, "output");
        assert_eq!(events[0].data, "partial");
    }

    #[test]
    fn joins_multiline_data_and_ignores_comments() {
        let input = b": keep-alive\ndata: line1\ndata: line2\n\n";
        let events = decode_all(input);
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].data, "line1\nline2");
        assert!(events[0].event.is_empty());
    }

    #[test]
    fn flushes_unterminated_tail() {
        let mut decoder = SseDecoder::new();
        decoder.push(b"data: tail\n");
        let event = decoder.finish().expect("tail event");
        assert_eq!(event.data, "tail");
        assert!(decoder.finish().is_none());
    }

    #[test]
    fn keeps_blank_lines_inside_multiline_payloads() {
        let input = b"data: a\n\ndata: b\n\n";
        let events = decode_all(input);
        assert_eq!(events.len(), 2);
        assert_eq!(events[0].data, "a");
        assert_eq!(events[1].data, "b");
    }
}
