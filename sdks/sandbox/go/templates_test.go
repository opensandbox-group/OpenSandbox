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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCreateTemplate(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	want := TemplateInfo{
		TemplateID: "tpl-abc",
		Image:      "alpine:3.19",
		Publish:    "s3://bucket/publish",
		Format:     TemplateFormatOverlayBD,
		Status:     TemplateStatus{Phase: TemplatePhasePending},
		CreatedAt:  now,
		UpdatedAt:  now,
		Env:        map[string]string{"LOG_LEVEL": "info"},
	}

	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Fail(t, fmt.Sprintf("expected POST, got %s", r.Method))
		}
		if r.URL.Path != "/templates" {
			assert.Fail(t, fmt.Sprintf("expected /templates, got %s", r.URL.Path))
		}

		var req CreateTemplateRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Image != "alpine:3.19" {
			assert.Fail(t, fmt.Sprintf("expected image alpine:3.19, got %q", req.Image))
		}
		if req.Publish != "s3://bucket/publish" {
			assert.Fail(t, fmt.Sprintf("expected publish s3://bucket/publish, got %q", req.Publish))
		}
		if req.ResourceLimits["cpu"] != "1" {
			assert.Fail(t, fmt.Sprintf("expected resourceLimits cpu=1, got %v", req.ResourceLimits))
		}
		if req.Env["LOG_LEVEL"] != "info" {
			assert.Fail(t, fmt.Sprintf("expected env LOG_LEVEL=info, got %v", req.Env))
		}
		if req.Readiness == nil || req.Readiness.Probe != "tcp://127.0.0.1:44772" {
			assert.Fail(t, fmt.Sprintf("unexpected readiness: %+v", req.Readiness))
		}

		jsonResponse(w, http.StatusCreated, want)
	})

	got, err := client.CreateTemplate(context.Background(), CreateTemplateRequest{
		Image:          "alpine:3.19",
		Publish:        "s3://bucket/publish",
		ResourceLimits: ResourceLimits{"cpu": "1", "memory": "512Mi", "disk": "2Gi"},
		Env:            map[string]string{"LOG_LEVEL": "info"},
		Readiness:      &TemplateReadiness{Probe: "tcp://127.0.0.1:44772"},
	})
	require.NoErrorf(t, err, "CreateTemplate")
	if got.TemplateID != want.TemplateID {
		assert.Fail(t, fmt.Sprintf("TemplateID = %q, want %q", got.TemplateID, want.TemplateID))
	}
	if got.Status.Phase != TemplatePhasePending {
		assert.Fail(t, fmt.Sprintf("Phase = %q, want %q", got.Status.Phase, TemplatePhasePending))
	}
	if got.Env["LOG_LEVEL"] != "info" {
		assert.Fail(t, fmt.Sprintf("Env = %v, want LOG_LEVEL=info", got.Env))
	}
	if got.CreatedAt.UTC() != now {
		assert.Fail(t, fmt.Sprintf("CreatedAt = %v, want %v", got.CreatedAt, now))
	}
}

func TestGetTemplate(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if r.URL.Path != "/templates/tpl-abc" {
			assert.Fail(t, fmt.Sprintf("expected /templates/tpl-abc, got %s", r.URL.Path))
		}
		jsonResponse(w, http.StatusOK, TemplateInfo{
			TemplateID: "tpl-abc",
			Image:      "alpine:3.19",
			Publish:    "s3://bucket/publish",
			Format:     TemplateFormatNative,
			Status:     TemplateStatus{Phase: TemplatePhaseSucceeded, ManifestRef: "s3://bucket/manifest"},
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
		})
	})

	got, err := client.GetTemplate(context.Background(), "tpl-abc")
	require.NoErrorf(t, err, "GetTemplate")
	if got.Status.Phase != TemplatePhaseSucceeded {
		assert.Fail(t, fmt.Sprintf("Phase = %q, want %q", got.Status.Phase, TemplatePhaseSucceeded))
	}
	if got.Status.ManifestRef != "s3://bucket/manifest" {
		assert.Fail(t, fmt.Sprintf("ManifestRef = %q, want %q", got.Status.ManifestRef, "s3://bucket/manifest"))
	}
}

