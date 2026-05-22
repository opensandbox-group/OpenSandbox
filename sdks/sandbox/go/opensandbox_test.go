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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// newLifecycleServer creates an httptest.Server and a LifecycleClient pointing at it.
func newLifecycleServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *LifecycleClient) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewLifecycleClient(srv.URL, "test-api-key")
	return srv, client
}

// newEgressServer creates an httptest.Server and an EgressClient pointing at it.
func newEgressServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *EgressClient) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewEgressClient(srv.URL, "test-egress-token")
	return srv, client
}

// newExecdServer creates an httptest.Server and an ExecdClient pointing at it.
func newExecdServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *ExecdClient) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewExecdClient(srv.URL, "test-execd-token")
	return srv, client
}

func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func TestCreateSandbox(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	want := SandboxInfo{
		ID: "sbx-123",
		Status: SandboxStatus{
			State: StatePending,
		},
		CreatedAt: now,
	}

	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/sandboxes" {
			assert.Fail(t, fmt.Sprintf("expected /sandboxes, got %s", r.URL.Path))
		}

		var req CreateSandboxRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Image == nil || req.Image.URI != "python:3.12" {
			assert.Fail(t, fmt.Sprintf("expected image python:3.12, got %+v", req.Image))
		}

		jsonResponse(w, http.StatusCreated, want)
	})

	got, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		Image:      &ImageSpec{URI: "python:3.12"},
		Entrypoint: []string{"/bin/sh"},
		ResourceLimits: ResourceLimits{
			"cpu":    "500m",
			"memory": "512Mi",
		},
	})
	require.NoErrorf(t, err, "CreateSandbox")
	if got.ID != want.ID {
		assert.Fail(t, fmt.Sprintf("ID = %q, want %q", got.ID, want.ID))
	}
	if got.Status.State != StatePending {
		assert.Fail(t, fmt.Sprintf("State = %q, want %q", got.Status.State, StatePending))
	}
}

func TestCreateSandbox_ImageAuth(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req CreateSandboxRequest
		json.NewDecoder(r.Body).Decode(&req)

		if req.Image == nil || req.Image.Auth == nil {
			require.FailNow(t, "expected ImageAuth to be set")
		}
		if req.Image.Auth.Username != "user" {
			assert.Fail(t, fmt.Sprintf("Username = %q, want %q", req.Image.Auth.Username, "user"))
		}
		if req.Image.Auth.Password != "pass" {
			assert.Fail(t, fmt.Sprintf("Password = %q, want %q", req.Image.Auth.Password, "pass"))
		}

		jsonResponse(w, http.StatusCreated, SandboxInfo{
			ID:        "sbx-auth",
			Status:    SandboxStatus{State: StatePending},
			CreatedAt: time.Now().UTC().Truncate(time.Second),
		})
	})

	_, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		Image: &ImageSpec{
			URI:  "registry.example.com/private:latest",
			Auth: &ImageAuth{Username: "user", Password: "pass"},
		},
		Entrypoint:     []string{"/bin/sh"},
		ResourceLimits: ResourceLimits{"cpu": "500m"},
	})
	require.NoErrorf(t, err, "CreateSandbox with ImageAuth")
}

func TestPatchSandboxMetadata(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	value := "platform"

	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			assert.Fail(t, fmt.Sprintf("expected PATCH, got %s", r.Method))
		}
		if r.URL.Path != "/sandboxes/sbx-123/metadata" {
			assert.Fail(t, fmt.Sprintf("expected /sandboxes/sbx-123/metadata, got %s", r.URL.Path))
		}

		var body map[string]*string
		require.NoErrorf(t, json.NewDecoder(r.Body).Decode(&body), "decode metadata patch")
		require.NotNil(t, body["team"])
		if *body["team"] != "platform" {
			assert.Fail(t, fmt.Sprintf("team = %q, want platform", *body["team"]))
		}
		_, hasOld := body["old"]
		require.True(t, hasOld, "old metadata key should be present")
		if body["old"] != nil {
			assert.Fail(t, "old metadata key should be null")
		}

		jsonResponse(w, http.StatusOK, SandboxInfo{
			ID:         "sbx-123",
			Status:     SandboxStatus{State: StateRunning},
			Metadata:   map[string]string{"team": "platform"},
			Entrypoint: []string{"/bin/sh"},
			CreatedAt:  now,
		})
	})

	got, err := client.PatchSandboxMetadata(context.Background(), "sbx-123", MetadataPatch{
		"team": &value,
		"old":  nil,
	})
	require.NoErrorf(t, err, "PatchSandboxMetadata")
	if got.Metadata["team"] != "platform" {
		assert.Fail(t, fmt.Sprintf("team = %q, want platform", got.Metadata["team"]))
	}
}

func TestCreateSandbox_SecureAccess(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req CreateSandboxRequest
		json.NewDecoder(r.Body).Decode(&req)

		if !req.SecureAccess {
			assert.Fail(t, "expected SecureAccess to be true")
		}

		jsonResponse(w, http.StatusCreated, SandboxInfo{
			ID:        "sbx-secure",
			Status:    SandboxStatus{State: StatePending},
			CreatedAt: time.Now().UTC().Truncate(time.Second),
		})
	})

	_, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		Image:          &ImageSpec{URI: "python:3.12"},
		Entrypoint:     []string{"/bin/sh"},
		ResourceLimits: ResourceLimits{"cpu": "500m"},
		SecureAccess:   true,
	})
	require.NoErrorf(t, err, "CreateSandbox with SecureAccess")
}

func TestCreateSandbox_ManualCleanup(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		json.Unmarshal(body, &raw)

		if _, exists := raw["timeout"]; exists {
			assert.Fail(t, "expected timeout to be omitted from request when ManualCleanup is true")
		}

		jsonResponse(w, http.StatusCreated, SandboxInfo{
			ID:        "sbx-manual",
			Status:    SandboxStatus{State: StatePending},
			CreatedAt: time.Now().UTC().Truncate(time.Second),
		})
	})

	_, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		Image:          &ImageSpec{URI: "python:3.12"},
		Entrypoint:     []string{"/bin/sh"},
		ResourceLimits: ResourceLimits{"cpu": "500m"},
		// Timeout is nil — simulates ManualCleanup (no timeout sent)
	})
	require.NoErrorf(t, err, "CreateSandbox with ManualCleanup")
}

func TestCreateSandbox_FromSnapshot(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req CreateSandboxRequest
		json.NewDecoder(r.Body).Decode(&req)

		if req.SnapshotID != "snap-123" {
			assert.Fail(t, fmt.Sprintf("SnapshotID = %q, want %q", req.SnapshotID, "snap-123"))
		}
		if req.Image != nil {
			assert.Fail(t, "expected image to be omitted for snapshot restore")
		}
		if len(req.Entrypoint) != 0 {
			assert.Fail(t, "expected entrypoint to be omitted for snapshot restore")
		}

		jsonResponse(w, http.StatusCreated, SandboxInfo{
			ID:         "sbx-snapshot",
			SnapshotID: "snap-123",
			Status:     SandboxStatus{State: StatePending},
			CreatedAt:  time.Now().UTC().Truncate(time.Second),
			Entrypoint: []string{"/bin/sh"},
		})
	})

	_, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		SnapshotID:     "snap-123",
		ResourceLimits: ResourceLimits{"cpu": "500m"},
	})
	require.NoErrorf(t, err, "CreateSandbox from snapshot")
}

func TestCreateSandbox_Platform(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req CreateSandboxRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			assert.Fail(t, fmt.Sprintf("decode request: %v", err))
			return
		}
		require.NotNil(t, req.Platform, "expected Platform to be sent in the request")
		require.Equal(t, OSWindows, req.Platform.OS, "Platform.OS")
		require.Equal(t, ArchAMD64, req.Platform.Arch, "Platform.Arch")

		jsonResponse(w, http.StatusCreated, SandboxInfo{
			ID:        "sbx-windows",
			Status:    SandboxStatus{State: StatePending},
			Platform:  &PlatformSpec{OS: OSWindows, Arch: ArchAMD64},
			CreatedAt: time.Now().UTC().Truncate(time.Second),
		})
	})

	info, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		Image:          &ImageSpec{URI: "dockurr/windows:latest"},
		Entrypoint:     []string{"cmd", "/c", "echo hi"},
		ResourceLimits: ResourceLimits{"cpu": "2", "memory": "4G", "disk": "64G"},
		Platform:       &PlatformSpec{OS: OSWindows, Arch: ArchAMD64},
	})
	require.NoErrorf(t, err, "CreateSandbox with Platform")
	require.NotNil(t, info.Platform, "response should echo Platform")
	require.Equal(t, OSWindows, info.Platform.OS, "echoed Platform.OS")
	require.Equal(t, ArchAMD64, info.Platform.Arch, "echoed Platform.Arch")
}

func TestCreateSandbox_PlatformOmittedWhenNil(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			assert.Fail(t, fmt.Sprintf("read request body: %v", err))
			return
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			assert.Fail(t, fmt.Sprintf("unmarshal request body: %v", err))
			return
		}
		if _, present := raw["platform"]; present {
			assert.Fail(t, "platform should be omitted from JSON when nil")
		}

		jsonResponse(w, http.StatusCreated, SandboxInfo{
			ID:        "sbx-no-platform",
			Status:    SandboxStatus{State: StatePending},
			CreatedAt: time.Now().UTC().Truncate(time.Second),
		})
	})

	_, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		Image:          &ImageSpec{URI: "python:3.12"},
		Entrypoint:     []string{"/bin/sh"},
		ResourceLimits: ResourceLimits{"cpu": "500m"},
	})
	require.NoErrorf(t, err, "CreateSandbox without Platform")
}

func TestGetSandbox(t *testing.T) {
	want := SandboxInfo{
		ID: "sbx-456",
		Status: SandboxStatus{
			State: StateRunning,
		},
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}

	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if r.URL.Path != "/sandboxes/sbx-456" {
			assert.Fail(t, fmt.Sprintf("expected /sandboxes/sbx-456, got %s", r.URL.Path))
		}
		jsonResponse(w, http.StatusOK, want)
	})

	got, err := client.GetSandbox(context.Background(), "sbx-456")
	require.NoErrorf(t, err, "GetSandbox")
	if got.ID != want.ID {
		assert.Fail(t, fmt.Sprintf("ID = %q, want %q", got.ID, want.ID))
	}
	if got.Status.State != StateRunning {
		assert.Fail(t, fmt.Sprintf("State = %q, want %q", got.Status.State, StateRunning))
	}
}

