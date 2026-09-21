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

package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/binding"
	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func newMiddlewareTestRouter(t *testing.T, legacyToken string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Same order as NewRouter: the init gate runs before auth.
	r.Use(runtimeInitGate(), accessTokenMiddleware(legacyToken))
	r.GET("/ping", okHandler)
	r.GET("/ready", okHandler)
	r.POST("/internal/init", okHandler)
	r.GET("/api", okHandler)
	return r
}

func okHandler(ctx *gin.Context) {
	ctx.Status(http.StatusOK)
}

func doRequest(t *testing.T, r *gin.Engine, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set(model.ApiAccessTokenHeader, token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// withTestBinding installs a binding and restores the previous state.
func withTestBinding(t *testing.T, b *binding.RuntimeBinding) {
	t.Helper()
	previous := binding.Apply(b)
	t.Cleanup(func() { binding.Apply(previous) })
}

// withRuntimeInit toggles the runtime-init gate and restores it.
func withRuntimeInit(t *testing.T, enabled bool) {
	t.Helper()
	previous := flag.RuntimeInit
	flag.RuntimeInit = enabled
	t.Cleanup(func() { flag.RuntimeInit = previous })
}

func TestAccessTokenLegacyTokenBeforeInit(t *testing.T) {
	withTestBinding(t, nil)
	r := newMiddlewareTestRouter(t, "legacy-token")

	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/api", "legacy-token").Code)
	require.Equal(t, http.StatusUnauthorized, doRequest(t, r, http.MethodGet, "/api", "").Code)
	require.Equal(t, http.StatusUnauthorized, doRequest(t, r, http.MethodGet, "/api", "wrong").Code)
}

func TestAccessTokenBindingHashIsAuthoritative(t *testing.T) {
	withTestBinding(t, &binding.RuntimeBinding{
		SandboxID:       "sandbox-1",
		Generation:      1,
		HasAccessToken:  true,
		AccessTokenHash: mustHash(t, "rotated-token"),
	})
	r := newMiddlewareTestRouter(t, "legacy-token")

	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/api", "rotated-token").Code)
	// The legacy container-env token is no longer accepted once /internal/init
	// provided a token hash (/internal/init is the authoritative source).
	require.Equal(t, http.StatusUnauthorized, doRequest(t, r, http.MethodGet, "/api", "legacy-token").Code)
	require.Equal(t, http.StatusUnauthorized, doRequest(t, r, http.MethodGet, "/api", "").Code)
}

func TestAccessTokenPreInitPathsSkipToken(t *testing.T) {
	withTestBinding(t, &binding.RuntimeBinding{
		HasAccessToken:  true,
		AccessTokenHash: mustHash(t, "rotated-token"),
	})
	r := newMiddlewareTestRouter(t, "legacy-token")

	// /ping, /ready, /internal/init stay reachable without the API token.
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/ping", "").Code)
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/ready", "").Code)
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodPost, "/internal/init", "").Code)
}

// withManager installs a fresh runtime-init manager with the given ready
// state (each call replaces the process-wide manager).
func withManager(t *testing.T, ready bool) {
	t.Helper()
	manager := controller.InitRuntimeInitManager(&controller.RuntimeInitConfig{})
	if ready {
		manager.MarkReady()
	}
}

func TestRuntimeInitGateBlocksUninitializedAPIs(t *testing.T) {
	withRuntimeInit(t, true)
	withTestBinding(t, nil)
	withManager(t, false)
	r := newMiddlewareTestRouter(t, "")

	// Business APIs are unavailable before /internal/init...
	require.Equal(t, http.StatusServiceUnavailable, doRequest(t, r, http.MethodGet, "/api", "").Code)
	// ...while liveness, readiness, and init stay reachable.
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/ping", "").Code)
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/ready", "").Code)
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodPost, "/internal/init", "").Code)
}

func TestRuntimeInitGateChecksReadinessNotBinding(t *testing.T) {
	withRuntimeInit(t, true)
	// A binding installed by a half-completed apply (preStart/entrypoint
	// returned 500) must NOT open the business APIs while /ready is 503.
	withTestBinding(t, &binding.RuntimeBinding{SandboxID: "sandbox-1", Generation: 1})
	withManager(t, false)
	r := newMiddlewareTestRouter(t, "")

	require.Equal(t, http.StatusServiceUnavailable, doRequest(t, r, http.MethodGet, "/api", "").Code)
}

func TestRuntimeInitGateOpenAfterInit(t *testing.T) {
	withRuntimeInit(t, true)
	withManager(t, true)
	r := newMiddlewareTestRouter(t, "")

	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/api", "").Code)
}

func TestRuntimeInitGateDisabledByDefault(t *testing.T) {
	withRuntimeInit(t, false)
	withTestBinding(t, nil)
	r := newMiddlewareTestRouter(t, "")

	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/api", "").Code)
}

func TestNewRouterServesInitRoutes(t *testing.T) {
	withTestBinding(t, nil)
	withRuntimeInit(t, false)
	withManager(t, false)
	r := NewRouter("")

	// /ping keeps working.
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/ping", "").Code)

	// /ready reports uninitialized.
	w := doRequest(t, r, http.MethodGet, "/ready", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), `"initialized":false`)

	// /internal/init routes through to the handler: an invalid payload
	// fails validation (400) instead of a routing error (404), and the
	// failed request does not initialize the runtime.
	w = doRequest(t, r, http.MethodPost, "/internal/init", `{"sandboxId":"s","generation":0}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func mustHash(t *testing.T, raw string) [32]byte {
	t.Helper()
	digest, err := binding.ParseAccessTokenHash(binding.HashAccessToken(raw))
	require.NoError(t, err)
	return digest
}
