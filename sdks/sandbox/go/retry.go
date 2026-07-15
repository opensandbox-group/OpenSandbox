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
	"context"
	"errors"
	"math"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"time"
)

// RetryConfig controls automatic retry behavior for transient errors.
// A zero-value config disables retries.
type RetryConfig struct {
	// MaxRetries is the maximum number of retry attempts after the initial
	// request. 0 means no retries (only the original attempt).
	MaxRetries int

	// InitialBackoff is the delay before the first retry.
	InitialBackoff time.Duration

	// MaxBackoff caps the delay between retries.
	MaxBackoff time.Duration

	// Multiplier scales the backoff after each retry attempt.
	Multiplier float64

	// Jitter adds randomness to avoid thundering herd. Expressed as a
	// fraction of the computed delay: 0.0 = no jitter, 0.25 = +/-25%.
	Jitter float64

	// RetryableStatusCodes optionally overrides which HTTP status codes are
	// treated as transient for retry decisions. When empty, SDK defaults are
	// used (429, 502, 503, 504).
	RetryableStatusCodes []int
}

// DefaultRetryConfig returns a retry configuration suitable for most SDK
// consumers: 3 retries, 500ms initial backoff, 2x multiplier, 30s cap.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:     3,
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     30 * time.Second,
		Multiplier:     2.0,
		Jitter:         0.25,
		RetryableStatusCodes: []int{
			http.StatusTooManyRequests,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		},
	}
}

// WithRetry enables automatic retry with exponential backoff for transient
// errors and network failures. By default, transient status codes are
// 429/502/503/504; override RetryConfig.RetryableStatusCodes to customize.
func WithRetry(cfg RetryConfig) Option {
	return func(c *Client) {
		c.retry = &cfg
	}
}

// IsTransient reports whether the API error represents a transient server
// condition that may succeed on retry.
func (e *APIError) IsTransient() bool {
	return isTransientStatus(e.StatusCode)
}

// isTransientStatus classifies HTTP status codes.
//
//	Retryable: 429 (rate limit), 502, 503, 504 (infrastructure).
//	Permanent: everything else (400, 401, 403, 404, 409, 422, ...).
func isTransientStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (r *RetryConfig) isRetryableStatus(code int) bool {
	codes := r.RetryableStatusCodes
	if len(codes) == 0 {
		return isTransientStatus(code)
	}
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
}

// isTransientError checks whether err should trigger a retry. It handles
// *APIError (HTTP status classification) and net.Error (network-level).
func isTransientError(err error, cfg *RetryConfig) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if cfg != nil {
			return cfg.isRetryableStatus(apiErr.StatusCode)
		}
		return apiErr.IsTransient()
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// backoff computes the delay for attempt n (0-indexed) with optional jitter.
func (r *RetryConfig) backoff(attempt int) time.Duration {
	delay := float64(r.InitialBackoff) * math.Pow(r.Multiplier, float64(attempt))
	if delay > float64(r.MaxBackoff) {
		delay = float64(r.MaxBackoff)
	}
	if r.Jitter > 0 {
		jitter := delay * r.Jitter
		delay = delay - jitter + rand.Float64()*2*jitter
	}
	return time.Duration(delay)
}

// retryDelay returns the backoff duration, respecting Retry-After if present.
func retryDelay(cfg *RetryConfig, attempt int, err error) time.Duration {
	computed := cfg.backoff(attempt)

	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		if apiErr.RetryAfter > computed {
			return apiErr.RetryAfter
		}
	}
	return computed
}

// parseRetryAfter extracts the Retry-After header value as a duration.
// Returns 0 if the header is absent or unparseable.
func parseRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	val := resp.Header.Get("Retry-After")
	if val == "" {
		return 0
	}
	if secs, err := strconv.Atoi(val); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(val); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// retrySleep waits for d or until ctx is cancelled.
func retrySleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// withRetry executes fn, retrying on transient errors per the client's
// RetryConfig. If retry is nil or MaxRetries is 0, fn is called once.
func (c *Client) withRetry(ctx context.Context, fn func() error) error {
	if c.retry == nil || c.retry.MaxRetries == 0 {
		return fn()
	}

	var lastErr error
	for attempt := 0; attempt <= c.retry.MaxRetries; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if !isTransientError(lastErr, c.retry) {
			return lastErr
		}
		if attempt == c.retry.MaxRetries {
			break
		}
		delay := retryDelay(c.retry, attempt, lastErr)
		if err := retrySleep(ctx, delay); err != nil {
			return err
		}
	}
	return lastErr
}