func TestListSandboxes(t *testing.T) {
	want := ListSandboxesResponse{
		Items: []SandboxInfo{
			{ID: "sbx-1", Status: SandboxStatus{State: StateRunning}, CreatedAt: time.Now().UTC().Truncate(time.Second)},
			{ID: "sbx-2", Status: SandboxStatus{State: StatePaused}, CreatedAt: time.Now().UTC().Truncate(time.Second)},
		},
		Pagination: PaginationInfo{
			Page:        1,
			PageSize:    20,
			TotalItems:  2,
			TotalPages:  1,
			HasNextPage: false,
		},
	}

	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if !strings.HasPrefix(r.URL.Path, "/sandboxes") {
			assert.Fail(t, fmt.Sprintf("expected /sandboxes prefix, got %s", r.URL.Path))
		}
		if r.URL.Query().Get("page") != "1" {
			assert.Fail(t, fmt.Sprintf("expected page=1, got %s", r.URL.Query().Get("page")))
		}
		if r.URL.Query().Get("pageSize") != "20" {
			assert.Fail(t, fmt.Sprintf("expected pageSize=20, got %s", r.URL.Query().Get("pageSize")))
		}
		jsonResponse(w, http.StatusOK, want)
	})

	got, err := client.ListSandboxes(context.Background(), ListOptions{
		Page:     1,
		PageSize: 20,
	})
	require.NoErrorf(t, err, "ListSandboxes")
	require.Len(t, got.Items, 2)
	if got.Pagination.TotalItems != 2 {
		assert.Fail(t, fmt.Sprintf("TotalItems = %d, want 2", got.Pagination.TotalItems))
	}
}

func TestDeleteSandbox(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			assert.Fail(t, fmt.Sprintf("expected DELETE, got %s", r.Method))
		}
		if r.URL.Path != "/sandboxes/sbx-789" {
			assert.Fail(t, fmt.Sprintf("expected /sandboxes/sbx-789, got %s", r.URL.Path))
		}
		w.WriteHeader(http.StatusNoContent)
	})

	err := client.DeleteSandbox(context.Background(), "sbx-789")
	require.NoErrorf(t, err, "DeleteSandbox")
}

func TestSnapshotLifecycle(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/sbx-1/snapshots":
			var req CreateSnapshotRequest
			json.NewDecoder(r.Body).Decode(&req)
			if req.Name != "before-upgrade" {
				assert.Fail(t, fmt.Sprintf("Name = %q, want %q", req.Name, "before-upgrade"))
			}
			jsonResponse(w, http.StatusAccepted, SnapshotInfo{
				ID:        "snap-1",
				SandboxID: "sbx-1",
				Name:      "before-upgrade",
				Status:    SnapshotStatus{State: SnapshotStateCreating},
				CreatedAt: now,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/snapshots/snap-1":
			jsonResponse(w, http.StatusOK, SnapshotInfo{
				ID:        "snap-1",
				SandboxID: "sbx-1",
				Status:    SnapshotStatus{State: SnapshotStateReady},
				CreatedAt: now,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/snapshots":
			if r.URL.Query().Get("sandboxId") != "sbx-1" {
				assert.Fail(t, fmt.Sprintf("sandboxId = %q, want %q", r.URL.Query().Get("sandboxId"), "sbx-1"))
			}
			jsonResponse(w, http.StatusOK, ListSnapshotsResponse{
				Items: []SnapshotInfo{{
					ID:        "snap-1",
					SandboxID: "sbx-1",
					Status:    SnapshotStatus{State: SnapshotStateReady},
					CreatedAt: now,
				}},
				Pagination: PaginationInfo{Page: 1, PageSize: 10, TotalItems: 1, TotalPages: 1},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/snapshots/snap-1":
			w.WriteHeader(http.StatusNoContent)
		default:
			assert.Fail(t, fmt.Sprintf("unexpected request: %s %s", r.Method, r.URL.Path))
		}
	})

	created, err := client.CreateSnapshot(context.Background(), "sbx-1", CreateSnapshotRequest{Name: "before-upgrade"})
	require.NoErrorf(t, err, "CreateSnapshot")
	require.Equal(t, "snap-1", created.ID)

	got, err := client.GetSnapshot(context.Background(), "snap-1")
	require.NoErrorf(t, err, "GetSnapshot")
	require.Equal(t, SnapshotStateReady, got.Status.State)

	listed, err := client.ListSnapshots(context.Background(), ListSnapshotsOptions{
		SandboxID: "sbx-1",
		States:    []SnapshotState{SnapshotStateReady},
		Page:      1,
		PageSize:  10,
	})
	require.NoErrorf(t, err, "ListSnapshots")
	require.Len(t, listed.Items, 1)

	err = client.DeleteSnapshot(context.Background(), "snap-1")
	require.NoErrorf(t, err, "DeleteSnapshot")
}

func TestResumeSandbox(t *testing.T) {
	var resumed bool
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/sandboxes/sbx-paused/resume" {
			resumed = true
			w.WriteHeader(http.StatusAccepted)
			return
		}
		assert.Fail(t, fmt.Sprintf("unexpected request: %s %s", r.Method, r.URL.Path))
		w.WriteHeader(http.StatusNotFound)
	})

	err := client.ResumeSandbox(context.Background(), "sbx-paused")
	require.NoErrorf(t, err, "ResumeSandbox")
	if !resumed {
		assert.Fail(t, "expected resume endpoint to be called")
	}
}

func TestSandbox_Resume(t *testing.T) {
	var resumeCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resume"):
			resumeCalled = true
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/endpoints/"):
			jsonResponse(w, http.StatusOK, Endpoint{
				Endpoint: "http://execd.test:8080",
				Headers:  map[string]string{"X-EXECD-ACCESS-TOKEN": "tok"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	config := ConnectionConfig{Domain: srv.URL}
	sb := &Sandbox{
		id:     "sbx-resume-test",
		config: &config,
	}

	got, err := sb.Resume(context.Background())
	require.NoErrorf(t, err, "Resume")
	if !resumeCalled {
		assert.Fail(t, "expected resume endpoint to be called")
	}
	if got.ID() != "sbx-resume-test" {
		assert.Fail(t, fmt.Sprintf("ID = %q, want %q", got.ID(), "sbx-resume-test"))
	}
}

func TestPauseSandbox(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/sandboxes/sbx-pause/pause" {
			assert.Fail(t, fmt.Sprintf("expected /sandboxes/sbx-pause/pause, got %s", r.URL.Path))
		}
		w.WriteHeader(http.StatusAccepted)
	})

	err := client.PauseSandbox(context.Background(), "sbx-pause")
	require.NoErrorf(t, err, "PauseSandbox")
}

func TestAPIError(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusNotFound, ErrorResponse{
			Code:    "SANDBOX_NOT_FOUND",
			Message: "sandbox sbx-missing does not exist",
		})
	})

	_, err := client.GetSandbox(context.Background(), "sbx-missing")
	require.Error(t, err)

	apiErr, ok := err.(*APIError)
	require.True(t, ok, "expected *APIError, got %T", err)
	if apiErr.StatusCode != http.StatusNotFound {
		assert.Fail(t, fmt.Sprintf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusNotFound))
	}
	if apiErr.Response.Code != "SANDBOX_NOT_FOUND" {
		assert.Fail(t, fmt.Sprintf("Code = %q, want %q", apiErr.Response.Code, "SANDBOX_NOT_FOUND"))
	}
	if !strings.Contains(apiErr.Error(), "SANDBOX_NOT_FOUND") {
		assert.Fail(t, fmt.Sprintf("Error() = %q, expected to contain SANDBOX_NOT_FOUND", apiErr.Error()))
	}
}

func TestGetPolicy(t *testing.T) {
	want := PolicyStatusResponse{
		Status:          "active",
		Mode:            "enforce",
		EnforcementMode: "strict",
		Policy: &NetworkPolicy{
			DefaultAction: "deny",
			Egress: []NetworkRule{
				{Action: "allow", Target: "api.example.com"},
			},
		},
	}

	_, client := newEgressServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if r.URL.Path != "/policy" {
			assert.Fail(t, fmt.Sprintf("expected /policy, got %s", r.URL.Path))
		}
		jsonResponse(w, http.StatusOK, want)
	})

	got, err := client.GetPolicy(context.Background())
	require.NoErrorf(t, err, "GetPolicy")
	if got.Status != "active" {
		assert.Fail(t, fmt.Sprintf("Status = %q, want %q", got.Status, "active"))
	}
	if got.Policy == nil || len(got.Policy.Egress) != 1 {
		require.FailNow(t, "expected 1 egress rule")
	}
	if got.Policy.Egress[0].Target != "api.example.com" {
		assert.Fail(t, fmt.Sprintf("Target = %q, want %q", got.Policy.Egress[0].Target, "api.example.com"))
	}
}

func TestPatchPolicy(t *testing.T) {
	want := PolicyStatusResponse{
		Status: "active",
		Mode:   "enforce",
		Policy: &NetworkPolicy{
			DefaultAction: "deny",
			Egress: []NetworkRule{
				{Action: "allow", Target: "api.example.com"},
				{Action: "allow", Target: "cdn.example.com"},
			},
		},
	}

	_, client := newEgressServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			assert.Fail(t, fmt.Sprintf("expected PATCH, got %s", r.Method))
		}

		var rules []NetworkRule
		json.NewDecoder(r.Body).Decode(&rules)
		if len(rules) != 1 {
			assert.Fail(t, fmt.Sprintf("expected 1 rule in request, got %d", len(rules)))
		}

		jsonResponse(w, http.StatusOK, want)
	})

	got, err := client.PatchPolicy(context.Background(), []NetworkRule{
		{Action: "allow", Target: "cdn.example.com"},
	})
	require.NoErrorf(t, err, "PatchPolicy")
	require.NotNil(t, got.Policy)
	require.Len(t, got.Policy.Egress, 2)
}

