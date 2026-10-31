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

//! Error types shared by every client in this crate.

use std::time::Duration;

use reqwest::header::HeaderMap;
use thiserror::Error;

use crate::config::{WAKE_MARKER_EXPIRED, WAKE_MARKER_HEADER};

/// A non-2xx response returned by the lifecycle server, execd, or the egress
/// sidecar.
///
/// The server reports failures as `{"code": "...", "message": "..."}`. When
/// the body is not in that shape (for example a gateway HTML error page), the
/// HTTP reason phrase is used as the code and the raw body as the message.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ApiError {
    /// HTTP status code.
    pub status: u16,
    /// Machine-readable error code, for example `INVALID_ARGUMENT`.
    pub code: String,
    /// Human-readable error message.
    pub message: String,
    /// `X-Request-Id` echoed by the server, when present.
    pub request_id: Option<String>,
    /// Parsed `Retry-After` value, when the server sent one.
    pub retry_after: Option<Duration>,
    /// True when a `503` carried `OpenSandbox-Wake: expired`, meaning the
    /// request was parked by wake-on-access and was never forwarded.
    pub wake_expired: bool,
}

impl ApiError {
    /// Builds an error from a response status, headers, and body.
    pub fn from_parts(status: reqwest::StatusCode, headers: &HeaderMap, body: &[u8]) -> Self {
        let retry_after = headers
            .get(reqwest::header::RETRY_AFTER)
            .and_then(|v| v.to_str().ok())
            .and_then(parse_retry_after);
        let request_id = headers
            .get("x-request-id")
            .and_then(|v| v.to_str().ok())
            .map(str::to_string);
        let wake_expired = status == reqwest::StatusCode::SERVICE_UNAVAILABLE
            && headers
                .get(WAKE_MARKER_HEADER)
                .and_then(|v| v.to_str().ok())
                .map(|v| v == WAKE_MARKER_EXPIRED)
                .unwrap_or(false);

        #[derive(serde::Deserialize)]
        struct Body {
            code: Option<String>,
            message: Option<String>,
        }

        let parsed = serde_json::from_slice::<Body>(body).ok();
        let (code, message) = match parsed {
            Some(Body {
                code: Some(code),
                message,
            }) if !code.is_empty() => (
                code,
                message.unwrap_or_else(|| status.canonical_reason().unwrap_or("error").to_string()),
            ),
            _ => (
                status
                    .canonical_reason()
                    .unwrap_or("HTTP_ERROR")
                    .to_string(),
                String::from_utf8_lossy(body).trim().to_string(),
            ),
        };

        Self {
            status: status.as_u16(),
            code,
            message,
            request_id,
            retry_after,
            wake_expired,
        }
    }

    /// True for `404 Not Found`.
    pub fn is_not_found(&self) -> bool {
        self.status == 404
    }

    /// True for `409 Conflict`.
    pub fn is_conflict(&self) -> bool {
        self.status == 409
    }

    /// True for `412 Precondition Failed`.
    pub fn is_precondition_failed(&self) -> bool {
        self.status == 412
    }
}

impl std::fmt::Display for ApiError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}: {}", self.code, self.message)?;
        if let Some(request_id) = &self.request_id {
            write!(f, " (request_id: {request_id})")?;
        }
        Ok(())
    }
}

impl std::error::Error for ApiError {}

