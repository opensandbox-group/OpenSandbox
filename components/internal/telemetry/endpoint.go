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

package telemetry

import (
	"net/url"
	"os"
	"strings"
)

// OTLPEndpointHostPort returns the host and port of the configured OTLP
// endpoint. Endpoint precedence matches the exporters:
// OTEL_EXPORTER_OTLP_METRICS_ENDPOINT, then OTEL_EXPORTER_OTLP_ENDPOINT.
// The value must be a URL (scheme://host[:port][/path]), matching the
// OTLP exporter env-var form; bare host:port or host values are invalid
// because the exporter parses them as opaque URLs with an empty host.
// Without a port, gRPC uses its resolver default (443); HTTP uses the scheme default.
// Domain hosts are returned without the trailing dot, matching DNS policy
// normalization. ok is false when no endpoint is configured or it cannot
// be parsed.
func OTLPEndpointHostPort() (host, port string, ok bool) {
	raw := otlpEndpointFromEnv()
	if raw == "" {
		return "", "", false
	}
	host, port, ok = parseOTLPEndpoint(raw)
	if ok && otlpMetricsProtocol() == otlpGRPCProtocol {
		u, _ := url.Parse(strings.TrimSpace(raw))
		if u.Port() == "" {
			port = "443" // Match the gRPC DNS resolver, including plaintext URLs.
		}
	}
	return host, port, ok
}

// OTLPEndpointEnvSet reports whether any OTEL endpoint env var is non-blank,
// regardless of whether it parses. Callers that also use
// OTLPEndpointFallbackHostPort need this to distinguish "unset" from
// "configured but invalid": the exporter never falls back to the node IP once
// an endpoint env var is set, so neither should the auto-allow logic.
func OTLPEndpointEnvSet() bool {
	return otlpEndpointFromEnv() != ""
}

// OTLPEndpointFallbackHostPort returns the exporter fallback destination used
// only when no OTEL endpoint env var is set: the resolved node IP
// (HOST_IP, then /etc/hostinfo) on port 4317 for gRPC or 4318 for HTTP. ok is
// false when no node IP can be resolved.
func OTLPEndpointFallbackHostPort() (host, port string, ok bool) {
	ip, ok := resolveNodeIP()
	if !ok {
		return "", "", false
	}
	port = otlpHTTPPort
	if otlpMetricsProtocol() == otlpGRPCProtocol {
		port = otlpGRPCPort
	}
	return ip, port, true
}

// otlpMetricsProtocol applies signal-specific precedence before normalization.
// Unsupported values are reported once by Init and use the HTTP fallback.
func otlpMetricsProtocol() string {
	protocol := strings.TrimSpace(os.Getenv(envOTLPMetricsProtocol))
	if protocol == "" {
		protocol = strings.TrimSpace(os.Getenv(envOTLPProtocol))
	}
	if protocol == "" {
		return otlpHTTPProtocol
	}
	return strings.ToLower(protocol)
}

func parseOTLPEndpoint(raw string) (host, port string, ok bool) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		return "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	host = strings.TrimRight(strings.TrimSpace(u.Hostname()), ".")
	if host == "" {
		return "", "", false
	}
	port = u.Port()
	if port == "" {
		port = defaultPortForScheme(u.Scheme)
	}
	return host, port, true
}

func defaultPortForScheme(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}
