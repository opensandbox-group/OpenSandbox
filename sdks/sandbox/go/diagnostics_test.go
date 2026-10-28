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

package opensandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiagnosticPublicFacades(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("OPEN-SANDBOX-API-KEY") != "key" {
			t.Error("missing API key")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sandboxes/sbx-1/diagnostics/context":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"sandboxId": "sbx-1", "backendType": "docker",
				"backendIdentifiers": map[string]string{"containerId": "container-1"},
			})
		case "/sandboxes/sbx-1/diagnostics/logs", "/sandboxes/sbx-1/diagnostics/events":
			if r.URL.Query().Get("scope") != "all" {
				t.Error("missing scope")
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"sandboxId": "sbx-1", "kind": "logs", "scope": "all", "delivery": "url",
				"contentType": "text/plain", "truncated": true,
				"contentUrl": "https://example.invalid/content", "warnings": []string{"partial"},
			})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	lifecycle := NewLifecycleClient(server.URL, "key")
	manager := &SandboxManager{lifecycle: lifecycle}
	sandbox := &Sandbox{id: "sbx-1", lifecycle: lifecycle}
	ctx := context.Background()
	for _, call := range []func() (*DiagnosticContext, error){
		func() (*DiagnosticContext, error) { return manager.GetDiagnosticContext(ctx, "sbx-1") },
		func() (*DiagnosticContext, error) { return sandbox.GetDiagnosticContext(ctx) },
	} {
		result, err := call()
		if err != nil {
			t.Fatal(err)
		}
		if result.BackendIdentifiers["containerId"] != "container-1" {
			t.Fatalf("unexpected context: %+v", result)
		}
	}
	for _, call := range []func() (*DiagnosticContent, error){
		func() (*DiagnosticContent, error) { return manager.GetDiagnosticLogs(ctx, "sbx-1", "all") },
		func() (*DiagnosticContent, error) { return manager.GetDiagnosticEvents(ctx, "sbx-1", "all") },
		func() (*DiagnosticContent, error) { return sandbox.GetDiagnosticLogs(ctx, "all") },
		func() (*DiagnosticContent, error) { return sandbox.GetDiagnosticEvents(ctx, "all") },
	} {
		result, err := call()
		if err != nil {
			t.Fatal(err)
		}
		if result.ContentURL == nil || !result.Truncated || len(result.Warnings) != 1 {
			t.Fatalf("unexpected descriptor: %+v", result)
		}
	}
	if requests != 6 {
		t.Fatalf("unexpected requests (content URLs must not be fetched): %d", requests)
	}
}

func TestDiagnosticContextPreservesAPIErrors(t *testing.T) {
	for _, status := range []int{403, 404, 501} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]string{"code": "TEST_ERROR", "message": "Unavailable"})
		}))
		client := NewLifecycleClient(server.URL, "key")
		_, err := client.GetDiagnosticContext(context.Background(), "sbx-1")
		server.Close()
		var apiError *APIError
		if !errors.As(err, &apiError) {
			t.Fatalf("expected API error for %d, got %v", status, err)
		}
	}
}
