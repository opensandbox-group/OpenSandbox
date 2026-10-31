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

//! Minimal HTTP/1.1 mock server used by the wire tests.
//!
//! The SDK is exercised against a real socket so that header injection,
//! request bodies, chunked streaming, and retries are verified end to end
//! without extra test dependencies.

#![allow(dead_code)]

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::io::{
    AsyncBufRead, AsyncBufReadExt, AsyncReadExt, AsyncWrite, AsyncWriteExt, BufReader,
};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::mpsc::{unbounded_channel, UnboundedReceiver, UnboundedSender};

/// A request received by the mock server.
#[derive(Debug, Clone)]
pub struct Recorded {
    /// HTTP method as sent on the wire.
    pub method: String,
    /// Path including the query string.
    pub path: String,
    /// Headers, with lower-case names.
    pub headers: BTreeMap<String, String>,
    /// Raw request body.
    pub body: Vec<u8>,
}

impl Recorded {
    /// Returns a header value, matched case-insensitively.
    pub fn header(&self, name: &str) -> Option<&str> {
        self.headers
            .get(&name.to_ascii_lowercase())
            .map(String::as_str)
    }

    /// Parses the body as JSON.
    pub fn json(&self) -> serde_json::Value {
        serde_json::from_slice(&self.body).expect("request body is JSON")
    }

    /// Renders the body as UTF-8.
    pub fn text(&self) -> String {
        String::from_utf8_lossy(&self.body).to_string()
    }
}

/// A scripted response.
#[derive(Debug, Clone)]
pub enum Reply {
    /// Response with an explicit `Content-Length`.
    Simple {
        /// Status code.
        status: u16,
        /// Extra response headers.
        headers: Vec<(String, String)>,
        /// Response body.
        body: Vec<u8>,
    },
    /// Chunked response; every chunk is written and flushed separately so
    /// partial lines reach the client across reads.
    Chunked {
        /// Status code.
        status: u16,
        /// Extra response headers.
        headers: Vec<(String, String)>,
        /// Body chunks, in order.
        chunks: Vec<Vec<u8>>,
    },
}

impl Reply {
    /// Empty response.
    pub fn empty(status: u16) -> Self {
        Self::Simple {
            status,
            headers: Vec::new(),
            body: Vec::new(),
        }
    }

    /// JSON response.
    pub fn json(status: u16, body: impl Into<String>) -> Self {
        Self::Simple {
            status,
            headers: vec![("content-type".into(), "application/json".into())],
            body: body.into().into_bytes(),
        }
    }

    /// `200 OK` with a JSON body.
    pub fn ok_json(body: impl Into<String>) -> Self {
        Self::json(200, body)
    }

    /// Plain-text response.
    pub fn text(status: u16, body: impl Into<Vec<u8>>) -> Self {
        Self::Simple {
            status,
            headers: vec![("content-type".into(), "text/plain".into())],
            body: body.into(),
        }
    }

    /// Streamed event response; each element is one flushed write.
    pub fn sse<I, S>(chunks: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<Vec<u8>>,
    {
        Self::Chunked {
            status: 200,
            headers: vec![("content-type".into(), "text/event-stream".into())],
            chunks: chunks.into_iter().map(Into::into).collect(),
        }
    }

    /// Adds one response header.
    pub fn with_header(mut self, name: impl Into<String>, value: impl Into<String>) -> Self {
        let header = (name.into(), value.into());
        match &mut self {
            Self::Simple { headers, .. } | Self::Chunked { headers, .. } => headers.push(header),
        }
        self
    }
}

/// A running mock server with a queue of scripted replies.
pub struct MockServer {
    domain: String,
    replies: Arc<Mutex<VecDeque<Reply>>>,
}

impl MockServer {
    /// Starts a server that answers the given replies in order.
    pub async fn start(replies: Vec<Reply>) -> (Self, UnboundedReceiver<Recorded>) {
        Self::start_with(|_| replies).await
    }

    /// Starts a server whose replies may depend on the bound `host:port`,
    /// which is useful when a reply has to point back at the server.
    pub async fn start_with<F>(build: F) -> (Self, UnboundedReceiver<Recorded>)
    where
        F: FnOnce(&str) -> Vec<Reply>,
    {
        let listener = TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind mock server");
        let domain = format!("127.0.0.1:{}", listener.local_addr().unwrap().port());
        let replies = Arc::new(Mutex::new(VecDeque::from(build(&domain))));
        let (sender, receiver) = unbounded_channel();

        let queue = replies.clone();
        tokio::spawn(async move {
            while let Ok((stream, _)) = listener.accept().await {
                let queue = queue.clone();
                let sender = sender.clone();
                tokio::spawn(async move {
                    let _ = serve(stream, queue, sender).await;
                });
            }
        });

        (Self { domain, replies }, receiver)
    }

