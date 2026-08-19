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

package telemetry

import "testing"

func TestParseOTLPEndpoint(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		host string
		port string
		ok   bool
	}{
		{name: "empty", raw: "", ok: false},
		{name: "whitespace", raw: "   ", ok: false},
		{name: "url with port and path", raw: "https://collector.example:4318/v1/metrics", host: "collector.example", port: "4318", ok: true},
		{name: "url without port", raw: "https://collector.example/v1/metrics", host: "collector.example", port: "443", ok: true},
		{name: "http url without port", raw: "http://collector.example/v1/metrics", host: "collector.example", port: "80", ok: true},
		{name: "ip url", raw: "http://10.0.0.1:4317", host: "10.0.0.1", port: "4317", ok: true},
		{name: "ipv6 url", raw: "http://[::1]:4318/v1/metrics", host: "::1", port: "4318", ok: true},
		{name: "host port without scheme", raw: "collector.example:4318", ok: false},
		{name: "ip port without scheme", raw: "10.0.0.1:4318", ok: false},
		{name: "bare host", raw: "collector.example", ok: false},
		{name: "bare ip", raw: "10.0.0.1", ok: false},
		{name: "fqdn url trailing dot", raw: "http://otel-collector.ns.svc.cluster.local.:4318", host: "otel-collector.ns.svc.cluster.local", port: "4318", ok: true},
		{name: "fqdn trailing dot without scheme", raw: "otel-collector.ns.svc.cluster.local.:4318", ok: false},
		{name: "bare fqdn trailing dot", raw: "collector.example.", ok: false},
		{name: "scheme only", raw: "http://", ok: false},
		{name: "malformed url", raw: "https://:443", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port, ok := parseOTLPEndpoint(tc.raw)
			if ok != tc.ok {
				t.Fatalf("parseOTLPEndpoint(%q) ok=%v, want %v", tc.raw, ok, tc.ok)
			}
			if host != tc.host || port != tc.port {
				t.Fatalf("parseOTLPEndpoint(%q) = (%q, %q), want (%q, %q)", tc.raw, host, port, tc.host, tc.port)
			}
		})
	}
}

func TestOTLPEndpointHostPortPrecedence(t *testing.T) {
	t.Setenv(envOTLPMetricsEndpoint, "")
	t.Setenv(envOTLPEndpoint, "")
	host, _, ok := OTLPEndpointHostPort()
	if ok {
		t.Fatal("expected no endpoint when both env vars are unset")
	}
	if host != "" {
		t.Fatalf("expected empty host, got %q", host)
	}

	t.Setenv(envOTLPEndpoint, "https://fallback.example:4318")
	host, port, ok := OTLPEndpointHostPort()
	if !ok || host != "fallback.example" || port != "4318" {
		t.Fatalf("fallback endpoint parsed as (%q, %q, %v)", host, port, ok)
	}

	t.Setenv(envOTLPMetricsEndpoint, "https://primary.example:4317/v1/metrics")
	host, port, ok = OTLPEndpointHostPort()
	if !ok || host != "primary.example" || port != "4317" {
		t.Fatalf("metrics endpoint should win; parsed as (%q, %q, %v)", host, port, ok)
	}

	t.Setenv(envOTLPMetricsEndpoint, "   ")
	host, port, ok = OTLPEndpointHostPort()
	if !ok || host != "fallback.example" || port != "4318" {
		t.Fatalf("blank metrics endpoint should fall back; parsed as (%q, %q, %v)", host, port, ok)
	}
}

func TestOTLPEndpointEnvSet(t *testing.T) {
	t.Setenv(envOTLPMetricsEndpoint, "")
	t.Setenv(envOTLPEndpoint, "")
	if OTLPEndpointEnvSet() {
		t.Fatal("expected env unset")
	}

	t.Setenv(envOTLPEndpoint, "http://")
	if !OTLPEndpointEnvSet() {
		t.Fatal("expected env set even when unparseable")
	}

	t.Setenv(envOTLPEndpoint, "")
	t.Setenv(envOTLPMetricsEndpoint, "https://collector.example:4318")
	if !OTLPEndpointEnvSet() {
		t.Fatal("expected metrics endpoint env set")
	}
}

func TestOTLPEndpointFallbackHostPort(t *testing.T) {
	t.Setenv(envHostIP, "10.0.0.9")
	host, port, ok := OTLPEndpointFallbackHostPort()
	if !ok || host != "10.0.0.9" || port != otlpHTTPPort {
		t.Fatalf("fallback from HOST_IP parsed as (%q, %q, %v)", host, port, ok)
	}

	t.Setenv(envHostIP, "   ")
	host, _, ok = OTLPEndpointFallbackHostPort()
	if ok {
		t.Fatalf("expected no fallback without a resolvable node IP, got %q", host)
	}
}