/// Every failure surfaced by the SDK.
#[derive(Debug, Error)]
pub enum Error {
    /// The server returned a non-2xx response.
    #[error("{0}")]
    Api(#[from] ApiError),
    /// A client-side argument failed validation.
    #[error("invalid argument `{field}`: {message}")]
    InvalidArgument {
        /// Offending field or argument name.
        field: String,
        /// Description of the violated constraint.
        message: String,
    },
    /// The HTTP transport failed.
    #[error(transparent)]
    Http(#[from] reqwest::Error),
    /// A URL could not be built or parsed.
    #[error(transparent)]
    Url(#[from] url::ParseError),
    /// A JSON payload could not be encoded or decoded.
    #[error("failed to decode JSON: {0}")]
    Json(#[from] serde_json::Error),
    /// Local I/O failed.
    #[error("i/o error: {0}")]
    Io(#[from] std::io::Error),
    /// The sandbox never reached `Running`.
    #[error("sandbox `{sandbox_id}` did not reach Running within {elapsed:?}")]
    RunningTimeout {
        /// Sandbox identifier.
        sandbox_id: String,
        /// How long the SDK waited.
        elapsed: Duration,
    },
    /// The execd service never answered a readiness probe.
    #[error("sandbox `{sandbox_id}` did not become ready within {elapsed:?}")]
    ReadyTimeout {
        /// Sandbox identifier.
        sandbox_id: String,
        /// How long the SDK waited.
        elapsed: Duration,
    },
    /// The sandbox entered `Failed` or `Terminated` while waiting.
    #[error("sandbox `{sandbox_id}` entered terminal state {state}: {reason}")]
    TerminalState {
        /// Sandbox identifier.
        sandbox_id: String,
        /// Terminal state reported by the server.
        state: String,
        /// Reason supplied by the server.
        reason: String,
    },
    /// The execd endpoint is unknown for this sandbox.
    #[error("execd endpoint is not resolved for sandbox `{0}`")]
    ExecdUnavailable(String),
    /// A streamed response was malformed.
    #[error("stream error: {0}")]
    Stream(String),
}

impl Error {
    /// Builds an [`Error::InvalidArgument`].
    pub fn invalid_argument(field: impl Into<String>, message: impl Into<String>) -> Self {
        Self::InvalidArgument {
            field: field.into(),
            message: message.into(),
        }
    }

    /// Returns the underlying [`ApiError`] when this is an API failure.
    pub fn api(&self) -> Option<&ApiError> {
        match self {
            Self::Api(err) => Some(err),
            _ => None,
        }
    }
}

/// Convenience alias used throughout the crate.
pub type Result<T> = std::result::Result<T, Error>;

fn parse_retry_after(value: &str) -> Option<Duration> {
    let value = value.trim();
    if let Ok(seconds) = value.parse::<u64>() {
        return Some(Duration::from_secs(seconds.min(60)));
    }
    chrono::DateTime::parse_from_rfc2822(value).ok().map(|at| {
        let now = chrono::Utc::now();
        let delta = at.with_timezone(&chrono::Utc) - now;
        delta
            .to_std()
            .unwrap_or_default()
            .min(Duration::from_secs(60))
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use reqwest::header::{HeaderValue, RETRY_AFTER};

    #[test]
    fn parses_structured_error_body() {
        let mut headers = HeaderMap::new();
        headers.insert("x-request-id", HeaderValue::from_static("req-1"));
        headers.insert(RETRY_AFTER, HeaderValue::from_static("2"));
        let err = ApiError::from_parts(
            reqwest::StatusCode::TOO_MANY_REQUESTS,
            &headers,
            br#"{"code":"POOL_EXHAUSTED","message":"no capacity"}"#,
        );
        assert_eq!(err.code, "POOL_EXHAUSTED");
        assert_eq!(err.message, "no capacity");
        assert_eq!(err.request_id.as_deref(), Some("req-1"));
        assert_eq!(err.retry_after, Some(Duration::from_secs(2)));
        assert!(!err.wake_expired);
    }

    #[test]
    fn falls_back_to_raw_body() {
        let err = ApiError::from_parts(
            reqwest::StatusCode::BAD_GATEWAY,
            &HeaderMap::new(),
            b"upstream exploded",
        );
        assert_eq!(err.code, "Bad Gateway");
        assert_eq!(err.message, "upstream exploded");
    }

    #[test]
    fn detects_wake_marker() {
        let mut headers = HeaderMap::new();
        headers.insert(WAKE_MARKER_HEADER, HeaderValue::from_static("expired"));
        headers.insert(RETRY_AFTER, HeaderValue::from_static("1"));
        let err = ApiError::from_parts(reqwest::StatusCode::SERVICE_UNAVAILABLE, &headers, b"");
        assert!(err.wake_expired);
    }

    #[test]
    fn retry_after_is_capped() {
        assert_eq!(parse_retry_after("600"), Some(Duration::from_secs(60)));
        assert_eq!(parse_retry_after("nonsense"), None);
    }
}
