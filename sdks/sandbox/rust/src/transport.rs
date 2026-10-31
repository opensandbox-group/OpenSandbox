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

//! HTTP transport with authentication, header injection, and retry handling.

use std::collections::BTreeMap;
use std::time::{Duration, Instant};

use bytes::Bytes;
use reqwest::header::{HeaderMap, HeaderName, HeaderValue, ACCEPT, CONTENT_TYPE, USER_AGENT};
use reqwest::{Method, Response};
use serde::de::DeserializeOwned;
use serde::Serialize;

use crate::config::{ConnectionConfig, RetryConfig};
use crate::error::{ApiError, Error, Result};

pub(crate) const USER_AGENT_VALUE: &str =
    concat!("OpenSandbox-Rust-SDK/", env!("CARGO_PKG_VERSION"));

/// Maximum wait a server-supplied `Retry-After` may impose.
const RETRY_AFTER_CAP: Duration = Duration::from_secs(60);
/// Default wait for a wake retry when the gateway sent no `Retry-After`.
const WAKE_DEFAULT_WAIT: Duration = Duration::from_secs(1);

/// Shared HTTP layer used by the lifecycle and execd clients.
#[derive(Clone)]
pub(crate) struct Http {
    plain: reqwest::Client,
    streaming: reqwest::Client,
    base: String,
    auth_header: HeaderName,
    api_key: Option<String>,
    headers: HeaderMap,
    retry: RetryConfig,
}

impl Http {
    /// Builds a transport rooted at `base`.
    pub(crate) fn new(
        base: impl Into<String>,
        auth_header: &str,
        api_key: Option<String>,
        extra_headers: &BTreeMap<String, String>,
        retry: &RetryConfig,
        timeout: Duration,
    ) -> Result<Self> {
        let plain = reqwest::Client::builder()
            .timeout(timeout)
            .build()
            .map_err(Error::Http)?;
        // Streamed responses (SSE, metrics, file downloads) are unbounded: the
        // caller closes them through the stream's lifetime, not a total timeout.
        let streaming = reqwest::Client::builder()
            .connect_timeout(timeout)
            .pool_idle_timeout(Duration::from_secs(90))
            .build()
            .map_err(Error::Http)?;

        let mut headers = HeaderMap::new();
        for (name, value) in extra_headers {
            let name = HeaderName::from_bytes(name.as_bytes()).map_err(|_| {
                Error::invalid_argument("header", format!("invalid header name `{name}`"))
            })?;
            let value = HeaderValue::from_str(value).map_err(|_| {
                Error::invalid_argument("header", format!("invalid value for header `{name}`"))
            })?;
            headers.insert(name, value);
        }

        let auth_header = HeaderName::from_bytes(auth_header.as_bytes())
            .map_err(|_| Error::invalid_argument("auth_header", "invalid header name"))?;

        Ok(Self {
            plain,
            streaming,
            base: base.into().trim_end_matches('/').to_string(),
            auth_header,
            api_key: api_key.filter(|v| !v.is_empty()),
            headers,
            retry: retry.clone(),
        })
    }

    /// Convenience constructor from a [`ConnectionConfig`].
    pub(crate) fn from_config(
        config: &ConnectionConfig,
        base: &str,
        auth_header: &str,
    ) -> Result<Self> {
        Self::new(
            base,
            auth_header,
            config.api_key(),
            config.headers(),
            config.retry(),
            config.request_timeout(),
        )
    }

    /// Sends a request and decodes the JSON response.
    pub(crate) async fn json<T: DeserializeOwned>(
        &self,
        method: Method,
        path: &str,
        body: Option<&impl Serialize>,
    ) -> Result<T> {
        let body = encode_body(body)?;
        let idempotent = is_idempotent(&method);
        let response = self.execute(method, path, body, idempotent, true).await?;
        let bytes = response.bytes().await.map_err(Error::Http)?;
        if bytes.is_empty() {
            return Err(Error::Stream(
                "expected a JSON body but the response was empty".into(),
            ));
        }
        serde_json::from_slice(&bytes).map_err(Error::Json)
    }

