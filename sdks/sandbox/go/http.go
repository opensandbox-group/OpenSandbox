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
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultTimeout is 0 (no global timeout) because a non-zero value kills
// long-lived SSE streaming connections. Use per-request context deadlines
// instead to control individual call timeouts.
const defaultTimeout = 0

// Client is the base HTTP client shared by LifecycleClient and EgressClient.
type Client struct {
	baseURL    string
	apiKey     string
	authHeader string
	httpClient *http.Client
	timeout    *time.Duration // stored separately, applied after all options
	headers    map[string]string
	retry      *RetryConfig
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
func (c *Client) doRequest(ctx context.Context, method, path string, body any, result any) error {
	return c.withRetry(ctx, func() error {
		return c.doRequestOnce(ctx, method, path, body, result)
	})
}

// doRequestOnce is the single-attempt implementation of doRequest.
func (c *Client) doRequestOnce(ctx context.Context, method, path string, body any, result any) error {
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

		r, err := c.httpClient.Do(req)
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
