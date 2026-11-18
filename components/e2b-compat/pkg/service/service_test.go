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

package service_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/e2b-compat/pkg/service"
)

func TestHealthzGET(t *testing.T) {
	rec := httptest.NewRecorder()
	service.NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok\n" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "ok\n")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
}

// A recorder keeps the written body, so HEAD is checked through a real
// http.Server, which drops it.
func TestHealthzHEADHasNoBody(t *testing.T) {
	srv := httptest.NewServer(service.NewHandler())
	defer srv.Close()
	resp, err := http.Head(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestHealthzOtherMethodsGet405(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		service.NewHandler().ServeHTTP(rec, httptest.NewRequest(method, "/healthz", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /healthz: status = %d, want 405", method, rec.Code)
		}
	}
}

// No E2B route is registered: every path other than the health check is 404,
// at the root and under the compatibility prefix.
func TestOnlyHealthPathIsServed(t *testing.T) {
	handler := service.NewHandler()
	for _, target := range []string{
		"/",
		"/v2/sandboxes",
		"/sandboxes",
		"/compat/e2b",
		"/compat/e2b/v2/sandboxes",
		"/compat/e2b/sandboxes",
		"/healthz/",
		"/healthz/extra",
		"/readyz",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: status = %d, want 404", method, target, rec.Code)
			}
		}
	}
}

func TestServeLifecycle(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Serve(ctx, ln, service.NewHandler(), service.ShutdownTimeout) }()

	resp, err := httpGet(addr)
	if err != nil {
		t.Fatalf("health check over real HTTP: %v", err)
	}
	if resp != "ok\n" {
		t.Errorf("health body = %q", resp)
	}

	// A listener failure surfaces as an error from Serve.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve after cancel returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop within 10s of cancellation")
	}
}

func TestServeReturnsListenerError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing the listener makes Serve return a non-nil error.
	errCh := make(chan error, 1)
	go func() {
		errCh <- service.Serve(context.Background(), ln, service.NewHandler(), service.ShutdownTimeout)
	}()
	select {
	case err := <-errCh:
		if err == nil {
			t.Error("Serve on a closed listener returned nil error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve on a closed listener did not return")
	}
}

func httpGet(addr string) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		// A connection refused means the server is not accepting yet.
		return "", err
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n]), nil
}
