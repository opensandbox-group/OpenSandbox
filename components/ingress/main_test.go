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

package main

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
)

func TestFastSandboxStartupAfterTelemetryFailure(t *testing.T) {
	if os.Getenv("OPENSANDBOX_TEST_MAIN") == "1" {
		os.Args = append([]string{os.Args[0]}, os.Args[3:]...)
		main()
		return
	}

	fastPathListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fastPathServer := grpc.NewServer()
	go func() { _ = fastPathServer.Serve(fastPathListener) }()
	t.Cleanup(fastPathServer.Stop)

	// An occupied HTTP port stops main only after provider initialization succeeds.
	httpListener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = httpListener.Close() })
	_, port, err := net.SplitHostPort(httpListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	certServer := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(certServer.Close)
	certificate := filepath.Join(t.TempDir(), "otlp-ca.pem")
	if err := os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certServer.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENSANDBOX_TEST_MAIN", "1")
	t.Setenv("OTEL_SDK_DISABLED", "false")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE", certificate)

	for _, test := range []struct {
		name, accessMode, wantError string
	}{
		{name: "valid provider", accessMode: "direct-fastlet-proxy", wantError: "Error starting http server:"},
		{name: "invalid provider", accessMode: "invalid", wantError: "Failed to create sandbox provider: unsupported FastPath access mode"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestFastSandboxStartupAfterTelemetryFailure$", "--",
				"--provider-type=fast-sandbox", "--fastpath-endpoint="+fastPathListener.Addr().String(),
				"--fast-sandbox-namespace=test", "--mode=header", "--wake-enabled=false", "--fastpath-auth-enabled=false",
				"--fastpath-access-mode="+test.accessMode, "--port="+port)
			output, err := command.CombinedOutput()
			if ctx.Err() != nil || err == nil {
				t.Fatalf("startup did not reach its expected error: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "OpenTelemetry metrics disabled (continuing without OTLP): insecure HTTP endpoint cannot use TLS client configuration") {
				t.Fatalf("telemetry initialization did not fail as expected:\n%s", output)
			}
			if !strings.Contains(string(output), test.wantError) {
				t.Fatalf("startup did not reach %q:\n%s", test.wantError, output)
			}
		})
	}
}

func TestNewRedisClient(t *testing.T) {
	for _, test := range []struct {
		name, dsn, address string
		db                 int
		invalid            bool
	}{
		{name: "redis", dsn: "redis://localhost:6379/2", address: "localhost:6379", db: 2},
		{name: "TLS", dsn: "rediss://localhost:6380/0", address: "localhost:6380"},
		{name: "invalid scheme", dsn: "http://localhost:6379", invalid: true},
		{name: "invalid database", dsn: "redis://localhost:6379/not-a-db", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := newRedisClient(test.dsn)
			if test.invalid {
				if err == nil || client != nil {
					t.Fatalf("newRedisClient() = %v, %v; want nil client and error", client, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			if opts := client.Options(); opts.Addr != test.address || opts.DB != test.db {
				t.Fatalf("Redis options = %+v, want address %s and database %d", opts, test.address, test.db)
			}
		})
	}
}

func TestValidateHTTPConnectionPool(t *testing.T) {
	for _, test := range []struct {
		name            string
		maxIdleConns    int
		maxIdlePerHost  int
		idleTimeout     time.Duration
		wantInvalidFlag string
	}{
		{name: "defaults", maxIdleConns: 100, maxIdlePerHost: 2, idleTimeout: 90 * time.Second},
		{name: "custom", maxIdleConns: 1000, maxIdlePerHost: 8, idleTimeout: 2 * time.Minute},
		{name: "zero"},
		{name: "negative total", maxIdleConns: -1, wantInvalidFlag: "--http-max-idle-conns"},
		{name: "negative per host", maxIdlePerHost: -1, wantInvalidFlag: "--http-max-idle-conns-per-host"},
		{name: "negative timeout", idleTimeout: -time.Nanosecond, wantInvalidFlag: "--http-idle-conn-timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateHTTPConnectionPool(test.maxIdleConns, test.maxIdlePerHost, test.idleTimeout)
			if test.wantInvalidFlag == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantInvalidFlag) {
				t.Fatalf("validateHTTPConnectionPool() = %v, want error naming %s", err, test.wantInvalidFlag)
			}
		})
	}
}

func TestValidateFastPathRouting(t *testing.T) {
	for _, test := range []struct {
		name, endpoint, namespace, mode, keys string
		valid                                 bool
	}{
		{name: "legacy", mode: "header", valid: true},
		{name: "wildcard without keys", endpoint: "fastpath:9090", namespace: "tenant-a", mode: "header", valid: true},
		{name: "signed header", endpoint: "fastpath:9090", mode: "header", keys: "k=c2VjcmV0", valid: true},
		{name: "signed URI restricted namespace", endpoint: "fastpath:9090", namespace: "tenant-a", mode: "uri", keys: "k=c2VjcmV0", valid: true},
		{name: "missing FastPath", namespace: "tenant-a", mode: "header"},
		{name: "missing credentials and namespace", endpoint: "fastpath:9090", mode: "header"},
		{name: "unsigned URI", endpoint: "fastpath:9090", namespace: "tenant-a", mode: "uri"},
		{name: "blank keys", endpoint: "fastpath:9090", mode: "header", keys: " "},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateFastPathRouting(test.endpoint, test.namespace, test.mode, test.keys)
			if (err == nil) != test.valid {
				t.Fatalf("validateFastPathRouting() = %v, valid = %v", err, test.valid)
			}
		})
	}
}

func pathRecorder(name string, got *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = name + " " + r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusTeapot)
	})
}

func TestIngressHandlerSendsUncleanPathsToTheProxy(t *testing.T) {
	var got string
	handler := newIngressHandler(pathRecorder("proxy", &got), pathRecorder("readiness", &got))

	for _, target := range []string{
		"/api//v1/items",
		"/sbx/8080/proxy/http://example.com/x",
		"/sbx/8080/a/../b",
		"/sbx/8080/./c",
	} {
		got = ""
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, nil))
		if rec.Code != http.StatusTeapot || got != "proxy POST "+target {
			t.Errorf("POST %s: status %d, handled as %q; want it proxied unchanged", target, rec.Code, got)
		}
	}
}

func TestIngressHandlerServesStatusPaths(t *testing.T) {
	var got string
	handler := newIngressHandler(pathRecorder("proxy", &got), pathRecorder("readiness", &got))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status.ok/network-readiness", nil))
	if got != "readiness GET /status.ok/network-readiness" {
		t.Errorf("network readiness handled as %q", got)
	}

	got = ""
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status.ok", nil))
	if rec.Code != http.StatusOK || got != "" {
		t.Errorf("/status.ok: status %d, handled as %q; want the health check", rec.Code, got)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status.ok/other", nil))
	if got != "proxy GET /status.ok/other" {
		t.Errorf("/status.ok/other handled as %q; want it proxied", got)
	}

	// An encoded slash is part of a sandbox path, not a status endpoint.
	got = ""
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status.ok%2Fnetwork-readiness", nil))
	if got != "proxy GET /status.ok/network-readiness" {
		t.Errorf("/status.ok%%2Fnetwork-readiness handled as %q; want it proxied", got)
	}
}