func TestPing(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if r.URL.Path != "/ping" {
			assert.Fail(t, fmt.Sprintf("expected /ping, got %s", r.URL.Path))
		}
		w.WriteHeader(http.StatusOK)
	})

	err := client.Ping(context.Background())
	require.NoErrorf(t, err, "Ping")
}

func TestRunCommand_SSE(t *testing.T) {
	ssePayload := "event: stdout\ndata: hello world\n\nevent: stderr\ndata: warning\n\nevent: result\ndata: {\"exit_code\": 0}\n\n"

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/command" {
			assert.Fail(t, fmt.Sprintf("expected /command, got %s", r.URL.Path))
		}

		var req RunCommandRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Command != "echo hello" {
			assert.Fail(t, fmt.Sprintf("Command = %q, want %q", req.Command, "echo hello"))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload))
	})

	var mu sync.Mutex
	var events []StreamEvent
	err := client.RunCommand(context.Background(), RunCommandRequest{
		Command: "echo hello",
	}, func(event StreamEvent) error {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		return nil
	})
	require.NoErrorf(t, err, "RunCommand")

	require.Len(t, events, 3)
	if events[0].Event != "stdout" || events[0].Data != "hello world" {
		assert.Fail(t, fmt.Sprintf("event[0] = %+v, want stdout/hello world", events[0]))
	}
	if events[1].Event != "stderr" || events[1].Data != "warning" {
		assert.Fail(t, fmt.Sprintf("event[1] = %+v, want stderr/warning", events[1]))
	}
	if events[2].Event != "result" {
		assert.Fail(t, fmt.Sprintf("event[2].Event = %q, want result", events[2].Event))
	}
}

func TestGetFileInfo(t *testing.T) {
	want := map[string]FileInfo{
		"/tmp/test.txt": {
			Path:       "/tmp/test.txt",
			Size:       1024,
			ModifiedAt: time.Now().UTC().Truncate(time.Second),
			CreatedAt:  time.Now().UTC().Truncate(time.Second),
			Owner:      "root",
			Group:      "root",
			Mode:       0644,
		},
	}

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if !strings.HasPrefix(r.URL.Path, "/files/info") {
			assert.Fail(t, fmt.Sprintf("expected /files/info, got %s", r.URL.Path))
		}
		if r.URL.Query().Get("path") != "/tmp/test.txt" {
			assert.Fail(t, fmt.Sprintf("expected path=/tmp/test.txt, got %s", r.URL.Query().Get("path")))
		}
		jsonResponse(w, http.StatusOK, want)
	})

	got, err := client.GetFileInfo(context.Background(), "/tmp/test.txt")
	require.NoErrorf(t, err, "GetFileInfo")
	info, ok := got["/tmp/test.txt"]
	if !ok {
		require.FailNow(t, "expected /tmp/test.txt in result")
	}
	if info.Size != 1024 {
		assert.Fail(t, fmt.Sprintf("Size = %d, want 1024", info.Size))
	}
	if info.Owner != "root" {
		assert.Fail(t, fmt.Sprintf("Owner = %q, want root", info.Owner))
	}
}

func TestUploadFile(t *testing.T) {
	// Create a temp file to upload.
	tmpFile, err := os.CreateTemp("", "opensandbox-test-*")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())
	tmpFile.WriteString("file contents here")
	tmpFile.Close()

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/files/upload" {
			assert.Fail(t, fmt.Sprintf("expected /files/upload, got %s", r.URL.Path))
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			assert.Fail(t, fmt.Sprintf("expected multipart content type, got %s", r.Header.Get("Content-Type")))
		}

		// Verify metadata file part exists.
		r.ParseMultipartForm(1 << 20)
		metaFile, _, mfErr := r.FormFile("metadata")
		require.NoErrorf(t, mfErr, "expected metadata file part")
		metaBytes, rdErr := io.ReadAll(metaFile)
		require.NoErrorf(t, rdErr, "read metadata part")
		require.NoError(t, metaFile.Close())
		metaStr := string(metaBytes)
		if metaStr == "" {
			assert.Fail(t, "expected metadata content")
		}
		var meta FileMetadata
		json.Unmarshal([]byte(metaStr), &meta)
		if meta.Path != "/sandbox/upload.txt" {
			assert.Fail(t, fmt.Sprintf("metadata path = %q, want /sandbox/upload.txt", meta.Path))
		}

		// Verify file part exists.
		file, _, fErr := r.FormFile("file")
		if fErr != nil {
			assert.Fail(t, fmt.Sprintf("expected file part: %v", fErr))
		} else {
			data, _ := io.ReadAll(file)
			if string(data) != "file contents here" {
				assert.Fail(t, fmt.Sprintf("file content = %q, want %q", string(data), "file contents here"))
			}
			file.Close()
		}

		w.WriteHeader(http.StatusOK)
	})

	up, err := os.Open(tmpFile.Name())
	require.NoError(t, err)
	defer up.Close()
	err = client.UploadFile(context.Background(), up, UploadFileOptions{
		FileName: "upload.txt",
		Metadata: FileMetadata{Path: "/sandbox/upload.txt"},
	})
	require.NoErrorf(t, err, "UploadFile")
}

func TestUploadFile_WithCustomHeaders(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "opensandbox-upload-headers-*")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())
	_, werr := tmpFile.WriteString("header-check")
	require.NoError(t, werr)
	require.NoError(t, tmpFile.Close())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Header") != "upload-ok" {
			assert.Fail(t, fmt.Sprintf("X-Test-Header = %q, want %q", r.Header.Get("X-Test-Header"), "upload-ok"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "token", WithHeaders(map[string]string{
		"X-Test-Header": "upload-ok",
	}))

	up, err := os.Open(tmpFile.Name())
	require.NoError(t, err)
	defer up.Close()
	err = client.UploadFile(context.Background(), up, UploadFileOptions{
		FileName: "upload.txt",
		Metadata: FileMetadata{Path: "/tmp/upload.txt"},
	})
	require.NoErrorf(t, err, "UploadFile with custom headers")
}

func TestUploadFile_WithReader(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/files/upload", r.URL.Path)

		require.NoError(t, r.ParseMultipartForm(1<<20))
		metaFile, _, err := r.FormFile("metadata")
		require.NoError(t, err)
		metaBytes, err := io.ReadAll(metaFile)
		require.NoError(t, err)
		require.NoError(t, metaFile.Close())

		var meta FileMetadata
		require.NoError(t, json.Unmarshal(metaBytes, &meta))
		require.Equal(t, "/sandbox/reader.txt", meta.Path)

		filePart, fileHeader, err := r.FormFile("file")
		require.NoError(t, err)
		require.Equal(t, "reader.txt", fileHeader.Filename)
		data, err := io.ReadAll(filePart)
		require.NoError(t, err)
		require.NoError(t, filePart.Close())
		require.Equal(t, "reader-content", string(data))

		w.WriteHeader(http.StatusOK)
	})

	err := client.UploadFile(context.Background(), strings.NewReader("reader-content"), UploadFileOptions{
		FileName: "reader.txt",
		Metadata: FileMetadata{Path: "/sandbox/reader.txt"},
	})
	require.NoError(t, err)
}

func TestUploadFiles(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/files/upload", r.URL.Path)
		require.True(t, strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data"))

		require.NoError(t, r.ParseMultipartForm(1<<20))
		metadataParts := r.MultipartForm.File["metadata"]
		fileParts := r.MultipartForm.File["file"]
		require.Len(t, metadataParts, 2)
		require.Len(t, fileParts, 2)

		wantPaths := []string{"/sandbox/a.txt", "/sandbox/b.txt"}
		wantFileNames := []string{"a.txt", "custom-b.txt"}
		wantContents := []string{"alpha", "bravo"}

		for i := range metadataParts {
			metaFile, err := metadataParts[i].Open()
			require.NoError(t, err)
			require.Equal(t, "application/json", metadataParts[i].Header.Get("Content-Type"))
			metaBytes, err := io.ReadAll(metaFile)
			require.NoError(t, err)
			require.NoError(t, metaFile.Close())

			var meta FileMetadata
			require.NoError(t, json.Unmarshal(metaBytes, &meta))
			require.Equal(t, wantPaths[i], meta.Path)
			require.Equal(t, 600+i, meta.Mode)

			filePart, err := fileParts[i].Open()
			require.NoError(t, err)
			data, err := io.ReadAll(filePart)
			require.NoError(t, err)
			require.NoError(t, filePart.Close())
			require.Equal(t, wantFileNames[i], fileParts[i].Filename)
			require.Equal(t, wantContents[i], string(data))
		}

		w.WriteHeader(http.StatusOK)
	})

	err := client.UploadFiles(context.Background(), []UploadFileEntry{
		{
			File: strings.NewReader("alpha"),
			Options: UploadFileOptions{
				FileName: "a.txt",
				Metadata: FileMetadata{Path: "/sandbox/a.txt", Mode: 600},
			},
		},
		{
			File: strings.NewReader("bravo"),
			Options: UploadFileOptions{
				FileName: "custom-b.txt",
				Metadata: FileMetadata{Path: "/sandbox/b.txt", Mode: 601},
			},
		},
	})
	require.NoError(t, err)
}

func TestGetMetrics(t *testing.T) {
	want := Metrics{
		CPUCount:   4,
		CPUUsedPct: 25.5,
		MemTotalMB: 8192,
		MemUsedMB:  4096,
		Timestamp:  time.Now().Unix(),
	}

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if r.URL.Path != "/metrics" {
			assert.Fail(t, fmt.Sprintf("expected /metrics, got %s", r.URL.Path))
		}
		jsonResponse(w, http.StatusOK, want)
	})

	got, err := client.GetMetrics(context.Background())
	require.NoErrorf(t, err, "GetMetrics")
	if got.CPUCount != 4 {
		assert.Fail(t, fmt.Sprintf("CPUCount = %f, want 4", got.CPUCount))
	}
	if got.MemTotalMB != 8192 {
		assert.Fail(t, fmt.Sprintf("MemTotalMB = %f, want 8192", got.MemTotalMB))
	}
}

