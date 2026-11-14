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
	"testing"
)

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