    /// Sends a request and returns the JSON body together with the response
    /// headers, for endpoints whose headers carry protocol information (for
    /// example `Open-Sandbox-Origin`).
    pub(crate) async fn json_with_headers<T: DeserializeOwned>(
        &self,
        method: Method,
        path: &str,
        body: Option<&impl Serialize>,
    ) -> Result<(T, HeaderMap)> {
        let body = encode_body(body)?;
        let idempotent = is_idempotent(&method);
        let response = self.execute(method, path, body, idempotent, true).await?;
        let headers = response.headers().clone();
        let bytes = response.bytes().await.map_err(Error::Http)?;
        let value = serde_json::from_slice(&bytes).map_err(Error::Json)?;
        Ok((value, headers))
    }

    /// Sends a request and returns the raw body together with response headers.
    pub(crate) async fn bytes_with_headers(
        &self,
        method: Method,
        path: &str,
        accept: &str,
    ) -> Result<(Bytes, HeaderMap)> {
        let idempotent = is_idempotent(&method);
        let response = self
            .execute_with_accept(method, path, None, idempotent, true, Some(accept))
            .await?;
        let headers = response.headers().clone();
        let body = response.bytes().await.map_err(Error::Http)?;
        Ok((body, headers))
    }

    /// Returns a copy of this transport with one extra default header.
    pub(crate) fn with_header(&self, name: &str, value: &str) -> Result<Self> {
        let mut clone = self.clone();
        let name = HeaderName::from_bytes(name.as_bytes()).map_err(|_| {
            Error::invalid_argument("header", format!("invalid header name `{name}`"))
        })?;
        let value = HeaderValue::from_str(value).map_err(|_| {
            Error::invalid_argument("header", format!("invalid value for header `{name}`"))
        })?;
        clone.headers.insert(name, value);
        Ok(clone)
    }

    /// Uploads a multipart form. Uploads are never retried: the body is not
    /// replayable once the request has been sent.
    pub(crate) async fn multipart(&self, path: &str, form: reqwest::multipart::Form) -> Result<()> {
        let url = url::Url::parse(&format!("{}{}", self.base, path))?;
        let mut request = self.streaming.post(url);
        for (name, value) in &self.headers {
            request = request.header(name, value);
        }
        if let Some(api_key) = &self.api_key {
            request = request.header(self.auth_header.clone(), api_key.as_str());
        }
        request = request.header(USER_AGENT, USER_AGENT_VALUE).multipart(form);

        let response = request.send().await.map_err(Error::Http)?;
        let status = response.status();
        if status.is_success() {
            return Ok(());
        }
        let headers = response.headers().clone();
        let body = response.bytes().await.unwrap_or_default();
        Err(Error::Api(ApiError::from_parts(status, &headers, &body)))
    }

    /// Sends a request and discards the body.
    pub(crate) async fn empty(
        &self,
        method: Method,
        path: &str,
        body: Option<&impl Serialize>,
    ) -> Result<()> {
        let body = encode_body(body)?;
        let idempotent = is_idempotent(&method);
        self.execute(method, path, body, idempotent, true).await?;
        Ok(())
    }

    /// Sends a request and returns the raw response body.
    pub(crate) async fn bytes(
        &self,
        method: Method,
        path: &str,
        body: Option<&impl Serialize>,
        accept: &str,
    ) -> Result<Bytes> {
        let body = encode_body(body)?;
        let idempotent = is_idempotent(&method);
        let response = self
            .execute_with_accept(method, path, body, idempotent, true, Some(accept))
            .await?;
        response.bytes().await.map_err(Error::Http)
    }