func TestStreamSSE(t *testing.T) {
	ssePayload := strings.Join([]string{
		"event: start",
		"data: initializing",
		"",
		"event: progress",
		"data: step 1",
		"data: step 2",
		"",
		"id: evt-3",
		"event: done",
		"data: complete",
		"",
		": this is a comment",
		"event: final",
		"data: goodbye",
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload))
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "tok")

	var events []StreamEvent
	err := client.RunCommand(context.Background(), RunCommandRequest{
		Command: "test",
	}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	require.NoErrorf(t, err, "stream")

	require.Len(t, events, 4)

	// Event 1: start
	if events[0].Event != "start" || events[0].Data != "initializing" {
		assert.Fail(t, fmt.Sprintf("event[0] = %+v", events[0]))
	}

	// Event 2: progress with multi-line data
	if events[1].Event != "progress" || events[1].Data != "step 1\nstep 2" {
		assert.Fail(t, fmt.Sprintf("event[1] = %+v, want progress/step 1\\nstep 2", events[1]))
	}

	// Event 3: done with ID
	if events[2].Event != "done" || events[2].Data != "complete" || events[2].ID != "evt-3" {
		assert.Fail(t, fmt.Sprintf("event[2] = %+v", events[2]))
	}

	// Event 4: final (comment should be skipped)
	if events[3].Event != "final" || events[3].Data != "goodbye" {
		assert.Fail(t, fmt.Sprintf("event[3] = %+v", events[3]))
	}
}

func TestStreamSSE_NDJSON(t *testing.T) {
	// Simulate the real execd server format: raw JSON blobs separated by blank lines.
	ndjsonPayload := "{\"type\":\"stdout\",\"data\":\"hello\"}\n\n{\"type\":\"result\",\"exit_code\":0}\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ndjsonPayload))
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "tok")

	var events []StreamEvent
	err := client.RunCommand(context.Background(), RunCommandRequest{
		Command: "test",
	}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	require.NoErrorf(t, err, "stream")

	require.Len(t, events, 2)

	// NDJSON events with a "type" field should have Event populated.
	if events[0].Event != "stdout" {
		assert.Fail(t, fmt.Sprintf("event[0].Event = %q, want %q", events[0].Event, "stdout"))
	}
	if events[0].Data != `{"type":"stdout","data":"hello"}` {
		assert.Fail(t, fmt.Sprintf("event[0].Data = %q", events[0].Data))
	}
	if events[1].Event != "result" {
		assert.Fail(t, fmt.Sprintf("event[1].Event = %q, want %q", events[1].Event, "result"))
	}
	if events[1].Data != `{"type":"result","exit_code":0}` {
		assert.Fail(t, fmt.Sprintf("event[1].Data = %q", events[1].Data))
	}
}

func TestLifecycleAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("OPEN-SANDBOX-API-KEY")
		if got != "my-lifecycle-key" {
			assert.Fail(t, fmt.Sprintf("OPEN-SANDBOX-API-KEY = %q, want %q", got, "my-lifecycle-key"))
		}
		jsonResponse(w, http.StatusOK, SandboxInfo{ID: "sbx-1", CreatedAt: time.Now()})
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "my-lifecycle-key")
	_, err := client.GetSandbox(context.Background(), "sbx-1")
	require.NoErrorf(t, err, "GetSandbox")
}

func TestExecdAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-EXECD-ACCESS-TOKEN")
		if got != "my-execd-token" {
			assert.Fail(t, fmt.Sprintf("X-EXECD-ACCESS-TOKEN = %q, want %q", got, "my-execd-token"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "my-execd-token")
	err := client.Ping(context.Background())
	require.NoErrorf(t, err, "Ping")
}

// TestResolveExecdForwardsAllEndpointHeaders verifies that every header
// returned by GetEndpoint (auth tokens, routing hints, sticky-session keys,
// etc.) is forwarded as-is on subsequent execd requests, mirroring the
// Python SDK behavior.
func TestResolveExecdForwardsAllEndpointHeaders(t *testing.T) {
	endpointHeaders := map[string]string{
		"X-EXECD-ACCESS-TOKEN": "execd-tok",
		"X-Route-Hint":         "vip-pool",
		"X-Sticky-Session":     "sess-abc",
	}

	execdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, want := range endpointHeaders {
			if got := r.Header.Get(k); got != want {
				assert.Fail(t, fmt.Sprintf("header %s = %q, want %q", k, got, want))
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer execdSrv.Close()

	lifecycleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/endpoints/") {
			jsonResponse(w, http.StatusOK, Endpoint{
				Endpoint: execdSrv.URL,
				Headers:  endpointHeaders,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer lifecycleSrv.Close()

	config := ConnectionConfig{Domain: lifecycleSrv.URL}
	sb := &Sandbox{
		id:        "sbx-headers",
		config:    &config,
		lifecycle: config.lifecycleClient(),
	}

	require.NoErrorf(t, sb.resolveExecd(context.Background()), "resolveExecd")
	require.NoErrorf(t, sb.execd.Ping(context.Background()), "Ping")
}

// TestResolveEgressForwardsAllEndpointHeaders verifies the same forwarding
// behavior for the egress sidecar client.
func TestResolveEgressForwardsAllEndpointHeaders(t *testing.T) {
	endpointHeaders := map[string]string{
		"OPENSANDBOX-EGRESS-AUTH": "egress-tok",
		"X-Route-Hint":            "egress-vip",
	}

	egressSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, want := range endpointHeaders {
			if got := r.Header.Get(k); got != want {
				assert.Fail(t, fmt.Sprintf("header %s = %q, want %q", k, got, want))
			}
		}
		jsonResponse(w, http.StatusOK, PolicyStatusResponse{Status: "ok"})
	}))
	defer egressSrv.Close()

	lifecycleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/endpoints/") {
			jsonResponse(w, http.StatusOK, Endpoint{
				Endpoint: egressSrv.URL,
				Headers:  endpointHeaders,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer lifecycleSrv.Close()

	config := ConnectionConfig{Domain: lifecycleSrv.URL}
	sb := &Sandbox{
		id:        "sbx-egress-headers",
		config:    &config,
		lifecycle: config.lifecycleClient(),
	}

	require.NoErrorf(t, sb.resolveEgress(context.Background()), "resolveEgress")
	_, err := sb.egress.GetPolicy(context.Background())
	require.NoErrorf(t, err, "GetPolicy")
}

func TestSandboxManager_ListFilter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	want := ListSandboxesResponse{
		Items: []SandboxInfo{
			{ID: "sbx-a", Status: SandboxStatus{State: StateRunning}, Metadata: map[string]string{"env": "prod"}, CreatedAt: now},
		},
		Pagination: PaginationInfo{Page: 1, PageSize: 10, TotalItems: 1, TotalPages: 1, HasNextPage: false},
	}

	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}

		q := r.URL.Query()
		// Verify state filter
		states := q["state"]
		if len(states) != 1 || states[0] != "Running" {
			assert.Fail(t, fmt.Sprintf("expected state=[Running], got %v", states))
		}

		// Verify metadata filter
		meta := q.Get("metadata")
		if meta == "" {
			assert.Fail(t, "expected metadata query param")
		}
		if !strings.Contains(meta, "env=prod") {
			assert.Fail(t, fmt.Sprintf("expected metadata to contain env=prod, got %q", meta))
		}

		// Verify pagination
		if q.Get("page") != "1" {
			assert.Fail(t, fmt.Sprintf("expected page=1, got %s", q.Get("page")))
		}
		if q.Get("pageSize") != "10" {
			assert.Fail(t, fmt.Sprintf("expected pageSize=10, got %s", q.Get("pageSize")))
		}

		jsonResponse(w, http.StatusOK, want)
	})

	mgr := &SandboxManager{lifecycle: client}
	got, err := mgr.ListSandboxInfos(context.Background(), ListOptions{
		States:   []SandboxState{StateRunning},
		Metadata: map[string]string{"env": "prod"},
		Page:     1,
		PageSize: 10,
	})
	require.NoErrorf(t, err, "ListSandboxInfos")
	require.Len(t, got.Items, 1)
	if got.Items[0].ID != "sbx-a" {
		assert.Fail(t, fmt.Sprintf("ID = %q, want %q", got.Items[0].ID, "sbx-a"))
	}
	if got.Items[0].Metadata["env"] != "prod" {
		assert.Fail(t, fmt.Sprintf("Metadata[env] = %q, want %q", got.Items[0].Metadata["env"], "prod"))
	}
}

func TestSandboxManager_ListMultipleStates(t *testing.T) {
	want := ListSandboxesResponse{
		Items: []SandboxInfo{
			{ID: "sbx-1", Status: SandboxStatus{State: StateRunning}, CreatedAt: time.Now().UTC().Truncate(time.Second)},
			{ID: "sbx-2", Status: SandboxStatus{State: StatePaused}, CreatedAt: time.Now().UTC().Truncate(time.Second)},
		},
		Pagination: PaginationInfo{Page: 1, PageSize: 20, TotalItems: 2, TotalPages: 1},
	}

	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		states := r.URL.Query()["state"]
		if len(states) != 2 {
			assert.Fail(t, fmt.Sprintf("expected 2 state params, got %d: %v", len(states), states))
		}
		jsonResponse(w, http.StatusOK, want)
	})

	mgr := &SandboxManager{lifecycle: client}
	got, err := mgr.ListSandboxInfos(context.Background(), ListOptions{
		States: []SandboxState{StateRunning, StatePaused},
	})
	require.NoErrorf(t, err, "ListSandboxInfos")
	require.Len(t, got.Items, 2)
}

func TestSandboxManager_GetSandboxInfo(t *testing.T) {
	want := SandboxInfo{
		ID:        "sbx-get",
		Status:    SandboxStatus{State: StateRunning},
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}

	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if r.URL.Path != "/sandboxes/sbx-get" {
			assert.Fail(t, fmt.Sprintf("expected /sandboxes/sbx-get, got %s", r.URL.Path))
		}
		jsonResponse(w, http.StatusOK, want)
	})

	mgr := &SandboxManager{lifecycle: client}
	got, err := mgr.GetSandboxInfo(context.Background(), "sbx-get")
	require.NoErrorf(t, err, "GetSandboxInfo")
	if got.ID != "sbx-get" {
		assert.Fail(t, fmt.Sprintf("ID = %q, want %q", got.ID, "sbx-get"))
	}
}

