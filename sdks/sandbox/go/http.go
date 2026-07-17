// Copyright 2026 Alibaba Group Holding Ltd.
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

package opensandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// defaultTimeout is 0 (no global timeout) because a non-zero value kills
// long-lived SSE streaming connections. Use per-request context deadlines
// instead to control individual call timeouts.
const defaultTimeout = 0

// streamResponseHeaderTimeout bounds how long an SSE request waits for the
// server to send response headers after the connection is established. It does
// NOT bound reading the (potentially long-lived) event stream body. Without it,
// a server that accepts the connection but never sends headers would hang the
// stream forever for callers using context.Background().
const streamResponseHeaderTimeout = 30 * time.Second

// Client is the base HTTP client shared by LifecycleClient and EgressClient.
type Client struct {
	baseURL    string
	apiKey     string
	authHeader string
	httpClient *http.Client
	timeout    *time.Duration // stored separately, applied after all options
	headers    map[string]string
	retry      *RetryConfig

	// streamClient is a dedicated HTTP client for SSE streaming, created lazily.
	// It disables connection pooling/keep-alive and has no overall request
	// timeout (see streamHTTPClient).
	streamClient *http.Client
	streamOnce   sync.Once
}

// streamHTTPClient returns a dedicated HTTP client for SSE streaming.
//
// Streaming differs from normal requests in two ways that make the shared
// httpClient unsuitable:
//   - It must not be bounded by an overall request timeout (http.Client.Timeout),
//     because that timeout also covers reading the response body and would kill
//     a long-running command's event stream mid-flight.
//   - It must not reuse pooled keep-alive connections: a connection silently
//     dropped by a load balancer while idle would stall the stream until it
//     times out. Each stream therefore uses a fresh, non-pooled connection.
//
// Connection setup is still bounded by the transport's DialTimeout,
// TLSHandshakeTimeout, and ResponseHeaderTimeout (the wait for response
// headers); only the (unbounded) body read is uncapped.
//
// The dedicated client is a shallow copy of the configured httpClient, so any
// caller-provided CookieJar / CheckRedirect policy still applies to streams;
// only Timeout (cleared) and Transport (replaced) differ.
func (c *Client) streamHTTPClient() *http.Client {
	c.streamOnce.Do(func() {
		sc := *c.httpClient // shallow copy: keep Jar, CheckRedirect, etc.
		sc.Timeout = 0      // no overall request timeout for long-lived streams
		if tr, ok := c.httpClient.Transport.(*http.Transport); ok && tr != nil {
			clone := tr.Clone()
			clone.DisableKeepAlives = true // do not pool/reuse stream connections
			// Bound only the "connected -> first response header" phase.
			// DialTimeout/TLSHandshakeTimeout do not cover waiting for response
			// headers, so without this a server that accepts the connection but
			// never sends headers would hang forever for context.Background()
			// callers. The SSE body read stays uncapped (Client.Timeout == 0).
			// Only set it when unset, to preserve an explicit caller value.
			if clone.ResponseHeaderTimeout == 0 {
				clone.ResponseHeaderTimeout = streamResponseHeaderTimeout
			}
			sc.Transport = clone
		}
		// else: custom RoundTripper is kept as-is via the shallow copy
		// (cannot toggle keep-alives on an unknown transport).
		c.streamClient = &sc
	})
	return c.streamClient
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets a custom http.Client.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) {
		cl.httpClient = c
	}
}

// WithTimeout sets the HTTP client timeout. The timeout is applied after all
// options, so it is safe to combine with WithHTTPClient in any order.
func WithTimeout(d time.Duration) Option {
	return func(cl *Client) {
		cl.timeout = &d
	}
}

// WithHeaders adds custom HTTP headers to all requests. These are applied
// before the auth and content-type headers, so they cannot override those.
func WithHeaders(headers map[string]string) Option {
	return func(cl *Client) {
		if cl.headers == nil {
			cl.headers = make(map[string]string, len(headers))
		}
		for k, v := range headers {
			cl.headers[k] = v
		}
	}
}

// WithAuthHeader overrides the default auth header name. Use this when the
// server expects a different header (e.g. "X-API-Key" instead of
// "OPEN-SANDBOX-API-KEY").
func WithAuthHeader(header string) Option {
	return func(cl *Client) {
		cl.authHeader = header
	}
}

// NewClient creates a new base Client. The authHeader parameter specifies
// which HTTP header carries the API key (e.g. "OPEN-SANDBOX-API-KEY" for
// lifecycle, "OPENSANDBOX-EGRESS-AUTH" for egress).
func NewClient(baseURL, apiKey, authHeader string, opts ...Option) *Client {
	c := &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		authHeader: authHeader,
		httpClient: &http.Client{
			Timeout:   defaultTimeout,
			Transport: DefaultTransport(),
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{
			Timeout:   defaultTimeout,
			Transport: DefaultTransport(),
		}
	} else if c.httpClient.Transport == nil {
		// Clone the caller's client to avoid mutating shared instances
		// (e.g. http.DefaultClient) which would leak the SDK's transport
		// settings into unrelated traffic in the same process.
		cloned := *c.httpClient
		cloned.Transport = DefaultTransport()
		c.httpClient = &cloned
	}
	// Apply deferred timeout after all options so it works regardless of
	// WithHTTPClient ordering and guards against a nil httpClient.
	if c.timeout != nil {
		c.httpClient.Timeout = *c.timeout
	}
	return c
}