func TestListTemplates(t *testing.T) {
	want := ListTemplatesResponse{
		Items: []TemplateInfo{{
			TemplateID: "tpl-abc",
			Image:      "alpine:3.19",
			Publish:    "s3://bucket/publish",
			Format:     TemplateFormatOverlayBD,
			Status:     TemplateStatus{Phase: TemplatePhaseBuilding},
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
		}},
		Pagination: PaginationInfo{Page: 1, PageSize: 20, TotalItems: 1, TotalPages: 1},
	}

	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			assert.Fail(t, fmt.Sprintf("expected GET, got %s", r.Method))
		}
		if r.URL.Path != "/templates" {
			assert.Fail(t, fmt.Sprintf("expected /templates, got %s", r.URL.Path))
		}

		// The server URL-decodes the metadata parameter once and then splits
		// it with parse_qsl semantics; keys and values containing &, = or %
		// must round-trip.
		raw := r.URL.Query().Get("metadata")
		pairs, err := url.ParseQuery(raw)
		if err != nil {
			assert.Fail(t, fmt.Sprintf("metadata %q is not parse_qsl-decodable: %v", raw, err))
		}
		if pairs.Get("a&b") != "x=y" {
			assert.Fail(t, fmt.Sprintf("expected a&b=x=y, got %q (raw %q)", pairs.Get("a&b"), raw))
		}
		if pairs.Get("p%q") != "1&2" {
			assert.Fail(t, fmt.Sprintf("expected p%%q=1&2, got %q (raw %q)", pairs.Get("p%q"), raw))
		}
		if q := r.URL.Query(); q.Get("page") != "1" || q.Get("pageSize") != "20" {
			assert.Fail(t, fmt.Sprintf("unexpected pagination: page=%s pageSize=%s", q.Get("page"), q.Get("pageSize")))
		}

		jsonResponse(w, http.StatusOK, want)
	})

	got, err := client.ListTemplates(context.Background(), ListTemplatesOptions{
		Metadata: map[string]string{"a&b": "x=y", "p%q": "1&2"},
		Page:     1,
		PageSize: 20,
	})
	require.NoErrorf(t, err, "ListTemplates")
	if len(got.Items) != 1 || got.Items[0].TemplateID != "tpl-abc" {
		assert.Fail(t, fmt.Sprintf("unexpected items: %+v", got.Items))
	}
}

func TestDeleteTemplate(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			assert.Fail(t, fmt.Sprintf("expected DELETE, got %s", r.Method))
		}
		if r.URL.Path != "/templates/tpl-abc" {
			assert.Fail(t, fmt.Sprintf("expected /templates/tpl-abc, got %s", r.URL.Path))
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.DeleteTemplate(context.Background(), "tpl-abc"); err != nil {
		assert.Fail(t, fmt.Sprintf("DeleteTemplate: %v", err))
	}
}