func TestSandboxManager_KillSandbox(t *testing.T) {
	var called bool
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			assert.Fail(t, fmt.Sprintf("expected DELETE, got %s", r.Method))
		}
		if r.URL.Path != "/sandboxes/sbx-kill" {
			assert.Fail(t, fmt.Sprintf("expected /sandboxes/sbx-kill, got %s", r.URL.Path))
		}
		called = true
		w.WriteHeader(http.StatusNoContent)
	})

	mgr := &SandboxManager{lifecycle: client}
	err := mgr.KillSandbox(context.Background(), "sbx-kill")
	require.NoErrorf(t, err, "KillSandbox")
	if !called {
		assert.Fail(t, "expected DELETE to be called")
	}
}

func TestSandboxManager_PauseSandbox(t *testing.T) {
	var called bool
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/sandboxes/sbx-mgr-pause/pause" {
			assert.Fail(t, fmt.Sprintf("expected /sandboxes/sbx-mgr-pause/pause, got %s", r.URL.Path))
		}
		called = true
		w.WriteHeader(http.StatusAccepted)
	})

	mgr := &SandboxManager{lifecycle: client}
	err := mgr.PauseSandbox(context.Background(), "sbx-mgr-pause")
	require.NoErrorf(t, err, "PauseSandbox")
	if !called {
		assert.Fail(t, "expected pause endpoint to be called")
	}
}

func TestSandboxManager_ResumeSandbox(t *testing.T) {
	var called bool
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/sandboxes/sbx-mgr-resume/resume" {
			assert.Fail(t, fmt.Sprintf("expected /sandboxes/sbx-mgr-resume/resume, got %s", r.URL.Path))
		}
		called = true
		w.WriteHeader(http.StatusAccepted)
	})

	mgr := &SandboxManager{lifecycle: client}
	err := mgr.ResumeSandbox(context.Background(), "sbx-mgr-resume")
	require.NoErrorf(t, err, "ResumeSandbox")
	if !called {
		assert.Fail(t, "expected resume endpoint to be called")
	}
}

func TestSandboxManager_RenewSandbox(t *testing.T) {
	wantExpiry := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)

	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/sandboxes/sbx-renew/renew-expiration" {
			assert.Fail(t, fmt.Sprintf("expected /sandboxes/sbx-renew/renew-expiration, got %s", r.URL.Path))
		}

		var req RenewExpirationRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.ExpiresAt.IsZero() {
			assert.Fail(t, "expected non-zero ExpiresAt")
		}

		jsonResponse(w, http.StatusOK, RenewExpirationResponse{ExpiresAt: wantExpiry})
	})

	mgr := &SandboxManager{lifecycle: client}
	got, err := mgr.RenewSandbox(context.Background(), "sbx-renew", 1*time.Hour)
	require.NoErrorf(t, err, "RenewSandbox")
	if got.ExpiresAt.IsZero() {
		assert.Fail(t, "expected non-zero ExpiresAt in response")
	}
}

func TestCreateDirectory(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/directories" {
			assert.Fail(t, fmt.Sprintf("expected /directories, got %s", r.URL.Path))
		}

		var body map[string]map[string]int
		json.NewDecoder(r.Body).Decode(&body)
		dirEntry, ok := body["/sandbox/mydir"]
		if !ok {
			assert.Fail(t, "expected /sandbox/mydir key in request body")
		}
		if dirEntry["mode"] != 755 {
			assert.Fail(t, fmt.Sprintf("mode = %d, want 755", dirEntry["mode"]))
		}

		w.WriteHeader(http.StatusOK)
	})

	err := client.CreateDirectory(context.Background(), "/sandbox/mydir", 755)
	require.NoErrorf(t, err, "CreateDirectory")
}

func TestDeleteDirectory(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			assert.Fail(t, fmt.Sprintf("expected DELETE, got %s", r.Method))
		}
		if !strings.HasPrefix(r.URL.Path, "/directories") {
			assert.Fail(t, fmt.Sprintf("expected /directories path, got %s", r.URL.Path))
		}
		if r.URL.Query().Get("path") != "/sandbox/mydir" {
			assert.Fail(t, fmt.Sprintf("expected path=/sandbox/mydir, got %s", r.URL.Query().Get("path")))
		}
		w.WriteHeader(http.StatusOK)
	})

	err := client.DeleteDirectory(context.Background(), "/sandbox/mydir")
	require.NoErrorf(t, err, "DeleteDirectory")
}

func TestDeleteFiles(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			assert.Fail(t, fmt.Sprintf("expected DELETE, got %s", r.Method))
		}
		if !strings.HasPrefix(r.URL.Path, "/files") {
			assert.Fail(t, fmt.Sprintf("expected /files path, got %s", r.URL.Path))
		}

		paths := r.URL.Query()["path"]
		if len(paths) != 2 {
			assert.Fail(t, fmt.Sprintf("expected 2 path params, got %d: %v", len(paths), paths))
		}

		w.WriteHeader(http.StatusOK)
	})

	err := client.DeleteFiles(context.Background(), []string{"/tmp/a.txt", "/tmp/b.txt"})
	require.NoErrorf(t, err, "DeleteFiles")
}

func TestMoveFiles(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/files/mv" {
			assert.Fail(t, fmt.Sprintf("expected /files/mv, got %s", r.URL.Path))
		}

		var req MoveRequest
		json.NewDecoder(r.Body).Decode(&req)
		require.Len(t, req, 1)
		if req[0].Src != "/tmp/old.txt" || req[0].Dest != "/tmp/new.txt" {
			assert.Fail(t, fmt.Sprintf("move item = %+v, want src=/tmp/old.txt dest=/tmp/new.txt", req[0]))
		}

		w.WriteHeader(http.StatusOK)
	})

	err := client.MoveFiles(context.Background(), MoveRequest{
		{Src: "/tmp/old.txt", Dest: "/tmp/new.txt"},
	})
	require.NoErrorf(t, err, "MoveFiles")
}

func TestSearchFiles(t *testing.T) {
	want := []FileInfo{
		{Path: "/sandbox/test.py", Size: 256, Owner: "root", Group: "root", Mode: 644},
		{Path: "/sandbox/test2.py", Size: 128, Owner: "root", Group: "root", Mode: 644},
	}

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if !strings.HasPrefix(r.URL.Path, "/files/search") {
			assert.Fail(t, fmt.Sprintf("expected /files/search, got %s", r.URL.Path))
		}
		if r.URL.Query().Get("path") != "/sandbox" {
			assert.Fail(t, fmt.Sprintf("expected path=/sandbox, got %s", r.URL.Query().Get("path")))
		}
		if r.URL.Query().Get("pattern") != "*.py" {
			assert.Fail(t, fmt.Sprintf("expected pattern=*.py, got %s", r.URL.Query().Get("pattern")))
		}

		jsonResponse(w, http.StatusOK, want)
	})

	got, err := client.SearchFiles(context.Background(), "/sandbox", "*.py")
	require.NoErrorf(t, err, "SearchFiles")
	require.Len(t, got, 2)
	if got[0].Path != "/sandbox/test.py" {
		assert.Fail(t, fmt.Sprintf("Path[0] = %q, want /sandbox/test.py", got[0].Path))
	}
}

func TestSetPermissions(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/files/permissions" {
			assert.Fail(t, fmt.Sprintf("expected /files/permissions, got %s", r.URL.Path))
		}

		var req PermissionsRequest
		json.NewDecoder(r.Body).Decode(&req)
		perm, ok := req["/tmp/script.sh"]
		if !ok {
			assert.Fail(t, "expected /tmp/script.sh key in request")
		}
		if perm.Mode != 755 {
			assert.Fail(t, fmt.Sprintf("Mode = %d, want 755", perm.Mode))
		}
		if perm.Owner != "root" {
			assert.Fail(t, fmt.Sprintf("Owner = %q, want root", perm.Owner))
		}

		w.WriteHeader(http.StatusOK)
	})

	err := client.SetPermissions(context.Background(), PermissionsRequest{
		"/tmp/script.sh": {Owner: "root", Group: "root", Mode: 755},
	})
	require.NoErrorf(t, err, "SetPermissions")
}

func TestReplaceInFiles(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/files/replace" {
			assert.Fail(t, fmt.Sprintf("expected /files/replace, got %s", r.URL.Path))
		}

		var req ReplaceRequest
		json.NewDecoder(r.Body).Decode(&req)
		item, ok := req["/tmp/config.txt"]
		if !ok {
			assert.Fail(t, "expected /tmp/config.txt key in request")
		}
		if item.Old != "localhost" || item.New != "production.example.com" {
			assert.Fail(t, fmt.Sprintf("replace item = %+v", item))
		}

		w.WriteHeader(http.StatusOK)
	})

	err := client.ReplaceInFiles(context.Background(), ReplaceRequest{
		"/tmp/config.txt": {Old: "localhost", New: "production.example.com"},
	})
	require.NoErrorf(t, err, "ReplaceInFiles")
}

func TestDownloadFile(t *testing.T) {
	fileContent := "hello from sandbox file"

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if !strings.HasPrefix(r.URL.Path, "/files/download") {
			assert.Fail(t, fmt.Sprintf("expected /files/download, got %s", r.URL.Path))
		}
		if r.URL.Query().Get("path") != "/sandbox/output.txt" {
			assert.Fail(t, fmt.Sprintf("expected path=/sandbox/output.txt, got %s", r.URL.Query().Get("path")))
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fileContent))
	})

	rc, err := client.DownloadFile(context.Background(), "/sandbox/output.txt", "")
	require.NoErrorf(t, err, "DownloadFile")
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoErrorf(t, err, "ReadAll")
	if string(data) != fileContent {
		assert.Fail(t, fmt.Sprintf("content = %q, want %q", string(data), fileContent))
	}
}

