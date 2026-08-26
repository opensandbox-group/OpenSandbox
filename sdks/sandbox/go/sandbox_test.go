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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSandbox_Close(t *testing.T) {
	sb := &Sandbox{id: "sbx-close"}
	require.NoError(t, sb.Close(), "Close should return nil")
}

func TestSandboxManager_Close(t *testing.T) {
	mgr := &SandboxManager{}
	require.NoError(t, mgr.Close(), "Close should return nil")
}

func TestCreateSandbox_ForwardsLifecycle(t *testing.T) {
	timeout := 300
	var received *SandboxLifecycle
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			var request CreateSandboxRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			received = request.Lifecycle
			jsonResponse(w, http.StatusCreated, SandboxInfo{
				ID:        "sbx-lifecycle",
				Status:    SandboxStatus{State: StateRunning},
				CreatedAt: time.Now().UTC(),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sbx-lifecycle":
			jsonResponse(w, http.StatusOK, SandboxInfo{
				ID:        "sbx-lifecycle",
				Status:    SandboxStatus{State: StateRunning},
				CreatedAt: time.Now().UTC(),
			})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/endpoints/"):
			jsonResponse(w, http.StatusOK, Endpoint{Endpoint: srv.URL})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	_, err := CreateSandbox(context.Background(), ConnectionConfig{
		Domain:         srv.URL,
		DisableMetrics: true,
	}, SandboxCreateOptions{
		Image:           "python:3.12",
		SkipHealthCheck: true,
		Lifecycle: &SandboxLifecycle{
			PreStart: &LifecycleHook{
				Command:        []string{"/opt/hooks/restore.sh"},
				TimeoutSeconds: &timeout,
			},
			Periodic: []PeriodicLifecycleHook{{
				Name:     "checkpoint",
				Schedule: "@hourly",
				Command:  []string{"/opt/hooks/checkpoint.sh"},
			}},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, received)
	require.NotNil(t, received.PreStart)
	require.Equal(t, []string{"/opt/hooks/restore.sh"}, received.PreStart.Command)
	require.NotNil(t, received.PreStart.TimeoutSeconds)
	require.Equal(t, 300, *received.PreStart.TimeoutSeconds)
	require.Len(t, received.Periodic, 1)
	require.Equal(t, "checkpoint", received.Periodic[0].Name)
}

func TestSandbox_Kill(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	sb := &Sandbox{
		id:        "sbx-kill-test",
		lifecycle: NewLifecycleClient(srv.URL, "test-key"),
	}

	require.NoError(t, sb.Kill(context.Background()))
	if gotMethod != http.MethodDelete {
		assert.Fail(t, fmt.Sprintf("method = %q, want DELETE", gotMethod))
	}
	if gotPath != "/sandboxes/sbx-kill-test" {
		assert.Fail(t, fmt.Sprintf("path = %q, want /sandboxes/sbx-kill-test", gotPath))
	}
}

func TestSandbox_GetInfo(t *testing.T) {
	want := SandboxInfo{
		ID:     "sbx-info",
		Status: SandboxStatus{State: StateRunning},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, want)
	}))
	defer srv.Close()

	sb := &Sandbox{
		id:        "sbx-info",
		lifecycle: NewLifecycleClient(srv.URL, "test-key"),
	}

	got, err := sb.GetInfo(context.Background())
	require.NoErrorf(t, err, "GetInfo")
	if got.ID != want.ID {
		assert.Fail(t, fmt.Sprintf("ID = %q, want %q", got.ID, want.ID))
	}
	if got.Status.State != StateRunning {
		assert.Fail(t, fmt.Sprintf("State = %q, want %q", got.Status.State, StateRunning))
	}
}

func TestSandbox_Ping_ExecdNil(t *testing.T) {
	sb := &Sandbox{id: "sbx-no-execd"}
	err := sb.Ping(context.Background())
	require.Error(t, err)
	if !strings.Contains(err.Error(), "execd client not initialized") {
		assert.Fail(t, fmt.Sprintf("error = %q, want contains 'execd client not initialized'", err.Error()))
	}
}

func TestSandbox_Ping_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sb := &Sandbox{
		id:    "sbx-ping-ok",
		execd: NewExecdClient(srv.URL, "tok"),
	}

	require.NoError(t, sb.Ping(context.Background()))
}