    /// Bound `host:port`.
    pub fn domain(&self) -> &str {
        &self.domain
    }

    /// `http://host:port`.
    pub fn base_url(&self) -> String {
        format!("http://{}", self.domain)
    }

    /// Scripted replies that have not been consumed yet.
    pub fn queued(&self) -> usize {
        self.replies.lock().unwrap().len()
    }
}

/// Drains every request recorded so far.
pub fn drain(receiver: &mut UnboundedReceiver<Recorded>) -> Vec<Recorded> {
    let mut requests = Vec::new();
    while let Ok(request) = receiver.try_recv() {
        requests.push(request);
    }
    requests
}

async fn serve(
    stream: TcpStream,
    replies: Arc<Mutex<VecDeque<Reply>>>,
    sender: UnboundedSender<Recorded>,
) -> std::io::Result<()> {
    let (reader, mut writer) = stream.into_split();
    let mut reader = BufReader::new(reader);

    while let Some(request) = read_request(&mut reader).await? {
        let reply = replies.lock().unwrap().pop_front().unwrap_or_else(|| {
            Reply::json(
                500,
                r#"{"code":"MOCK_NO_REPLY","message":"no reply queued"}"#,
            )
        });
        let _ = sender.send(request);
        write_reply(&mut writer, reply).await?;
    }
    Ok(())
}

async fn read_request<R>(reader: &mut R) -> std::io::Result<Option<Recorded>>
where
    R: AsyncBufRead + Unpin,
{
    let mut request_line = String::new();
    if reader.read_line(&mut request_line).await? == 0 {
        return Ok(None);
    }

    let mut parts = request_line.trim_end().split(' ');
    let method = parts.next().unwrap_or_default().to_string();
    let path = parts.next().unwrap_or_default().to_string();

    let mut headers = BTreeMap::new();
    loop {
        let mut line = String::new();
        if reader.read_line(&mut line).await? == 0 {
            break;
        }
        let line = line.trim_end();
        if line.is_empty() {
            break;
        }
        if let Some((name, value)) = line.split_once(':') {
            headers.insert(name.trim().to_ascii_lowercase(), value.trim().to_string());
        }
    }

    let length: usize = headers
        .get("content-length")
        .and_then(|value| value.parse().ok())
        .unwrap_or(0);
    let mut body = vec![0u8; length];
    if length > 0 {
        reader.read_exact(&mut body).await?;
    }

    Ok(Some(Recorded {
        method,
        path,
        headers,
        body,
    }))
}

async fn write_reply<W>(writer: &mut W, reply: Reply) -> std::io::Result<()>
where
    W: AsyncWrite + Unpin,
{
    match reply {
        Reply::Simple {
            status,
            headers,
            body,
        } => {
            let mut head = format!("HTTP/1.1 {status} {}\r\n", reason(status));
            for (name, value) in headers {
                head.push_str(&format!("{name}: {value}\r\n"));
            }
            head.push_str(&format!("content-length: {}\r\n\r\n", body.len()));
            writer.write_all(head.as_bytes()).await?;
            writer.write_all(&body).await?;
        }
        Reply::Chunked {
            status,
            headers,
            chunks,
        } => {
            let mut head = format!("HTTP/1.1 {status} {}\r\n", reason(status));
            for (name, value) in headers {
                head.push_str(&format!("{name}: {value}\r\n"));
            }
            head.push_str("transfer-encoding: chunked\r\n\r\n");
            writer.write_all(head.as_bytes()).await?;
            for chunk in chunks {
                if chunk.is_empty() {
                    continue;
                }
                writer
                    .write_all(format!("{:x}\r\n", chunk.len()).as_bytes())
                    .await?;
                writer.write_all(&chunk).await?;
                writer.write_all(b"\r\n").await?;
                // A pause between chunks makes the client observe them
                // separately, which exercises partial-line buffering.
                writer.flush().await?;
                tokio::time::sleep(Duration::from_millis(2)).await;
            }
            writer.write_all(b"0\r\n\r\n").await?;
        }
    }
    writer.flush().await
}

fn reason(status: u16) -> &'static str {
    match status {
        200 => "OK",
        201 => "Created",
        202 => "Accepted",
        204 => "No Content",
        206 => "Partial Content",
        400 => "Bad Request",
        404 => "Not Found",
        409 => "Conflict",
        412 => "Precondition Failed",
        429 => "Too Many Requests",
        500 => "Internal Server Error",
        502 => "Bad Gateway",
        503 => "Service Unavailable",
        _ => "Unknown",
    }
}