func TestDownloadFile_Range(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		rangeHdr := r.Header.Get("Range")
		if rangeHdr != "bytes=0-4" {
			assert.Fail(t, fmt.Sprintf("Range = %q, want %q", rangeHdr, "bytes=0-4"))
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("hello"))
	})

	rc, err := client.DownloadFile(context.Background(), "/sandbox/big.bin", "bytes=0-4")
	require.NoErrorf(t, err, "DownloadFile range")
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoErrorf(t, err, "ReadAll")
	if string(data) != "hello" {
		assert.Fail(t, fmt.Sprintf("content = %q, want %q", string(data), "hello"))
	}
}

func TestDownloadFile_WithCustomHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Header") != "download-ok" {
			assert.Fail(t, fmt.Sprintf("X-Test-Header = %q, want %q", r.Header.Get("X-Test-Header"), "download-ok"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "token", WithHeaders(map[string]string{
		"X-Test-Header": "download-ok",
	}))

	rc, err := client.DownloadFile(context.Background(), "/tmp/data.txt", "")
	require.NoErrorf(t, err, "DownloadFile with custom headers")
	defer rc.Close()
}

func TestCreateContext(t *testing.T) {
	want := CodeContext{ID: "ctx-123", Language: "python"}

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/code/context" {
			assert.Fail(t, fmt.Sprintf("expected /code/context, got %s", r.URL.Path))
		}

		var req CreateContextRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Language != "python" {
			assert.Fail(t, fmt.Sprintf("Language = %q, want python", req.Language))
		}

		jsonResponse(w, http.StatusCreated, want)
	})

	got, err := client.CreateContext(context.Background(), CreateContextRequest{Language: "python"})
	require.NoErrorf(t, err, "CreateContext")
	if got.ID != "ctx-123" {
		assert.Fail(t, fmt.Sprintf("ID = %q, want ctx-123", got.ID))
	}
	if got.Language != "python" {
		assert.Fail(t, fmt.Sprintf("Language = %q, want python", got.Language))
	}
}

func TestGetContext(t *testing.T) {
	want := CodeContext{ID: "ctx-456", Language: "python"}

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if r.URL.Path != "/code/contexts/ctx-456" {
			assert.Fail(t, fmt.Sprintf("expected /code/contexts/ctx-456, got %s", r.URL.Path))
		}
		jsonResponse(w, http.StatusOK, want)
	})

	got, err := client.GetContext(context.Background(), "ctx-456")
	require.NoErrorf(t, err, "GetContext")
	if got.ID != "ctx-456" {
		assert.Fail(t, fmt.Sprintf("ID = %q, want ctx-456", got.ID))
	}
}

func TestListContexts(t *testing.T) {
	want := []CodeContext{
		{ID: "ctx-1", Language: "python"},
		{ID: "ctx-2", Language: "python"},
	}

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if !strings.HasPrefix(r.URL.Path, "/code/contexts") {
			assert.Fail(t, fmt.Sprintf("expected /code/contexts, got %s", r.URL.Path))
		}
		if r.URL.Query().Get("language") != "python" {
			assert.Fail(t, fmt.Sprintf("expected language=python, got %s", r.URL.Query().Get("language")))
		}
		jsonResponse(w, http.StatusOK, want)
	})

	got, err := client.ListContexts(context.Background(), "python")
	require.NoErrorf(t, err, "ListContexts")
	require.Len(t, got, 2)
	if got[0].ID != "ctx-1" {
		assert.Fail(t, fmt.Sprintf("ID[0] = %q, want ctx-1", got[0].ID))
	}
}

func TestDeleteContext(t *testing.T) {
	var called bool
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			assert.Fail(t, fmt.Sprintf("expected DELETE, got %s", r.Method))
		}
		if r.URL.Path != "/code/contexts/ctx-del" {
			assert.Fail(t, fmt.Sprintf("expected /code/contexts/ctx-del, got %s", r.URL.Path))
		}
		called = true
		w.WriteHeader(http.StatusOK)
	})

	err := client.DeleteContext(context.Background(), "ctx-del")
	require.NoErrorf(t, err, "DeleteContext")
	if !called {
		assert.Fail(t, "expected DELETE to be called")
	}
}

func TestDeleteContextsByLanguage(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			assert.Fail(t, fmt.Sprintf("expected DELETE, got %s", r.Method))
		}
		if !strings.HasPrefix(r.URL.Path, "/code/contexts") {
			assert.Fail(t, fmt.Sprintf("expected /code/contexts path, got %s", r.URL.Path))
		}
		if r.URL.Query().Get("language") != "python" {
			assert.Fail(t, fmt.Sprintf("expected language=python, got %s", r.URL.Query().Get("language")))
		}
		w.WriteHeader(http.StatusOK)
	})

	err := client.DeleteContextsByLanguage(context.Background(), "python")
	require.NoErrorf(t, err, "DeleteContextsByLanguage")
}

func TestExecuteCode_SSE(t *testing.T) {
	// Simulate execd SSE response for code execution
	ssePayload := strings.Join([]string{
		`{"type":"init","text":"exec-001","timestamp":1000}`,
		"",
		`{"type":"stdout","text":"4","timestamp":1001}`,
		"",
		`{"type":"result","results":{"text/plain":"4"},"timestamp":1002}`,
		"",
		`{"type":"execution_complete","timestamp":1003,"execution_time":50}`,
		"",
	}, "\n")

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/code" {
			assert.Fail(t, fmt.Sprintf("expected /code, got %s", r.URL.Path))
		}

		var req RunCodeRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Code != "2+2" {
			assert.Fail(t, fmt.Sprintf("Code = %q, want 2+2", req.Code))
		}
		if req.Context == nil || req.Context.Language != "python" {
			assert.Fail(t, fmt.Sprintf("expected context with language python, got %+v", req.Context))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload))
	})

	var events []StreamEvent
	err := client.ExecuteCode(context.Background(), RunCodeRequest{
		Context: &CodeContext{Language: "python"},
		Code:    "2+2",
	}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	require.NoErrorf(t, err, "ExecuteCode")

	require.Len(t, events, 4)
	if events[0].Event != "init" {
		assert.Fail(t, fmt.Sprintf("event[0].Event = %q, want init", events[0].Event))
	}
	if events[1].Event != "stdout" {
		assert.Fail(t, fmt.Sprintf("event[1].Event = %q, want stdout", events[1].Event))
	}
	if events[2].Event != "result" {
		assert.Fail(t, fmt.Sprintf("event[2].Event = %q, want result", events[2].Event))
	}
	if events[3].Event != "execution_complete" {
		assert.Fail(t, fmt.Sprintf("event[3].Event = %q, want execution_complete", events[3].Event))
	}
}

func TestExecuteCode_SSE_EmptyStream(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/code" {
			assert.Fail(t, fmt.Sprintf("expected /code, got %s", r.URL.Path))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	})

	err := client.ExecuteCode(context.Background(), RunCodeRequest{
		Context: &CodeContext{Language: "python"},
		Code:    "2+2",
	}, func(event StreamEvent) error {
		return nil
	})
	if err == nil {
		require.FailNow(t, "ExecuteCode should fail on empty SSE stream")
	}
	if !strings.Contains(err.Error(), "empty sse stream") {
		assert.Fail(t, fmt.Sprintf("err = %v, want empty sse stream", err))
	}
}

func TestExecuteCode_InContext(t *testing.T) {
	ssePayload := `{"type":"stdout","text":"hello from context","timestamp":1000}` + "\n\n" +
		`{"type":"execution_complete","timestamp":1001,"execution_time":10}` + "\n\n"

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req RunCodeRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Context == nil {
			require.FailNow(t, "expected context in request")
		}
		if req.Context.ID != "ctx-persist" {
			assert.Fail(t, fmt.Sprintf("Context.ID = %q, want ctx-persist", req.Context.ID))
		}
		if req.Context.Language != "python" {
			assert.Fail(t, fmt.Sprintf("Context.Language = %q, want python", req.Context.Language))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload))
	})

	var events []StreamEvent
	err := client.ExecuteCode(context.Background(), RunCodeRequest{
		Context: &CodeContext{ID: "ctx-persist", Language: "python"},
		Code:    "print('hello from context')",
	}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	require.NoErrorf(t, err, "ExecuteCode in context")
	require.Len(t, events, 2)
}

func TestInterruptCode(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			assert.Fail(t, fmt.Sprintf("expected DELETE, got %s", r.Method))
		}
		if !strings.HasPrefix(r.URL.Path, "/code") {
			assert.Fail(t, fmt.Sprintf("expected /code path, got %s", r.URL.Path))
		}
		if r.URL.Query().Get("id") != "session-interrupt" {
			assert.Fail(t, fmt.Sprintf("expected id=session-interrupt, got %s", r.URL.Query().Get("id")))
		}
		w.WriteHeader(http.StatusOK)
	})

	err := client.InterruptCode(context.Background(), "session-interrupt")
	require.NoErrorf(t, err, "InterruptCode")
}

func TestCreateSession(t *testing.T) {
	want := Session{ID: "sess-abc"}

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/session" {
			assert.Fail(t, fmt.Sprintf("expected /session, got %s", r.URL.Path))
		}
		jsonResponse(w, http.StatusCreated, want)
	})

	got, err := client.CreateSession(context.Background())
	require.NoErrorf(t, err, "CreateSession")
	if got.ID != "sess-abc" {
		assert.Fail(t, fmt.Sprintf("ID = %q, want sess-abc", got.ID))
	}
}

func TestRunInSession_SSE(t *testing.T) {
	ssePayload := `{"type":"stdout","text":"bar","timestamp":2000}` + "\n\n" +
		`{"type":"execution_complete","timestamp":2001,"execution_time":5}` + "\n\n"

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/session/sess-run/run" {
			assert.Fail(t, fmt.Sprintf("expected /session/sess-run/run, got %s", r.URL.Path))
		}

		var req RunInSessionRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Command != "echo $FOO" {
			assert.Fail(t, fmt.Sprintf("Command = %q, want echo $FOO", req.Command))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload))
	})

	var events []StreamEvent
	err := client.RunInSession(context.Background(), "sess-run", RunInSessionRequest{
		Command: "echo $FOO",
	}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	require.NoErrorf(t, err, "RunInSession")
	require.Len(t, events, 2)
	if events[0].Event != "stdout" {
		assert.Fail(t, fmt.Sprintf("event[0].Event = %q, want stdout", events[0].Event))
	}
}