// doRequest executes an HTTP request with JSON encoding and auth headers,
// retrying on transient errors if a RetryConfig is set.
// If body is nil, no request body is sent. If result is non-nil, the
// response body is decoded into it.
//
// For idempotent requests (GET/HEAD) it also transparently recovers from a
// stale pooled connection: some load balancers silently drop idle keep-alive
// connections without sending a FIN, so a reused connection can hang until the
// request timeout. When such a failure happens on a REUSED pooled connection
// the client purges idle connections and retries once on a fresh connection.
// The retry is gated on the failed attempt having reused a pooled connection
// (observed via httptrace), so a slow server hit over a brand-new connection is
// not retried and cannot double the effective timeout. This is always on and
// independent of the opt-in RetryConfig.
func (c *Client) doRequest(ctx context.Context, method, path string, body any, result any) error {
	return c.withRetry(ctx, func() error {
		var reused bool
		err := c.doRequestOnce(ctx, method, path, body, result, &reused)
		if err != nil && reused && c.shouldRetryOnFreshConn(ctx, method, err) {
			// The reused pooled connection was likely silently dropped by an
			// intermediary. Drop idle connections so the retry dials a new one.
			c.httpClient.CloseIdleConnections()
			err = c.doRequestOnce(ctx, method, path, body, result, nil)
		}
		return err
	})
}

// shouldRetryOnFreshConn reports whether a failed request should be retried once
// on a fresh connection. It targets connection-level failures (timeouts waiting
// for response headers, connection resets/EOF) that typically mean a reused
// pooled connection was already dead. It is restricted to idempotent methods
// and never fires when the caller's context is done (respecting cancellation)
// or when the server actually responded with an error status. The caller
// additionally gates this on the failed attempt having reused a pooled
// connection.
func (c *Client) shouldRetryOnFreshConn(ctx context.Context, method string, err error) bool {
	if err == nil {
		return false
	}
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	// Respect caller cancellation / deadline: the caller gave up, don't retry.
	if ctx.Err() != nil {
		return false
	}
	// The server responded (4xx/5xx): not a connection problem.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return false
	}
	// Network-level timeout (incl. http.Client.Timeout awaiting headers) or a
	// connection error (reset, closed) surfaced as a net.Error.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// Idle connection closed by the peer between pooling and reuse.
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// doRequestOnce is the single-attempt implementation of doRequest. If reused is
// non-nil it is set to whether this attempt was carried over a reused pooled
// connection (observed via httptrace GotConn).
func (c *Client) doRequestOnce(ctx context.Context, method, path string, body any, result any, reused *bool) error {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("opensandbox: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}

	if reused != nil {
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				*reused = info.Reused
			},
		})
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("opensandbox: create request: %w", err)
	}

	req.Header.Set("User-Agent", "OpenSandbox-Go-SDK/"+Version)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if c.apiKey != "" {
		req.Header.Set(c.authHeader, c.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("opensandbox: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return handleError(resp)
	}

	// No content (e.g. 204)
	if resp.StatusCode == http.StatusNoContent || result == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("opensandbox: decode response: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// doStreamRequest builds an HTTP request, executes it, and streams SSE events
// through handler. Connection setup is retried on transient errors; once
// streaming begins, errors are not retried (partial data may have been
// delivered to the handler).
func (c *Client) doStreamRequest(ctx context.Context, method, path string, body any, handler EventHandler) error {
	var resp *http.Response

	connectErr := c.withRetry(ctx, func() error {
		var bodyReader io.Reader
		if body != nil {
			buf, err := json.Marshal(body)
			if err != nil {
				return fmt.Errorf("opensandbox: marshal request: %w", err)
			}
			bodyReader = bytes.NewReader(buf)
		}

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
		if err != nil {
			return fmt.Errorf("opensandbox: create request: %w", err)
		}

		req.Header.Set("User-Agent", "OpenSandbox-Go-SDK/"+Version)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		if c.apiKey != "" {
			req.Header.Set(c.authHeader, c.apiKey)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "text/event-stream")

		// SSE uses a dedicated non-pooled, timeout-free client so long streams
		// are not killed by the overall request timeout and are never carried
		// over a stale pooled connection.
		r, err := c.streamHTTPClient().Do(req)
		if err != nil {
			return fmt.Errorf("opensandbox: do request: %w", err)
		}

		if r.StatusCode >= 400 {
			defer r.Body.Close()
			return handleError(r)
		}

		resp = r
		return nil
	})
	if connectErr != nil {
		return connectErr
	}

	return streamSSE(ctx, resp, handler)
}

// handleError reads the response body and returns an *APIError.
// It captures the Retry-After header for use by the retry loop.
func handleError(resp *http.Response) error {
	apiErr := &APIError{
		StatusCode: resp.StatusCode,
		RequestID:  resp.Header.Get("X-Request-Id"),
		RetryAfter: parseRetryAfter(resp),
	}
	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		apiErr.Response = ErrorResponse{
			Code:    http.StatusText(resp.StatusCode),
			Message: fmt.Sprintf("failed to read error response body: %v", readErr),
		}
		return apiErr
	}

	// Try to decode as JSON ErrorResponse; fall back to raw body.
	if err := json.Unmarshal(data, &apiErr.Response); err != nil || apiErr.Response.Code == "" {
		apiErr.Response = ErrorResponse{
			Code:    http.StatusText(resp.StatusCode),
			Message: string(data),
		}
	}
	return apiErr
}