func TestSandboxManager_TemplateCRUD(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/templates":
			jsonResponse(w, http.StatusCreated, TemplateInfo{
				TemplateID: "tpl-abc",
				Image:      "alpine:3.19",
				Publish:    "s3://bucket/publish",
				Format:     TemplateFormatOverlayBD,
				Status:     TemplateStatus{Phase: TemplatePhasePending},
				CreatedAt:  now,
				UpdatedAt:  now,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/templates/tpl-abc":
			jsonResponse(w, http.StatusOK, TemplateInfo{
				TemplateID: "tpl-abc",
				Image:      "alpine:3.19",
				Publish:    "s3://bucket/publish",
				Format:     TemplateFormatOverlayBD,
				Status:     TemplateStatus{Phase: TemplatePhaseSucceeded},
				CreatedAt:  now,
				UpdatedAt:  now,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/templates":
			jsonResponse(w, http.StatusOK, ListTemplatesResponse{
				Items:      []TemplateInfo{},
				Pagination: PaginationInfo{Page: 1, PageSize: 20},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/templates/tpl-abc":
			w.WriteHeader(http.StatusNoContent)
		default:
			assert.Fail(t, fmt.Sprintf("unexpected request %s %s", r.Method, r.URL.Path))
		}
	})

	mgr := &SandboxManager{lifecycle: client}
	ctx := context.Background()

	created, err := mgr.CreateTemplate(ctx, CreateTemplateRequest{Image: "alpine:3.19", Publish: "s3://bucket/publish"})
	require.NoErrorf(t, err, "CreateTemplate")
	if created.Status.Phase != TemplatePhasePending {
		assert.Fail(t, fmt.Sprintf("Phase = %q, want %q", created.Status.Phase, TemplatePhasePending))
	}

	got, err := mgr.GetTemplate(ctx, "tpl-abc")
	require.NoErrorf(t, err, "GetTemplate")
	if got.Status.Phase != TemplatePhaseSucceeded {
		assert.Fail(t, fmt.Sprintf("Phase = %q, want %q", got.Status.Phase, TemplatePhaseSucceeded))
	}

	listed, err := mgr.ListTemplates(ctx, ListTemplatesOptions{})
	require.NoErrorf(t, err, "ListTemplates")
	if listed.Items == nil {
		assert.Fail(t, "expected non-nil items")
	}

	require.NoErrorf(t, mgr.DeleteTemplate(ctx, "tpl-abc"), "DeleteTemplate")
}

func TestEndpoint_OriginHeaderCaptured(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/endpoints/") {
			assert.Fail(t, fmt.Sprintf("expected an endpoint lookup, got %s", r.URL.Path))
		}
		w.Header().Set(SandboxOriginHeader, string(SandboxOriginTemplate))
		jsonResponse(w, http.StatusOK, Endpoint{Endpoint: "http://127.0.0.1:8080"})
	})

	got, err := client.getEndpointFromServer(context.Background(), "fsb-1", DefaultExecdPort, nil)
	require.NoErrorf(t, err, "getEndpointFromServer")
	if got.Origin != SandboxOriginTemplate {
		assert.Fail(t, fmt.Sprintf("Origin = %q, want %q", got.Origin, SandboxOriginTemplate))
	}

	signed, err := client.GetSignedEndpoint(context.Background(), "fsb-1", DefaultExecdPort, 12345)
	require.NoErrorf(t, err, "GetSignedEndpoint")
	if signed.Origin != SandboxOriginTemplate {
		assert.Fail(t, fmt.Sprintf("Origin = %q, want %q", signed.Origin, SandboxOriginTemplate))
	}
}

func TestEndpoint_OriginPreservedThroughEndpointCache(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/endpoints/") {
			assert.Fail(t, fmt.Sprintf("expected an endpoint lookup, got %s", r.URL.Path))
		}
		w.Header().Set(SandboxOriginHeader, string(SandboxOriginTemplate))
		jsonResponse(w, http.StatusOK, Endpoint{Endpoint: "http://127.0.0.1:8080"})
	})

	// First lookup goes through the cache's GetOrFetch, which clones even
	// the freshly fetched result; cached lookups clone the stored entry.
	// Both paths must preserve Origin.
	first, err := client.GetEndpoint(context.Background(), "fsb-1", DefaultExecdPort, nil)
	require.NoErrorf(t, err, "GetEndpoint (fresh)")
	if first.Origin != SandboxOriginTemplate {
		assert.Fail(t, fmt.Sprintf("Origin = %q, want %q", first.Origin, SandboxOriginTemplate))
	}

	cached, err := client.GetEndpoint(context.Background(), "fsb-1", DefaultExecdPort, nil)
	require.NoErrorf(t, err, "GetEndpoint (cached)")
	if cached.Origin != SandboxOriginTemplate {
		assert.Fail(t, fmt.Sprintf("Origin = %q, want %q", cached.Origin, SandboxOriginTemplate))
	}
}

