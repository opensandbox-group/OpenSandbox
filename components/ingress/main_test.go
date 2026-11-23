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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