    /// Sends a request and hands back the unread response for streaming.
    pub(crate) async fn stream(
        &self,
        method: Method,
        path: &str,
        body: Option<&impl Serialize>,
        accept: &str,
    ) -> Result<Response> {
        let body = encode_body(body)?;
        // Streaming requests only honor the wake contract: replaying a generic
        // transient failure could duplicate an execution.
        self.execute_with_accept(method, path, body, false, false, Some(accept))
            .await
    }

    async fn execute(
        &self,
        method: Method,
        path: &str,
        body: Option<Vec<u8>>,
        idempotent: bool,
        generic_retry: bool,
    ) -> Result<Response> {
        self.execute_with_accept(method, path, body, idempotent, generic_retry, None)
            .await
    }

    async fn execute_with_accept(
        &self,
        method: Method,
        path: &str,
        body: Option<Vec<u8>>,
        idempotent: bool,
        generic_retry: bool,
        accept: Option<&str>,
    ) -> Result<Response> {
        let started = Instant::now();
        let mut generic_retries = 0u32;
        let mut wake_retries = 0u32;

        loop {
            let request = self.request(&method, path, &body, accept)?;
            let outcome = match request.send().await {
                Ok(response) => self.check(response).await,
                Err(err) => Err(Error::Http(err)),
            };

            match outcome {
                Ok(response) => return Ok(response),
                Err(err) => {
                    match self.retry_delay(
                        &err,
                        idempotent,
                        generic_retry,
                        &mut generic_retries,
                        &mut wake_retries,
                        started,
                    ) {
                        Some(delay) => tokio::time::sleep(delay).await,
                        None => return Err(err),
                    }
                }
            }
        }
    }

    fn request(
        &self,
        method: &Method,
        path: &str,
        body: &Option<Vec<u8>>,
        accept: Option<&str>,
    ) -> Result<reqwest::RequestBuilder> {
        let url = format!("{}{}", self.base, path);
        let url = url::Url::parse(&url)?;
        let client = if accept == Some("text/event-stream") || path.contains("/files/download") {
            &self.streaming
        } else {
            &self.plain
        };

        let mut request = client.request(method.clone(), url);
        for (name, value) in &self.headers {
            request = request.header(name, value);
        }
        if let Some(api_key) = &self.api_key {
            request = request.header(self.auth_header.clone(), api_key.as_str());
        }
        request = request.header(USER_AGENT, USER_AGENT_VALUE);
        if let Some(accept) = accept {
            request = request.header(ACCEPT, accept);
        }
        if let Some(body) = body {
            request = request
                .header(CONTENT_TYPE, "application/json")
                .body(body.clone());
        }
        Ok(request)
    }

    async fn check(&self, response: Response) -> Result<Response> {
        let status = response.status();
        if status.is_success() {
            return Ok(response);
        }
        let headers = response.headers().clone();
        let body = response.bytes().await.unwrap_or_default();
        Err(Error::Api(ApiError::from_parts(status, &headers, &body)))
    }

    fn retry_delay(
        &self,
        err: &Error,
        idempotent: bool,
        generic_retry: bool,
        generic_retries: &mut u32,
        wake_retries: &mut u32,
        started: Instant,
    ) -> Option<Duration> {
        if let Error::Api(api) = err {
            if api.wake_expired && self.retry.wake_retry_enabled {
                let deadline = self.wake_deadline();
                if *wake_retries < self.retry.wake_max_attempts && started.elapsed() < deadline {
                    *wake_retries += 1;
                    return Some(api.retry_after.unwrap_or(WAKE_DEFAULT_WAIT));
                }
                return None;
            }
            if !generic_retry || *generic_retries >= self.retry.max_retries {
                return None;
            }
            if !self.retry.is_retryable(api.status, idempotent) {
                return None;
            }
            *generic_retries += 1;
            let backoff = self.backoff(*generic_retries);
            return Some(match api.retry_after {
                Some(retry_after) => backoff.max(retry_after.min(RETRY_AFTER_CAP)),
                None => backoff,
            });
        }

        // Transport failures are only safe to replay for idempotent methods.
        if !generic_retry || !idempotent || *generic_retries >= self.retry.max_retries {
            return None;
        }
        let Error::Http(http_err) = err else {
            return None;
        };
        if !(http_err.is_timeout() || http_err.is_connect() || http_err.is_request()) {
            return None;
        }
        *generic_retries += 1;
        Some(self.backoff(*generic_retries))
    }