func TestCreateSandboxFromTemplate(t *testing.T) {
	var mu sync.Mutex
	var received *CreateSandboxRequest
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			var req CreateSandboxRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			mu.Lock()
			received = &req
			mu.Unlock()
			jsonResponse(w, http.StatusCreated, SandboxInfo{
				ID:        "fsb-xyz",
				Status:    SandboxStatus{State: StateRunning},
				CreatedAt: time.Now().UTC(),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/fsb-xyz":
			jsonResponse(w, http.StatusOK, SandboxInfo{
				ID:        "fsb-xyz",
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

	sb, err := CreateSandboxFromTemplate(context.Background(), ConnectionConfig{
		Domain:         srv.URL,
		DisableMetrics: true,
	}, "tpl-abc", SandboxFromTemplateOptions{
		TimeoutSeconds:  120,
		Metadata:        map[string]string{"team": "backend"},
		NetworkPolicy:   &NetworkPolicy{DefaultAction: "deny", Egress: []NetworkRule{{Action: "allow", Target: "api.example.com"}}},
		Extensions:      map[string]string{"storage.id": "snap-1"},
		SkipHealthCheck: true,
	})
	require.NoErrorf(t, err, "CreateSandboxFromTemplate")
	if sb.ID() != "fsb-xyz" {
		assert.Fail(t, fmt.Sprintf("ID = %q, want %q", sb.ID(), "fsb-xyz"))
	}
	if sb.Origin() != SandboxOriginTemplate {
		assert.Fail(t, fmt.Sprintf("Origin = %q, want %q", sb.Origin(), SandboxOriginTemplate))
	}

	mu.Lock()
	defer mu.Unlock()
	if received == nil {
		require.FailNow(t, "expected a create request")
	}
	if received.TemplateID != "tpl-abc" {
		assert.Fail(t, fmt.Sprintf("TemplateID = %q, want %q", received.TemplateID, "tpl-abc"))
	}
	if received.Timeout == nil || *received.Timeout != 120 {
		assert.Fail(t, fmt.Sprintf("expected timeout 120, got %v", received.Timeout))
	}
	if received.Metadata["team"] != "backend" {
		assert.Fail(t, fmt.Sprintf("expected metadata team=backend, got %v", received.Metadata))
	}
	if received.NetworkPolicy == nil || len(received.NetworkPolicy.Egress) != 1 || received.NetworkPolicy.Egress[0].Target != "api.example.com" {
		assert.Fail(t, fmt.Sprintf("unexpected networkPolicy: %+v", received.NetworkPolicy))
	}
	if received.Extensions["storage.id"] != "snap-1" {
		assert.Fail(t, fmt.Sprintf("expected extensions storage.id=snap-1, got %v", received.Extensions))
	}
	// Template mode fixes the workload shape server-side: workload-shaping
	// fields must be omitted from the request.
	if received.Image != nil || received.SnapshotID != "" || received.Entrypoint != nil ||
		received.ResourceLimits != nil || received.ResourceRequests != nil || received.Env != nil ||
		received.Lifecycle != nil || received.Volumes != nil || received.Platform != nil ||
		received.CredentialProxy != nil || received.SecureAccess {
		assert.Fail(t, fmt.Sprintf("template-mode request must omit workload-shaping fields, got %+v", *received))
	}
}

func TestCreateSandboxFromTemplate_Validation(t *testing.T) {
	var invalid *InvalidArgumentError
	_, err := CreateSandboxFromTemplate(context.Background(), ConnectionConfig{}, "", SandboxFromTemplateOptions{TimeoutSeconds: 60})
	require.ErrorAs(t, err, &invalid, "blank template ID")

	_, err = CreateSandboxFromTemplate(context.Background(), ConnectionConfig{}, "tpl-abc", SandboxFromTemplateOptions{})
	require.ErrorAs(t, err, &invalid, "missing timeout")
}

func TestSandbox_TemplateBacked_EgressUsesLifecycleControlPlane(t *testing.T) {
	var calls []string
	var mu sync.Mutex
	record := func(s string) {
		mu.Lock()
		calls = append(calls, s)
		mu.Unlock()
	}

	lifecycleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/fsb-1/networkpolicy":
			record("get")
			jsonResponse(w, http.StatusOK, PolicyStatusResponse{
				Status: "ok",
				Policy: &NetworkPolicy{DefaultAction: "deny"},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/sandboxes/fsb-1/networkpolicy":
			var rules []NetworkRule
			require.NoError(t, json.NewDecoder(r.Body).Decode(&rules))
			if len(rules) != 1 || rules[0].Target != "api.example.com" || rules[0].Action != "allow" {
				assert.Fail(t, fmt.Sprintf("unexpected patch rules: %+v", rules))
			}
			record("patch")
			jsonResponse(w, http.StatusOK, PolicyStatusResponse{Status: "ok"})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/fsb-1/networkpolicy":
			var targets []string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&targets))
			if len(targets) != 1 || targets[0] != "api.example.com" {
				assert.Fail(t, fmt.Sprintf("unexpected delete targets: %+v", targets))
			}
			record("delete")
			jsonResponse(w, http.StatusOK, PolicyStatusResponse{Status: "ok"})
		default:
			// Any endpoint lookup means the SDK tried to resolve an egress
			// sidecar, which template-backed sandboxes must not do.
			record("UNEXPECTED " + r.Method + " " + r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer lifecycleSrv.Close()

	config := ConnectionConfig{Domain: lifecycleSrv.URL, DisableMetrics: true}
	sb := &Sandbox{
		id:        "fsb-1",
		config:    &config,
		lifecycle: config.lifecycleClient(),
		origin:    SandboxOriginTemplate,
	}

	got, err := sb.GetEgressPolicy(context.Background())
	require.NoErrorf(t, err, "GetEgressPolicy")
	if got.Policy == nil || got.Policy.DefaultAction != "deny" {
		assert.Fail(t, fmt.Sprintf("unexpected policy: %+v", got.Policy))
	}

	_, err = sb.PatchEgressRules(context.Background(), []NetworkRule{{Action: "allow", Target: "api.example.com"}})
	require.NoErrorf(t, err, "PatchEgressRules")

	_, err = sb.DeleteEgressRules(context.Background(), []string{"api.example.com"})
	require.NoErrorf(t, err, "DeleteEgressRules")

	mu.Lock()
	defer mu.Unlock()
	for _, c := range calls {
		if strings.HasPrefix(c, "UNEXPECTED") {
			assert.Fail(t, fmt.Sprintf("template-backed sandbox must not resolve the egress sidecar, got %s", c))
		}
	}
	if len(calls) != 3 {
		assert.Fail(t, fmt.Sprintf("expected 3 lifecycle networkpolicy calls, got %v", calls))
	}
}

func TestSandbox_TemplateBacked_CredentialVaultUnavailable(t *testing.T) {
	config := ConnectionConfig{Domain: "localhost:8080", DisableMetrics: true}
	sb := &Sandbox{
		id:        "fsb-1",
		config:    &config,
		lifecycle: config.lifecycleClient(),
		origin:    SandboxOriginTemplate,
	}

	_, err := sb.CredentialVault(context.Background())
	if err == nil {
		assert.Fail(t, "expected CredentialVault to fail for template-backed sandboxes")
	} else if !strings.Contains(err.Error(), "template-backed") {
		assert.Fail(t, fmt.Sprintf("expected a template-backed error, got %v", err))
	}

	if _, err := sb.CreateCredentialVault(context.Background(), CredentialVaultCreateRequest{}); err == nil {
		assert.Fail(t, "expected CreateCredentialVault to fail for template-backed sandboxes")
	}
}

func TestConnectSandbox_OriginAutoDetection(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/endpoints/") {
			w.Header().Set(SandboxOriginHeader, string(SandboxOriginTemplate))
			jsonResponse(w, http.StatusOK, Endpoint{Endpoint: srv.URL})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	sb, err := ConnectSandbox(context.Background(), ConnectionConfig{
		Domain:         srv.URL,
		DisableMetrics: true,
	}, "fsb-xyz")
	require.NoErrorf(t, err, "ConnectSandbox")
	if sb.Origin() != SandboxOriginTemplate {
		assert.Fail(t, fmt.Sprintf("Origin = %q, want %q", sb.Origin(), SandboxOriginTemplate))
	}
}

func TestConnectSandbox_OriginUnknownWithoutHeader(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/endpoints/") {
			jsonResponse(w, http.StatusOK, Endpoint{Endpoint: srv.URL})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	sb, err := ConnectSandbox(context.Background(), ConnectionConfig{
		Domain:         srv.URL,
		DisableMetrics: true,
	}, "sbx-plain")
	require.NoErrorf(t, err, "ConnectSandbox")
	if sb.Origin() != SandboxOriginUnknown {
		assert.Fail(t, fmt.Sprintf("Origin = %q, want %q", sb.Origin(), SandboxOriginUnknown))
	}
}

// TestSandbox_SidecarEgressUntouchedForImageOrigin verifies the backward
// compatible path: a sandbox without a template origin still resolves and uses
// the egress sidecar.
func TestSandbox_SidecarEgressUntouchedForImageOrigin(t *testing.T) {
	egressSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/policy" {
			jsonResponse(w, http.StatusOK, PolicyStatusResponse{Status: "ok"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer egressSrv.Close()

	lifecycleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/endpoints/18080") {
			jsonResponse(w, http.StatusOK, Endpoint{Endpoint: egressSrv.URL})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer lifecycleSrv.Close()

	config := ConnectionConfig{Domain: lifecycleSrv.URL, DisableMetrics: true}
	sb := &Sandbox{
		id:        "sbx-plain",
		config:    &config,
		lifecycle: config.lifecycleClient(),
		origin:    SandboxOriginUnknown,
	}

	_, err := sb.PatchEgressRules(context.Background(), []NetworkRule{{Action: "allow", Target: "api.example.com"}})
	require.NoErrorf(t, err, "PatchEgressRules")
}