func TestSandbox_IsHealthy_ExecdNil(t *testing.T) {
	sb := &Sandbox{id: "sbx-no-execd"}
	if sb.IsHealthy(context.Background()) {
		assert.Fail(t, "IsHealthy should return false when execd is nil")
	}
}

func TestSandbox_IsHealthy_True(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sb := &Sandbox{
		id:    "sbx-healthy",
		execd: NewExecdClient(srv.URL, "tok"),
	}

	if !sb.IsHealthy(context.Background()) {
		assert.Fail(t, "IsHealthy should return true when execd /ping succeeds")
	}
}

func TestSandbox_Renew(t *testing.T) {
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	want := RenewExpirationResponse{
		ExpiresAt: expiresAt,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if !strings.HasSuffix(r.URL.Path, "/renew-expiration") {
			assert.Fail(t, fmt.Sprintf("expected /renew-expiration suffix in path %s", r.URL.Path))
		}
		jsonResponse(w, http.StatusOK, want)
	}))
	defer srv.Close()

	sb := &Sandbox{
		id:        "sbx-renew",
		lifecycle: NewLifecycleClient(srv.URL, "test-key"),
	}

	got, err := sb.Renew(context.Background(), time.Hour)
	require.NoErrorf(t, err, "Renew")
	if got.ExpiresAt.Truncate(time.Second).Equal(expiresAt) {
		return
	}
	assert.Fail(t, fmt.Sprintf("ExpiresAt = %v, want ~%v", got.ExpiresAt, expiresAt))
}

func TestSandbox_Pause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/sandboxes/sbx-pause/pause" {
			assert.Fail(t, fmt.Sprintf("path = %q, want /sandboxes/sbx-pause/pause", r.URL.Path))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	sb := &Sandbox{
		id:        "sbx-pause",
		lifecycle: NewLifecycleClient(srv.URL, "test-key"),
	}

	require.NoError(t, sb.Pause(context.Background()))
}

func TestSandbox_CreateSnapshot(t *testing.T) {
	want := SnapshotInfo{
		ID:        "snap-1",
		SandboxID: "sbx-snap",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/sandboxes/sbx-snap/snapshots" {
			assert.Fail(t, fmt.Sprintf("path = %q, want /sandboxes/sbx-snap/snapshots", r.URL.Path))
		}
		jsonResponse(w, http.StatusCreated, want)
	}))
	defer srv.Close()

	sb := &Sandbox{
		id:        "sbx-snap",
		lifecycle: NewLifecycleClient(srv.URL, "test-key"),
	}

	got, err := sb.CreateSnapshot(context.Background(), CreateSnapshotRequest{})
	require.NoErrorf(t, err, "CreateSnapshot")
	if got.ID != "snap-1" {
		assert.Fail(t, fmt.Sprintf("ID = %q, want snap-1", got.ID))
	}
}

func TestSandbox_GetEndpoint(t *testing.T) {
	want := Endpoint{
		Endpoint: "https://sbx-test.example.com:8080",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/sandboxes/sbx-endpoint/endpoints/8080") {
			assert.Fail(t, fmt.Sprintf("expected path containing /sandboxes/sbx-endpoint/endpoints/8080, got %s", r.URL.Path))
		}
		jsonResponse(w, http.StatusOK, want)
	}))
	defer srv.Close()

	sb := &Sandbox{
		id:        "sbx-endpoint",
		lifecycle: NewLifecycleClient(srv.URL, "test-key"),
		config:    &ConnectionConfig{},
	}

	got, err := sb.GetEndpoint(context.Background(), 8080)
	require.NoErrorf(t, err, "GetEndpoint")
	if got.Endpoint != want.Endpoint {
		assert.Fail(t, fmt.Sprintf("Endpoint = %q, want %q", got.Endpoint, want.Endpoint))
	}
}
