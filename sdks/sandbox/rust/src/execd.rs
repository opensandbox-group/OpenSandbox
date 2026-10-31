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

//! Data-plane client for execd (port 44772).
//!
//! execd is reached directly inside the sandbox. Its access token arrives in
//! the endpoint headers returned by the lifecycle API, so callers normally get
//! an `ExecdClient` from [`crate::sandbox::Sandbox`] rather than building one.

use std::collections::BTreeMap;

use bytes::Bytes;
use futures_util::StreamExt;
use reqwest::multipart::{Form, Part};
use reqwest::Method;
use url::form_urlencoded;

use crate::config::{ConnectionConfig, EXECD_TOKEN_HEADER};
use crate::error::{Error, Result};
use crate::execution::{self, Execution, ExecutionStream};
use crate::models::{
    CodeContext, CommandLogsResponse, CommandStatusResponse, CreateContextRequest,
    CreateSessionRequest, FileInfo, FileMetadata, Metrics, MoveItem, Permission, ReplaceItem,
    ReplaceResult, RunCodeRequest, RunCommandRequest, RunInSessionRequest, Session,
};
use crate::sse;
use crate::transport::Http;

/// Header carrying the background log cursor.
pub const COMMANDS_TAIL_CURSOR_HEADER: &str = "EXECD-COMMANDS-TAIL-CURSOR";

/// One file in a multi-file upload.
#[derive(Debug, Clone)]
pub struct UploadEntry {
    /// Multipart file name. Defaults to `file` when empty.
    pub file_name: String,
    /// File content.
    pub bytes: Vec<u8>,
    /// Destination metadata, including the target path.
    pub metadata: FileMetadata,
}

impl UploadEntry {
    /// Builds an entry targeting `metadata.path`.
    pub fn new(metadata: FileMetadata, bytes: impl Into<Vec<u8>>) -> Self {
        Self {
            file_name: "file".into(),
            bytes: bytes.into(),
            metadata,
        }
    }

    /// Overrides the multipart file name.
    pub fn with_file_name(mut self, name: impl Into<String>) -> Self {
        self.file_name = name.into();
        self
    }
}

/// Client for the execd HTTP API.
#[derive(Clone)]
pub struct ExecdClient {
    http: Http,
}

impl ExecdClient {
    /// Builds a client for an endpoint URL and the headers returned by the
    /// lifecycle API.
    pub fn new(
        endpoint_url: &str,
        headers: &BTreeMap<String, String>,
        config: &ConnectionConfig,
    ) -> Result<Self> {
        // The access token travels as a normal extra header: execd, or the
        // lifecycle proxy in front of it, reads `X-EXECD-ACCESS-TOKEN`, and the
        // server-proxy mode replaces it with OPEN-SANDBOX-API-KEY.
        let http = Http::new(
            endpoint_url,
            EXECD_TOKEN_HEADER,
            None,
            &config.data_plane_headers(headers),
            config.retry(),
            config.request_timeout(),
        )?;
        Ok(Self { http })
    }

    /// Verifies that execd is alive (`GET /ping`).
    pub async fn ping(&self) -> Result<()> {
        self.http.empty(Method::GET, "/ping", None::<&()>).await
    }

    // -- commands ----------------------------------------------------------

    /// Starts a command and returns its event stream.
    ///
    /// For background commands the stream ends after the startup
    /// acknowledgement; read the output with [`ExecdClient::command_logs`].
    pub async fn run_command(&self, request: &RunCommandRequest) -> Result<ExecutionStream> {
        let response = self
            .http
            .stream(Method::POST, "/command", Some(request), "text/event-stream")
            .await?;
        Ok(ExecutionStream::new(
            sse::into_stream(response).map(|item| item.map(|event| execution::parse(&event))),
        ))
    }

    /// Runs a command and drains its output into an [`Execution`].
    pub async fn run_command_collect(&self, request: &RunCommandRequest) -> Result<Execution> {
        self.run_command(request).await?.collect().await
    }

    /// Interrupts a running command.
    pub async fn interrupt_command(&self, execution_id: &str) -> Result<()> {
        let query = form_urlencoded::Serializer::new(String::new())
            .append_pair("id", execution_id)
            .finish();
        self.http
            .empty(Method::DELETE, &format!("/command?{query}"), None::<&()>)
            .await
    }