func TestDeleteSession(t *testing.T) {
	var called bool
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			assert.Fail(t, fmt.Sprintf("expected DELETE, got %s", r.Method))
		}
		if r.URL.Path != "/session/sess-del" {
			assert.Fail(t, fmt.Sprintf("expected /session/sess-del, got %s", r.URL.Path))
		}
		called = true
		w.WriteHeader(http.StatusOK)
	})

	err := client.DeleteSession(context.Background(), "sess-del")
	require.NoErrorf(t, err, "DeleteSession")
	if !called {
		assert.Fail(t, "expected DELETE to be called")
	}
}

func TestGetCommandStatus(t *testing.T) {
	started := time.Now().Add(-10 * time.Second).UTC().Truncate(time.Second)
	finished := time.Now().UTC().Truncate(time.Second)
	exitCode := int32(0)
	want := CommandStatusResponse{
		ID:         "cmd-status-1",
		Content:    "hello\n",
		Running:    false,
		ExitCode:   &exitCode,
		StartedAt:  started,
		FinishedAt: &finished,
	}

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if r.URL.Path != "/command/status/cmd-status-1" {
			assert.Fail(t, fmt.Sprintf("expected /command/status/cmd-status-1, got %s", r.URL.Path))
		}
		jsonResponse(w, http.StatusOK, want)
	})

	got, err := client.GetCommandStatus(context.Background(), "cmd-status-1")
	require.NoErrorf(t, err, "GetCommandStatus")
	if got.ID != "cmd-status-1" {
		assert.Fail(t, fmt.Sprintf("ID = %q, want cmd-status-1", got.ID))
	}
	if got.Running {
		assert.Fail(t, "expected Running=false")
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		assert.Fail(t, fmt.Sprintf("ExitCode = %v, want 0", got.ExitCode))
	}
	if got.Content != "hello\n" {
		assert.Fail(t, fmt.Sprintf("Content = %q, want %q", got.Content, "hello\n"))
	}
}

func TestGetCommandStatus_Running(t *testing.T) {
	want := CommandStatusResponse{
		ID:        "cmd-running",
		Running:   true,
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, want)
	})

	got, err := client.GetCommandStatus(context.Background(), "cmd-running")
	require.NoErrorf(t, err, "GetCommandStatus")
	if !got.Running {
		assert.Fail(t, "expected Running=true")
	}
	if got.ExitCode != nil {
		assert.Fail(t, fmt.Sprintf("expected nil ExitCode for running command, got %d", *got.ExitCode))
	}
}

func TestGetCommandLogs(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if r.URL.Path != "/command/cmd-logs-1/logs" {
			assert.Fail(t, fmt.Sprintf("expected /command/cmd-logs-1/logs, got %s", r.URL.Path))
		}

		// Verify Accept header
		if r.Header.Get("Accept") != "text/plain" {
			assert.Fail(t, fmt.Sprintf("Accept = %q, want text/plain", r.Header.Get("Accept")))
		}

		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("EXECD-COMMANDS-TAIL-CURSOR", "42")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("line1\nline2\n"))
	})

	got, err := client.GetCommandLogs(context.Background(), "cmd-logs-1", nil)
	require.NoErrorf(t, err, "GetCommandLogs")
	if got.Output != "line1\nline2\n" {
		assert.Fail(t, fmt.Sprintf("Output = %q, want %q", got.Output, "line1\nline2\n"))
	}
	if got.Cursor != 42 {
		assert.Fail(t, fmt.Sprintf("Cursor = %d, want 42", got.Cursor))
	}
}

func TestGetCommandLogs_WithCursor(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		if cursor != "42" {
			assert.Fail(t, fmt.Sprintf("expected cursor=42, got %s", cursor))
		}

		w.Header().Set("EXECD-COMMANDS-TAIL-CURSOR", "99")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("line3\n"))
	})

	cursor := int64(42)
	got, err := client.GetCommandLogs(context.Background(), "cmd-logs-2", &cursor)
	require.NoErrorf(t, err, "GetCommandLogs with cursor")
	if got.Output != "line3\n" {
		assert.Fail(t, fmt.Sprintf("Output = %q, want %q", got.Output, "line3\n"))
	}
	if got.Cursor != 99 {
		assert.Fail(t, fmt.Sprintf("Cursor = %d, want 99", got.Cursor))
	}
}

func TestGetCommandLogs_WithCustomHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Header") != "logs-ok" {
			assert.Fail(t, fmt.Sprintf("X-Test-Header = %q, want %q", r.Header.Get("X-Test-Header"), "logs-ok"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("line\n"))
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "token", WithHeaders(map[string]string{
		"X-Test-Header": "logs-ok",
	}))

	got, err := client.GetCommandLogs(context.Background(), "cmd-1", nil)
	require.NoErrorf(t, err, "GetCommandLogs with custom headers")
	if got.Output != "line\n" {
		assert.Fail(t, fmt.Sprintf("Output = %q, want %q", got.Output, "line\n"))
	}
}

func TestInterruptCommand(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			assert.Fail(t, fmt.Sprintf("expected DELETE, got %s", r.Method))
		}
		if !strings.HasPrefix(r.URL.Path, "/command") {
			assert.Fail(t, fmt.Sprintf("expected /command path, got %s", r.URL.Path))
		}
		if r.URL.Query().Get("id") != "cmd-int" {
			assert.Fail(t, fmt.Sprintf("expected id=cmd-int, got %s", r.URL.Query().Get("id")))
		}
		w.WriteHeader(http.StatusOK)
	})

	err := client.InterruptCommand(context.Background(), "cmd-int")
	require.NoErrorf(t, err, "InterruptCommand")
}

func TestWatchMetrics_SSE(t *testing.T) {
	// Simulate SSE metric events (NDJSON format)
	ssePayload := strings.Join([]string{
		`{"type":"metrics","cpu_count":4,"cpu_used_pct":10.5,"mem_total_mib":8192,"mem_used_mib":2048,"timestamp":1000}`,
		"",
		`{"type":"metrics","cpu_count":4,"cpu_used_pct":15.2,"mem_total_mib":8192,"mem_used_mib":2100,"timestamp":1001}`,
		"",
		`{"type":"metrics","cpu_count":4,"cpu_used_pct":12.0,"mem_total_mib":8192,"mem_used_mib":2050,"timestamp":1002}`,
		"",
	}, "\n")

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if r.URL.Path != "/metrics/watch" {
			assert.Fail(t, fmt.Sprintf("expected /metrics/watch, got %s", r.URL.Path))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload))
	})

	var events []StreamEvent
	err := client.WatchMetrics(context.Background(), func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	require.NoErrorf(t, err, "WatchMetrics")

	require.Len(t, events, 3)
	if events[0].Event != "metrics" {
		assert.Fail(t, fmt.Sprintf("event[0].Event = %q, want metrics", events[0].Event))
	}

	// Verify we can parse the metric data from events
	var m Metrics
	if err := json.Unmarshal([]byte(events[0].Data), &m); err != nil {
		require.NoErrorf(t, err, "unmarshal metric")
	}
	if m.CPUCount != 4 {
		assert.Fail(t, fmt.Sprintf("CPUCount = %f, want 4", m.CPUCount))
	}
	if m.CPUUsedPct != 10.5 {
		assert.Fail(t, fmt.Sprintf("CPUUsedPct = %f, want 10.5", m.CPUUsedPct))
	}
}

