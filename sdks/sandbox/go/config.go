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
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// ConnectionConfig holds the configuration for connecting to an OpenSandbox server.
type ConnectionConfig struct {
	// Domain is the server address (e.g. "localhost:8080").
	// Falls back to OPEN_SANDBOX_DOMAIN env var, then DefaultDomain.
	Domain string

	// Protocol is "http" or "https".
	// Falls back to OPEN_SANDBOX_PROTOCOL env var, then DefaultProtocol.
	Protocol string

	// APIKey is the authentication token.
	// Falls back to OPEN_SANDBOX_API_KEY env var.
	APIKey string

	// UseServerProxy routes execd/egress requests through the sandbox server
	// instead of connecting directly to the sandbox endpoint.
	UseServerProxy bool

	// RequestTimeout is the timeout for non-streaming HTTP requests.
	// Zero means no timeout. Defaults to DefaultRequestTimeout.
	RequestTimeout time.Duration

	// Headers are custom HTTP headers added to all requests.
	Headers map[string]string

	// HTTPClient is an optional custom HTTP client. If nil, a default is created.
	HTTPClient *http.Client

	// AuthHeader overrides the default lifecycle auth header name.
	// Default is "OPEN-SANDBOX-API-KEY". Use "X-API-Key" for proxied deployments.
	AuthHeader string

	// Retry enables automatic retry with exponential backoff for transient
	// errors. Defaults to retrying 429/502/503/504; override
	// RetryConfig.RetryableStatusCodes for custom policies. If nil, requests
	// are not retried.
	// Use DefaultRetryConfig() for sensible defaults.
	Retry *RetryConfig

	// Transport configures HTTP connection pooling. If nil and HTTPClient
	// is also nil, the SDK uses DefaultTransport().
	// Use DefaultTransportConfig() for tuned pool settings.
	Transport *TransportConfig

	// EndpointHostRewrite maps hostnames returned by the server in endpoint
	// URLs to replacement hostnames. This is needed when the server runs
	// inside Docker and returns "host.docker.internal" which is not
	// resolvable from the host machine.
	// Example: map[string]string{"host.docker.internal": "localhost"}
	EndpointHostRewrite map[string]string
}

// RewriteEndpointURL applies EndpointHostRewrite rules to an endpoint URL
// returned by the server. This handles cases like Docker's
// "host.docker.internal" being unreachable from the host machine.
func (c *ConnectionConfig) RewriteEndpointURL(endpointURL string) string {
	for from, to := range c.EndpointHostRewrite {
		endpointURL = strings.ReplaceAll(endpointURL, from, to)
	}
	return endpointURL
}

// GetDomain returns the configured domain, falling back to env var and default.
func (c *ConnectionConfig) GetDomain() string {
	if c.Domain != "" {
		return c.Domain
	}
	if v := os.Getenv("OPEN_SANDBOX_DOMAIN"); v != "" {
		return v
	}
	return DefaultDomain
}

// GetProtocol returns the configured protocol, falling back to env var and default.
func (c *ConnectionConfig) GetProtocol() string {
	if c.Protocol != "" {
		return c.Protocol
	}
	if v := os.Getenv("OPEN_SANDBOX_PROTOCOL"); v != "" {
		return v
	}
	return DefaultProtocol
}

// GetAPIKey returns the configured API key, falling back to env var.
func (c *ConnectionConfig) GetAPIKey() string {
	if c.APIKey != "" {
		return c.APIKey
	}
	return os.Getenv("OPEN_SANDBOX_API_KEY")
}

// GetBaseURL returns the lifecycle API base URL (e.g. "http://localhost:8080").
// Note: this does NOT append /v1.
// lifecycleClient() appends /v1 when creating the lifecycle client.
func (c *ConnectionConfig) GetBaseURL() string {
	domain := c.GetDomain()
	protocol := c.GetProtocol()

	// If domain already has a scheme, use it as-is.
	if strings.HasPrefix(domain, "http://") || strings.HasPrefix(domain, "https://") {
		return strings.TrimRight(domain, "/")
	}

	return fmt.Sprintf("%s://%s", protocol, domain)
}

// GetAuthHeader returns the auth header name for lifecycle requests.
func (c *ConnectionConfig) GetAuthHeader() string {
	if c.AuthHeader != "" {
		return c.AuthHeader
	}
	return "OPEN-SANDBOX-API-KEY"
}

// GetRequestTimeout returns the request timeout, defaulting to DefaultRequestTimeout.
func (c *ConnectionConfig) GetRequestTimeout() time.Duration {
	if c.RequestTimeout > 0 {
		return c.RequestTimeout
	}
	return DefaultRequestTimeout
}

// clientOpts builds the common Option slice from config fields.
func (c *ConnectionConfig) clientOpts(includeAuthHeader bool) []Option {
	var opts []Option
	if includeAuthHeader && c.AuthHeader != "" {
		opts = append(opts, WithAuthHeader(c.AuthHeader))
	}
	if c.HTTPClient != nil {
		opts = append(opts, WithHTTPClient(c.HTTPClient))
	} else if c.Transport != nil {
		opts = append(opts, WithHTTPClient(&http.Client{
			Transport: c.Transport.NewTransport(),
		}))
	}
	if t := c.GetRequestTimeout(); t > 0 {
		opts = append(opts, WithTimeout(t))
	}
	if len(c.Headers) > 0 {
		opts = append(opts, WithHeaders(c.Headers))
	}
	if c.Retry != nil {
		opts = append(opts, WithRetry(*c.Retry))
	}
	return opts
}

// lifecycleClient creates a LifecycleClient from this config.
// Appends the API version prefix (/v1) to the base URL, as required by
// NewLifecycleClient and the OpenSandbox lifecycle API spec.
func (c *ConnectionConfig) lifecycleClient() *LifecycleClient {
	return NewLifecycleClient(c.GetBaseURL()+"/"+APIVersion, c.GetAPIKey(), c.clientOpts(true)...)
}

// execdClient creates an ExecdClient for a resolved endpoint. All headers
// returned by the lifecycle GetEndpoint call (auth tokens, routing hints,
// sticky-session keys, etc.) are forwarded as-is on every subsequent request.
func (c *ConnectionConfig) execdClient(endpointURL string, endpointHeaders map[string]string) *ExecdClient {
	opts := c.clientOpts(true)
	if len(endpointHeaders) > 0 {
		opts = append(opts, WithHeaders(endpointHeaders))
	}
	return NewExecdClient(endpointURL, "", opts...)
}

// egressClient creates an EgressClient for a resolved endpoint. All headers
// returned by the lifecycle GetEndpoint call are forwarded as-is on every
// subsequent request.
func (c *ConnectionConfig) egressClient(endpointURL string, endpointHeaders map[string]string) *EgressClient {
	opts := c.clientOpts(false)
	if len(endpointHeaders) > 0 {
		opts = append(opts, WithHeaders(endpointHeaders))
	}
	return NewEgressClient(endpointURL, "", opts...)
}