    /// Reads the status of a command.
    pub async fn command_status(&self, execution_id: &str) -> Result<CommandStatusResponse> {
        self.http
            .json(
                Method::GET,
                &format!("/command/status/{}", segment(execution_id)),
                None::<&()>,
            )
            .await
    }

    /// Reads background command logs from `cursor` (0-based line number).
    ///
    /// The returned cursor is the value to pass on the next call; it is also
    /// available through the `EXECD-COMMANDS-TAIL-CURSOR` response header.
    pub async fn command_logs(
        &self,
        execution_id: &str,
        cursor: Option<u64>,
    ) -> Result<CommandLogsResponse> {
        let mut path = format!("/command/{}/logs", segment(execution_id));
        if let Some(cursor) = cursor {
            path.push_str(&format!("?cursor={cursor}"));
        }
        let (body, headers) = self
            .http
            .bytes_with_headers(Method::GET, &path, "text/plain")
            .await?;
        let cursor = headers
            .get(COMMANDS_TAIL_CURSOR_HEADER)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.trim().parse::<i64>().ok())
            .unwrap_or_default();
        Ok(CommandLogsResponse {
            output: String::from_utf8_lossy(&body).to_string(),
            cursor,
        })
    }

    // -- bash sessions -----------------------------------------------------

    /// Creates a persistent bash session.
    pub async fn create_session(&self, request: &CreateSessionRequest) -> Result<Session> {
        self.http
            .json(Method::POST, "/session", Some(request))
            .await
    }

    /// Runs a command inside an existing session.
    pub async fn run_in_session(
        &self,
        session_id: &str,
        request: &RunInSessionRequest,
    ) -> Result<ExecutionStream> {
        let response = self
            .http
            .stream(
                Method::POST,
                &format!("/session/{}/run", segment(session_id)),
                Some(request),
                "text/event-stream",
            )
            .await?;
        Ok(ExecutionStream::new(
            sse::into_stream(response).map(|item| item.map(|event| execution::parse(&event))),
        ))
    }

    /// Deletes a bash session.
    pub async fn delete_session(&self, session_id: &str) -> Result<()> {
        self.http
            .empty(
                Method::DELETE,
                &format!("/session/{}", segment(session_id)),
                None::<&()>,
            )
            .await
    }

    // -- code execution ----------------------------------------------------

    /// Creates a code execution context.
    pub async fn create_context(&self, language: impl Into<String>) -> Result<CodeContext> {
        let body = CreateContextRequest {
            language: language.into(),
        };
        self.http
            .json(Method::POST, "/code/context", Some(&body))
            .await
    }

    /// Lists contexts, optionally filtered by language.
    pub async fn list_contexts(&self, language: Option<&str>) -> Result<Vec<CodeContext>> {
        let path = match language {
            Some(language) => format!(
                "/code/contexts?{}",
                form_urlencoded::Serializer::new(String::new())
                    .append_pair("language", language)
                    .finish()
            ),
            None => "/code/contexts".to_string(),
        };
        self.http.json(Method::GET, &path, None::<&()>).await
    }

    /// Fetches one context.
    pub async fn get_context(&self, context_id: &str) -> Result<CodeContext> {
        self.http
            .json(
                Method::GET,
                &format!("/code/contexts/{}", segment(context_id)),
                None::<&()>,
            )
            .await
    }

    /// Deletes one context.
    pub async fn delete_context(&self, context_id: &str) -> Result<()> {
        self.http
            .empty(
                Method::DELETE,
                &format!("/code/contexts/{}", segment(context_id)),
                None::<&()>,
            )
            .await
    }

    /// Deletes every context of a language.
    pub async fn delete_contexts_by_language(&self, language: &str) -> Result<()> {
        let query = form_urlencoded::Serializer::new(String::new())
            .append_pair("language", language)
            .finish();
        self.http
            .empty(
                Method::DELETE,
                &format!("/code/contexts?{query}"),
                None::<&()>,
            )
            .await
    }

    /// Executes code and streams stdout/stderr/result events.
    pub async fn execute_code(&self, request: &RunCodeRequest) -> Result<ExecutionStream> {
        let response = self
            .http
            .stream(Method::POST, "/code", Some(request), "text/event-stream")
            .await?;
        Ok(ExecutionStream::new(
            sse::into_stream(response).map(|item| item.map(|event| execution::parse(&event))),
        ))
    }

    /// Executes code and drains the resulting events.
    pub async fn execute_code_collect(&self, request: &RunCodeRequest) -> Result<Execution> {
        self.execute_code(request).await?.collect().await
    }

    /// Interrupts a running code execution.
    pub async fn interrupt_code(&self, execution_id: &str) -> Result<()> {
        let query = form_urlencoded::Serializer::new(String::new())
            .append_pair("id", execution_id)
            .finish();
        self.http
            .empty(Method::DELETE, &format!("/code?{query}"), None::<&()>)
            .await
    }

    // -- filesystem --------------------------------------------------------

    /// Reads metadata for one or more paths.
    pub async fn file_info(&self, paths: &[String]) -> Result<BTreeMap<String, FileInfo>> {
        if paths.is_empty() {
            return Err(Error::invalid_argument(
                "paths",
                "at least one path is required",
            ));
        }
        let mut serializer = form_urlencoded::Serializer::new(String::new());
        for path in paths {
            serializer.append_pair("path", path);
        }
        self.http
            .json(
                Method::GET,
                &format!("/files/info?{}", serializer.finish()),
                None::<&()>,
            )
            .await
    }

    /// Deletes one or more files.
    pub async fn delete_files(&self, paths: &[String]) -> Result<()> {
        let mut serializer = form_urlencoded::Serializer::new(String::new());
        for path in paths {
            serializer.append_pair("path", path);
        }
        self.http
            .empty(
                Method::DELETE,
                &format!("/files?{}", serializer.finish()),
                None::<&()>,
            )
            .await
    }

    /// Changes ownership and mode.
    pub async fn set_permissions(&self, permissions: &BTreeMap<String, Permission>) -> Result<()> {
        self.http
            .empty(Method::POST, "/files/permissions", Some(permissions))
            .await
    }

    /// Moves or renames files.
    pub async fn move_files(&self, moves: &[MoveItem]) -> Result<()> {
        self.http
            .empty(Method::POST, "/files/mv", Some(&moves))
            .await
    }

    /// Searches for files matching a glob.
    pub async fn search_files(&self, dir: &str, pattern: Option<&str>) -> Result<Vec<FileInfo>> {
        let mut serializer = form_urlencoded::Serializer::new(String::new());
        serializer.append_pair("path", dir);
        if let Some(pattern) = pattern.filter(|value| !value.is_empty()) {
            serializer.append_pair("pattern", pattern);
        }
        self.http
            .json(
                Method::GET,
                &format!("/files/search?{}", serializer.finish()),
                None::<&()>,
            )
            .await
    }

    /// Lists a directory up to `depth` levels (server default 1).
    pub async fn list_directory(&self, path: &str, depth: Option<u32>) -> Result<Vec<FileInfo>> {
        let mut serializer = form_urlencoded::Serializer::new(String::new());
        serializer.append_pair("path", path);
        if let Some(depth) = depth {
            serializer.append_pair("depth", &depth.to_string());
        }
        self.http
            .json(
                Method::GET,
                &format!("/directories/list?{}", serializer.finish()),
                None::<&()>,
            )
            .await
    }

    /// Creates a directory tree. `mode` uses octal digits as a decimal integer
    /// (`755` for `rwxr-xr-x`).
    pub async fn create_directory(&self, path: &str, mode: u32) -> Result<()> {
        let body: BTreeMap<&str, BTreeMap<&str, u32>> =
            BTreeMap::from([(path, BTreeMap::from([("mode", mode)]))]);
        self.http
            .empty(Method::POST, "/directories", Some(&body))
            .await
    }

    /// Deletes a directory recursively.
    pub async fn delete_directory(&self, path: &str) -> Result<()> {
        let query = form_urlencoded::Serializer::new(String::new())
            .append_pair("path", path)
            .finish();
        self.http
            .empty(
                Method::DELETE,
                &format!("/directories?{query}"),
                None::<&()>,
            )
            .await
    }

    /// Replaces literal text in files.
    ///
    /// With `verbose`, the server returns per-file replacement counts.
    pub async fn replace_in_files(
        &self,
        replacements: &BTreeMap<String, ReplaceItem>,
        verbose: bool,
    ) -> Result<Option<BTreeMap<String, ReplaceResult>>> {
        let path = if verbose {
            "/files/replace?verbose=true"
        } else {
            "/files/replace"
        };
        if verbose {
            self.http
                .json(Method::POST, path, Some(replacements))
                .await
                .map(Some)
        } else {
            self.http
                .empty(Method::POST, path, Some(replacements))
                .await
                .map(|_| None)
        }
    }

    /// Uploads one file.
    pub async fn upload_file(&self, entry: UploadEntry) -> Result<()> {
        self.upload_files(&[entry]).await
    }

    /// Uploads one or more files in a single multipart request.
    ///
    /// Each file is preceded by a `metadata` part carrying its destination
    /// path, exactly as execd expects.
    pub async fn upload_files(&self, entries: &[UploadEntry]) -> Result<()> {
        if entries.is_empty() {
            return Err(Error::invalid_argument(
                "entries",
                "at least one entry is required",
            ));
        }

        let mut form = Form::new();
        for (index, entry) in entries.iter().enumerate() {
            if entry.metadata.path.is_empty() {
                return Err(Error::invalid_argument(
                    format!("entries[{index}].metadata.path"),
                    "path is required",
                ));
            }
            let metadata = serde_json::to_vec(&entry.metadata)?;
            let part = Part::bytes(metadata)
                .file_name("metadata")
                .mime_str("application/json")
                .map_err(Error::Http)?;
            form = form.part("metadata", part);

            let file_name = if entry.file_name.is_empty() {
                "file".to_string()
            } else {
                entry.file_name.clone()
            };
            form = form.part(
                "file",
                Part::bytes(entry.bytes.clone()).file_name(file_name),
            );
        }

        self.http.multipart("/files/upload", form).await
    }

    /// Downloads a file.
    ///
    /// `range` sends a raw `Range` header (`bytes=0-1023`). `offset`/`limit`
    /// select a line range instead; the server rejects mixing both.
    pub async fn download_file(
        &self,
        path: &str,
        range: Option<&str>,
        offset: Option<u64>,
        limit: Option<u64>,
    ) -> Result<Bytes> {
        if range.is_some() && (offset.is_some() || limit.is_some()) {
            return Err(Error::invalid_argument(
                "range",
                "range and offset/limit are mutually exclusive",
            ));
        }
        let mut serializer = form_urlencoded::Serializer::new(String::new());
        serializer.append_pair("path", path);
        if let Some(offset) = offset.filter(|value| *value > 0) {
            serializer.append_pair("offset", &offset.to_string());
        }
        if let Some(limit) = limit.filter(|value| *value > 0) {
            serializer.append_pair("limit", &limit.to_string());
        }
        let request_path = format!("/files/download?{}", serializer.finish());

        let http = match range {
            Some(range) => self.http.with_header("Range", range)?,
            None => self.http.clone(),
        };
        http.bytes(Method::GET, &request_path, None::<&()>, "*/*")
            .await
    }

    // -- metrics -----------------------------------------------------------

    /// Reads current resource usage.
    pub async fn metrics(&self) -> Result<Metrics> {
        self.http.json(Method::GET, "/metrics", None::<&()>).await
    }

    /// Streams resource usage, roughly one sample per second.
    pub async fn watch_metrics(
        &self,
    ) -> Result<impl futures_util::Stream<Item = Result<Metrics>> + Send + Unpin> {
        let response = self
            .http
            .stream(
                Method::GET,
                "/metrics/watch",
                None::<&()>,
                "text/event-stream",
            )
            .await?;
        Ok(sse::into_stream(response).map(|item| {
            let event = item?;
            serde_json::from_str::<Metrics>(&event.data).map_err(Error::Json)
        }))
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
    fn upload_entry_defaults_file_name() {
        let entry = UploadEntry::new(
            FileMetadata {
                path: "/tmp/a.txt".into(),
                ..Default::default()
            },
            b"hi".to_vec(),
        );
        assert_eq!(entry.file_name, "file");
        assert_eq!(entry.metadata.path, "/tmp/a.txt");
    }

    #[test]
    fn encodes_execution_ids() {
        assert_eq!(segment("exec/1"), "exec%2F1");
    }
}