    fn wake_deadline(&self) -> Duration {
        if self.retry.wake_overall_deadline.is_zero() {
            Duration::from_secs(60)
        } else {
            self.retry.wake_overall_deadline
        }
    }

    fn backoff(&self, attempt: u32) -> Duration {
        let base = self.retry.initial_backoff.as_secs_f64()
            * self.retry.multiplier.powi(attempt.saturating_sub(1) as i32);
        let capped = base.min(self.retry.max_backoff.as_secs_f64());
        let jitter = if self.retry.jitter <= 0.0 {
            1.0
        } else {
            1.0 + self.retry.jitter * (2.0 * pseudo_random() - 1.0)
        };
        Duration::from_secs_f64((capped * jitter).max(0.0))
    }
}

fn encode_body(body: Option<&impl Serialize>) -> Result<Option<Vec<u8>>> {
    match body {
        Some(value) => Ok(Some(serde_json::to_vec(value)?)),
        None => Ok(None),
    }
}

fn is_idempotent(method: &Method) -> bool {
    matches!(
        *method,
        Method::GET | Method::HEAD | Method::PUT | Method::DELETE | Method::OPTIONS
    )
}

fn pseudo_random() -> f64 {
    // Cheap jitter source: the sub-second component of the monotonic-ish clock.
    // Retry timing does not need cryptographic quality.
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.subsec_nanos())
        .unwrap_or(0);
    f64::from(nanos) / 1_000_000_000.0
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::{RetryConfig, API_KEY_HEADER};

    fn http() -> Http {
        Http::new(
            "http://127.0.0.1:1",
            API_KEY_HEADER,
            Some("secret".into()),
            &BTreeMap::new(),
            &RetryConfig::default(),
            Duration::from_secs(1),
        )
        .unwrap()
    }

    #[test]
    fn idempotency_matches_contract() {
        assert!(is_idempotent(&Method::GET));
        assert!(is_idempotent(&Method::DELETE));
        assert!(!is_idempotent(&Method::POST));
        assert!(!is_idempotent(&Method::PATCH));
    }

    #[test]
    fn backoff_grows_and_caps() {
        let http = http();
        let first = http.backoff(1);
        let third = http.backoff(3);
        assert!(third >= first);
        assert!(http.backoff(30) <= http.retry.max_backoff + Duration::from_secs(8));
    }

    #[test]
    fn non_idempotent_post_is_not_retried() {
        let http = http();
        let err = Error::Api(ApiError {
            status: 503,
            code: "UNAVAILABLE".into(),
            message: String::new(),
            request_id: None,
            retry_after: None,
            wake_expired: false,
        });
        let mut generic = 0;
        let mut wake = 0;
        assert!(http
            .retry_delay(&err, false, true, &mut generic, &mut wake, Instant::now())
            .is_none());
    }

    #[test]
    fn wake_marker_is_retried_for_post() {
        let http = http();
        let err = Error::Api(ApiError {
            status: 503,
            code: "WAKE_PENDING".into(),
            message: String::new(),
            request_id: None,
            retry_after: Some(Duration::from_millis(10)),
            wake_expired: true,
        });
        let mut generic = 0;
        let mut wake = 0;
        assert_eq!(
            http.retry_delay(&err, false, false, &mut generic, &mut wake, Instant::now()),
            Some(Duration::from_millis(10))
        );
        assert_eq!(wake, 1);
    }
}