func TestWatchMetrics_ContextCancel(t *testing.T) {
	// Use a handler that blocks until context is cancelled to verify
	// the client respects cancellation.
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		// Write one event then stall
		w.Write([]byte(`{"type":"metrics","cpu_count":2,"cpu_used_pct":5,"mem_total_mib":4096,"mem_used_mib":1024,"timestamp":1}` + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}

		// Block until request context done (client disconnects)
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())

	var eventCount int
	go func() {
		// Cancel after a short delay to let the first event arrive
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := client.WatchMetrics(ctx, func(event StreamEvent) error {
		eventCount++
		return nil
	})

	// Should get context cancelled error
	if err == nil {
		assert.Fail(t, "expected error from cancelled context")
	}
	if eventCount < 1 {
		assert.Fail(t, fmt.Sprintf("expected at least 1 event before cancel, got %d", eventCount))
	}
}

func TestExecution_ProcessStreamEvents(t *testing.T) {
	// Test the full Execution aggregation pipeline with all event types
	exec := &Execution{}
	events := []StreamEvent{
		{Event: "init", Data: `{"type":"init","text":"exec-42","timestamp":100}`},
		{Event: "stdout", Data: `{"type":"stdout","text":"hello","timestamp":101}`},
		{Event: "stderr", Data: `{"type":"stderr","text":"warn: something","timestamp":102}`},
		{Event: "result", Data: `{"type":"result","results":{"text/plain":"42"},"timestamp":103}`},
		{Event: "execution_complete", Data: `{"type":"execution_complete","timestamp":104,"execution_time":200}`},
	}

	for _, ev := range events {
		err := processStreamEvent(exec, ev, nil)
		require.NoErrorf(t, err, "processStreamEvent(%s)", ev.Event)
	}

	if exec.ID != "exec-42" {
		assert.Fail(t, fmt.Sprintf("ID = %q, want exec-42", exec.ID))
	}
	if len(exec.Stdout) != 1 || exec.Stdout[0].Text != "hello" {
		assert.Fail(t, fmt.Sprintf("Stdout = %+v, want [hello]", exec.Stdout))
	}
	if len(exec.Stderr) != 1 || exec.Stderr[0].Text != "warn: something" {
		assert.Fail(t, fmt.Sprintf("Stderr = %+v", exec.Stderr))
	}
	if len(exec.Results) != 1 || exec.Results[0].Text() != "42" {
		assert.Fail(t, fmt.Sprintf("Results = %+v", exec.Results))
	}
	if exec.Complete == nil {
		assert.Fail(t, "expected Complete to be set")
	}
	if exec.ExitCode == nil || *exec.ExitCode != 0 {
		assert.Fail(t, fmt.Sprintf("ExitCode = %v, want 0", exec.ExitCode))
	}
	if exec.Text() != "hello" {
		assert.Fail(t, fmt.Sprintf("Text() = %q, want hello", exec.Text()))
	}
}

func TestExecution_ErrorEvent(t *testing.T) {
	exec := &Execution{}
	event := StreamEvent{
		Event: "error",
		Data:  `{"type":"error","error":{"ename":"NameError","evalue":"name 'x' is not defined","traceback":["line 1"]}}`,
	}

	if err := processStreamEvent(exec, event, nil); err != nil {
		require.NoErrorf(t, err, "processStreamEvent")
	}

	if exec.Error == nil {
		require.FailNow(t, "expected Error to be set")
	}
	if exec.Error.Name != "NameError" {
		assert.Fail(t, fmt.Sprintf("Error.Name = %q, want NameError", exec.Error.Name))
	}
	if exec.Error.Value != "name 'x' is not defined" {
		assert.Fail(t, fmt.Sprintf("Error.Value = %q", exec.Error.Value))
	}
	if len(exec.Error.Traceback) != 1 {
		assert.Fail(t, fmt.Sprintf("Traceback len = %d, want 1", len(exec.Error.Traceback)))
	}
}

func TestExecution_HandlersInvoked(t *testing.T) {
	exec := &Execution{}
	var initCalled, stdoutCalled, stderrCalled, resultCalled, completeCalled bool

	handlers := &ExecutionHandlers{
		OnInit:     func(e ExecutionInit) error { initCalled = true; return nil },
		OnStdout:   func(m OutputMessage) error { stdoutCalled = true; return nil },
		OnStderr:   func(m OutputMessage) error { stderrCalled = true; return nil },
		OnResult:   func(r ExecutionResult) error { resultCalled = true; return nil },
		OnComplete: func(c ExecutionComplete) error { completeCalled = true; return nil },
	}

	events := []StreamEvent{
		{Data: `{"type":"init","text":"x","timestamp":1}`},
		{Data: `{"type":"stdout","text":"out","timestamp":2}`},
		{Data: `{"type":"stderr","text":"err","timestamp":3}`},
		{Data: `{"type":"result","results":{"text/plain":"ok"},"timestamp":4}`},
		{Data: `{"type":"execution_complete","timestamp":5,"execution_time":100}`},
	}

	for _, ev := range events {
		if err := processStreamEvent(exec, ev, handlers); err != nil {
			require.NoErrorf(t, err, "processStreamEvent")
		}
	}

	if !initCalled {
		assert.Fail(t, "OnInit not called")
	}
	if !stdoutCalled {
		assert.Fail(t, "OnStdout not called")
	}
	if !stderrCalled {
		assert.Fail(t, "OnStderr not called")
	}
	if !resultCalled {
		assert.Fail(t, "OnResult not called")
	}
	if !completeCalled {
		assert.Fail(t, "OnComplete not called")
	}
}

func TestOctalMode(t *testing.T) {
	tests := []struct {
		mode os.FileMode
		want int
	}{
		{0755, 755},
		{0644, 644},
		{0700, 700},
		{0777, 777},
	}
	for _, tc := range tests {
		got := OctalMode(tc.mode)
		if got != tc.want {
			assert.Fail(t, fmt.Sprintf("OctalMode(%o) = %d, want %d", tc.mode, got, tc.want))
		}
	}
}

func TestStreamSSE_HandlerError(t *testing.T) {
	ssePayload := "event: first\ndata: a\n\nevent: second\ndata: b\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload))
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "tok")
	stopErr := fmt.Errorf("stop after first")

	var count int
	err := client.RunCommand(context.Background(), RunCommandRequest{Command: "x"}, func(event StreamEvent) error {
		count++
		if count == 1 {
			return stopErr
		}
		return nil
	})
	if err != stopErr {
		assert.Fail(t, fmt.Sprintf("expected stopErr, got %v", err))
	}
	if count != 1 {
		assert.Fail(t, fmt.Sprintf("handler called %d times, want 1", count))
	}
}

func TestRunCommand_WithEnvs(t *testing.T) {
	ssePayload := `{"type":"stdout","text":"bar","timestamp":1000}` + "\n\n" +
		`{"type":"execution_complete","timestamp":1001,"execution_time":5}` + "\n\n"

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req RunCommandRequest
		json.NewDecoder(r.Body).Decode(&req)

		if req.Envs == nil {
			require.FailNow(t, "expected Envs to be set")
		}
		if req.Envs["FOO"] != "bar" {
			assert.Fail(t, fmt.Sprintf("Envs[FOO] = %q, want bar", req.Envs["FOO"]))
		}
		if req.Envs["BAZ"] != "qux" {
			assert.Fail(t, fmt.Sprintf("Envs[BAZ] = %q, want qux", req.Envs["BAZ"]))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload))
	})

	err := client.RunCommand(context.Background(), RunCommandRequest{
		Command: "echo $FOO",
		Envs:    map[string]string{"FOO": "bar", "BAZ": "qux"},
	}, func(event StreamEvent) error { return nil })
	require.NoErrorf(t, err, "RunCommand with Envs")
}

func TestRunCommand_Background(t *testing.T) {
	ssePayload := `{"type":"init","text":"cmd-bg-123","timestamp":1000}` + "\n\n" +
		`{"type":"execution_complete","timestamp":1001,"execution_time":0}` + "\n\n"

	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req RunCommandRequest
		json.NewDecoder(r.Body).Decode(&req)

		if !req.Background {
			assert.Fail(t, "expected Background=true")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload))
	})

	var events []StreamEvent
	err := client.RunCommand(context.Background(), RunCommandRequest{
		Command:    "sleep 30",
		Background: true,
	}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	require.NoErrorf(t, err, "RunCommand background")
	require.Len(t, events, 2)
}

func TestAPIError_RequestID(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req-abc-123")
		jsonResponse(w, http.StatusNotFound, ErrorResponse{
			Code:    "SANDBOX_NOT_FOUND",
			Message: "not found",
		})
	})

	_, err := client.GetSandbox(context.Background(), "sbx-missing")
	require.Error(t, err)

	apiErr, ok := err.(*APIError)
	require.True(t, ok, "expected *APIError, got %T", err)
	if apiErr.RequestID != "req-abc-123" {
		assert.Fail(t, fmt.Sprintf("RequestID = %q, want req-abc-123", apiErr.RequestID))
	}
	if !strings.Contains(apiErr.Error(), "req-abc-123") {
		assert.Fail(t, fmt.Sprintf("Error() = %q, expected to contain request ID", apiErr.Error()))
	}
}

func TestCreateSandbox_WithNetworkPolicy(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req CreateSandboxRequest
		json.NewDecoder(r.Body).Decode(&req)

		if req.NetworkPolicy == nil {
			require.FailNow(t, "expected NetworkPolicy to be set")
		}
		if req.NetworkPolicy.DefaultAction != "deny" {
			assert.Fail(t, fmt.Sprintf("DefaultAction = %q, want deny", req.NetworkPolicy.DefaultAction))
		}
		require.Len(t, req.NetworkPolicy.Egress, 1)
		if req.NetworkPolicy.Egress[0].Target != "api.example.com" {
			assert.Fail(t, fmt.Sprintf("Target = %q, want api.example.com", req.NetworkPolicy.Egress[0].Target))
		}

		jsonResponse(w, http.StatusCreated, SandboxInfo{
			ID:        "sbx-policy",
			Status:    SandboxStatus{State: StatePending},
			CreatedAt: time.Now().UTC().Truncate(time.Second),
		})
	})

	_, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		Image:          &ImageSpec{URI: "python:3.12"},
		Entrypoint:     []string{"/bin/sh"},
		ResourceLimits: ResourceLimits{"cpu": "500m"},
		NetworkPolicy: &NetworkPolicy{
			DefaultAction: "deny",
			Egress: []NetworkRule{
				{Action: "allow", Target: "api.example.com"},
			},
		},
	})
	require.NoErrorf(t, err, "CreateSandbox with NetworkPolicy")
}

func TestCreateSandbox_WithVolumes(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req CreateSandboxRequest
		json.NewDecoder(r.Body).Decode(&req)

		require.Len(t, req.Volumes, 2)

		// Host volume
		v0 := req.Volumes[0]
		if v0.Name != "data" {
			assert.Fail(t, fmt.Sprintf("Volume[0].Name = %q, want data", v0.Name))
		}
		if v0.Host == nil || v0.Host.Path != "/host/data" {
			assert.Fail(t, fmt.Sprintf("Volume[0].Host = %+v, want /host/data", v0.Host))
		}
		if v0.MountPath != "/mnt/data" {
			assert.Fail(t, fmt.Sprintf("Volume[0].MountPath = %q, want /mnt/data", v0.MountPath))
		}
		if v0.ReadOnly {
			assert.Fail(t, "Volume[0] should not be ReadOnly")
		}

		// PVC volume with subPath and readOnly
		v1 := req.Volumes[1]
		if v1.PVC == nil || v1.PVC.ClaimName != "my-pvc" {
			assert.Fail(t, fmt.Sprintf("Volume[1].PVC = %+v, want my-pvc", v1.PVC))
		}
		if !v1.ReadOnly {
			assert.Fail(t, "Volume[1] should be ReadOnly")
		}
		if v1.SubPath != "subdir" {
			assert.Fail(t, fmt.Sprintf("Volume[1].SubPath = %q, want subdir", v1.SubPath))
		}

		jsonResponse(w, http.StatusCreated, SandboxInfo{
			ID:        "sbx-vols",
			Status:    SandboxStatus{State: StatePending},
			CreatedAt: time.Now().UTC().Truncate(time.Second),
		})
	})

	_, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		Image:          &ImageSpec{URI: "python:3.12"},
		Entrypoint:     []string{"/bin/sh"},
		ResourceLimits: ResourceLimits{"cpu": "500m"},
		Volumes: []Volume{
			{
				Name:      "data",
				Host:      &Host{Path: "/host/data"},
				MountPath: "/mnt/data",
			},
			{
				Name:      "pvc-vol",
				PVC:       &PVC{ClaimName: "my-pvc"},
				MountPath: "/mnt/pvc",
				ReadOnly:  true,
				SubPath:   "subdir",
			},
		},
	})
	require.NoErrorf(t, err, "CreateSandbox with Volumes")
}
