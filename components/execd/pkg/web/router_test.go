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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/binding"
	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func TestHTTPMetricsExcludeWatchAndUseRouteTemplates(t *testing.T) {
	counts := make(map[string]uint64)
	r := gin.New()
	r.Use(httpMetricsMiddleware(func(_ context.Context, method, route string, status int, duration float64) {
		require.GreaterOrEqual(t, duration, 0.0)
		counts[fmt.Sprintf("%s %s %d", method, route, status)]++
	}))
	r.GET("/metrics/watch", okHandler)
	r.GET("/files/:id", func(c *gin.Context) {
		if c.Param("id") == "bad" {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusOK)
	})
	r.POST("/code", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.String(http.StatusOK, "data: complete\n\n")
	})
	doRequest(t, r, http.MethodGet, "/metrics/watch", "")
	doRequest(t, r, http.MethodGet, "/files/one", "")
	doRequest(t, r, http.MethodGet, "/files/two", "")
	doRequest(t, r, http.MethodGet, "/files/bad", "")
	doRequest(t, r, http.MethodPost, "/code", "")
	doRequest(t, r, http.MethodGet, "/missing", "")
	require.Equal(t, map[string]uint64{
		"GET /files/:id 200": 2,
		"GET /files/:id 500": 1,
		"POST /code 200":     1,
		"GET  404":           1,
	}, counts)
}

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
	return doJSONRequest(t, r, method, path, token, "")
}

func doJSONRequest(t *testing.T, r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	var req *http.Request
	if body != "" {
		reader = strings.NewReader(body)
		req = httptest.NewRequest(method, path, reader)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
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

// withInitAuth toggles --init-auth enforcement on /internal/init and restores it.
func withInitAuth(t *testing.T, enabled bool) {
	t.Helper()
	previous := flag.InitAuth
	flag.InitAuth = enabled
	t.Cleanup(func() { flag.InitAuth = previous })
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

func TestAccessTokenProbePathsSkipTokenAndInitAuthOptIn(t *testing.T) {
	withTestBinding(t, &binding.RuntimeBinding{
		HasAccessToken:  true,
		AccessTokenHash: mustHash(t, "rotated-token"),
	})
	r := newMiddlewareTestRouter(t, "legacy-token")

	// Default (--init-auth=false): /ping, /ready, and /internal/init stay reachable without the API token.
	withInitAuth(t, false)
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/ping", "").Code)
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/ready", "").Code)
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodPost, "/internal/init", "").Code)

	// Opted-in (--init-auth=true): /ping and /ready stay open, while /internal/init requires the active token.
	withInitAuth(t, true)
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/ping", "").Code)
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/ready", "").Code)
	require.Equal(t, http.StatusUnauthorized, doRequest(t, r, http.MethodPost, "/internal/init", "").Code)
	require.Equal(t, http.StatusUnauthorized, doRequest(t, r, http.MethodPost, "/internal/init", "legacy-token").Code)
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodPost, "/internal/init", "rotated-token").Code)
}

func TestAccessTokenInitBootstrapAndLegacyCompatibility(t *testing.T) {
	t.Run("default init-auth=false keeps legacy and hashed env init unauthenticated (scenarios A and G)", func(t *testing.T) {
		withInitAuth(t, false)
		withTestBinding(t, nil)
		rPlain := newMiddlewareTestRouter(t, "legacy-token")
		require.Equal(t, http.StatusOK, doRequest(t, rPlain, http.MethodPost, "/internal/init", "").Code)

		hashedEnv := "sha256:v1:nonce123:" + strings.TrimPrefix(binding.HashAccessToken("derived-raw"), binding.AccessTokenHashPrefix)
		rHashed := newMiddlewareTestRouter(t, hashedEnv)
		require.Equal(t, http.StatusOK, doRequest(t, rHashed, http.MethodPost, "/internal/init", "").Code)
	})

	t.Run("init-auth=true with no token configured allows initial /internal/init", func(t *testing.T) {
		withInitAuth(t, true)
		withTestBinding(t, nil)
		r := newMiddlewareTestRouter(t, "")
		require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodPost, "/internal/init", "").Code)
	})

	t.Run("init-auth=true with legacy or hashed env token guards /internal/init before binding", func(t *testing.T) {
		withInitAuth(t, true)
		withTestBinding(t, nil)
		r := newMiddlewareTestRouter(t, "legacy-token")
		require.Equal(t, http.StatusUnauthorized, doRequest(t, r, http.MethodPost, "/internal/init", "").Code)
		require.Equal(t, http.StatusUnauthorized, doRequest(t, r, http.MethodPost, "/internal/init", "wrong").Code)
		require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodPost, "/internal/init", "legacy-token").Code)

		rawDerived := "derived-raw-token"
		hashedEnv := "sha256:v1:nonce123:" + strings.TrimPrefix(binding.HashAccessToken(rawDerived), binding.AccessTokenHashPrefix)
		rHashed := newMiddlewareTestRouter(t, hashedEnv)
		require.Equal(t, http.StatusUnauthorized, doRequest(t, rHashed, http.MethodPost, "/internal/init", "").Code)
		require.Equal(t, http.StatusOK, doRequest(t, rHashed, http.MethodPost, "/internal/init", rawDerived).Code)
	})
}

func TestRuntimeInitTokenBootstrapRotationAndRetryFlow(t *testing.T) {
	withTestBinding(t, nil)
	withRuntimeInit(t, true)
	withInitAuth(t, true)
	withManager(t, false)
	r := NewRouter("")

	// 1. Pre-init: business APIs are 503; initial /internal/init is allowed without token.
	require.Equal(t, http.StatusServiceUnavailable, doRequest(t, r, http.MethodGet, "/metrics", "").Code)

	tokenA := "sandbox-token-a"
	tokenB := "sandbox-token-b"
	hashA := binding.HashAccessToken(tokenA)
	hashB := binding.HashAccessToken(tokenB)

	w := doJSONRequest(t, r, http.MethodPost, "/internal/init", "", fmt.Sprintf(
		`{"sandboxId":"sb-1","generation":1,"accessTokenHash":%q}`, hashA,
	))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// 2. Subsequent /internal/init requires tokenA.
	w = doJSONRequest(t, r, http.MethodPost, "/internal/init", "", `{"sandboxId":"sb-1","generation":2,"preserveRuntimeState":true}`)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	w = doJSONRequest(t, r, http.MethodPost, "/internal/init", "wrong", `{"sandboxId":"sb-1","generation":2,"preserveRuntimeState":true}`)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 3. Same-sandbox resume omitting accessTokenHash keeps tokenA active.
	w = doJSONRequest(t, r, http.MethodPost, "/internal/init", tokenA,
		`{"sandboxId":"sb-1","generation":2,"preserveRuntimeState":true}`,
	)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.True(t, binding.Current().VerifyAccessToken(tokenA))

	// 4. Scenario C: authenticated snapshot-restore rebind rotates to tokenB cleanly.
	w = doJSONRequest(t, r, http.MethodPost, "/internal/init", tokenA, fmt.Sprintf(
		`{"sandboxId":"sb-2","generation":3,"accessTokenHash":%q,"preserveRuntimeState":true}`, hashB,
	))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.True(t, binding.Current().VerifyAccessToken(tokenB))
	require.False(t, binding.Current().VerifyAccessToken(tokenA))

	// Old tokenA is now rejected; new tokenB is accepted.
	w = doJSONRequest(t, r, http.MethodPost, "/internal/init", tokenA, `{"sandboxId":"sb-2","generation":4,"preserveRuntimeState":true}`)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	w = doJSONRequest(t, r, http.MethodPost, "/internal/init", tokenB, `{"sandboxId":"sb-2","generation":4,"preserveRuntimeState":true}`)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// 5. Scenario I: with --init-auth enabled and no EXECD_ACCESS_TOKEN fallback,
	// rebinding to a different sandboxId without accessTokenHash fails closed (400)
	// so authentication cannot be silently disarmed.
	prevServerToken := flag.ServerAccessToken
	flag.ServerAccessToken = ""
	t.Cleanup(func() { flag.ServerAccessToken = prevServerToken })

	w = doJSONRequest(t, r, http.MethodPost, "/internal/init", tokenB,
		`{"sandboxId":"sb-3","generation":5,"preserveRuntimeState":true}`,
	)
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	require.True(t, binding.Current().VerifyAccessToken(tokenB), "tokenB must remain active after rejected rebind")
	require.Equal(t, http.StatusUnauthorized, doRequest(t, r, http.MethodGet, "/metrics", "").Code)
}

func TestRuntimeInitFailedApplyRetryWithInitAuth(t *testing.T) {
	// Scenario B: when preStart/entrypoint returns 500, the binding rolls back
	// so the caller can retry the identical /internal/init without hitting 401.
	withTestBinding(t, nil)
	withRuntimeInit(t, true)
	withInitAuth(t, true)

	attempts := 0
	controller.InitRuntimeInitManager(&controller.RuntimeInitConfig{
		LaunchEntrypoint: func([]string) error {
			attempts++
			if attempts == 1 {
				return fmt.Errorf("transient entrypoint failure")
			}
			return nil
		},
		EntrypointArgs: []string{"/bin/entrypoint"},
	})

	r := NewRouter("")
	body := fmt.Sprintf(
		`{"sandboxId":"sb-retry","generation":1,"entrypointPolicy":"restart","accessTokenHash":%q}`,
		binding.HashAccessToken("retry-token"),
	)

	w1 := doJSONRequest(t, r, http.MethodPost, "/internal/init", "", body)
	require.Equal(t, http.StatusInternalServerError, w1.Code)
	require.Nil(t, binding.Current(), "failed init must roll back binding")

	w2 := doJSONRequest(t, r, http.MethodPost, "/internal/init", "", body)
	require.Equal(t, http.StatusOK, w2.Code, "body: %s", w2.Body.String())
	require.True(t, binding.Current().VerifyAccessToken("retry-token"))
}

func TestRuntimeInitBodySizeLimitBoundaries(t *testing.T) {
	withTestBinding(t, nil)
	withRuntimeInit(t, true)
	withInitAuth(t, true)
	withManager(t, false)
	r := NewRouter("")

	prefix := `{"sandboxId":"sb-cap","generation":1,"envs":{"P":"`
	suffix := `"}}`
	exactPad := (1 << 20) - len(prefix) - len(suffix)

	// Unauthenticated pre-init body at 1 MiB + 1 byte -> 413.
	over := prefix + strings.Repeat("a", exactPad+1) + suffix
	require.Equal(t, (1<<20)+1, len(over))
	w := doJSONRequest(t, r, http.MethodPost, "/internal/init", "", over)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "body: %s", w.Body.String())

	// Exactly 1 MiB -> 200.
	exact := prefix + strings.Repeat("a", exactPad) + suffix
	require.Equal(t, 1<<20, len(exact))
	w = doJSONRequest(t, r, http.MethodPost, "/internal/init", "", exact)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
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

func TestDownloadFileMultiRange(t *testing.T) {
	withTestBinding(t, nil)
	withRuntimeInit(t, false)
	r := NewRouter("")
	path := filepath.Join(t.TempDir(), "data.bin")
	require.NoError(t, os.WriteFile(path, []byte("0123456789"), 0644))

	for _, tt := range []struct {
		header     string
		wantStatus int
		wantBody   string
		wantRange  string
		wantLength string
	}{
		{header: "bytes=0-1,5-6", wantStatus: http.StatusOK, wantBody: "0123456789", wantLength: "10"},
		{header: "bytes=0-1,2-3", wantStatus: http.StatusOK, wantBody: "0123456789", wantLength: "10"},
		{header: "bytes=0-1,50-60", wantStatus: http.StatusPartialContent, wantBody: "01", wantRange: "bytes 0-1/10", wantLength: "2"},
	} {
		t.Run(tt.header, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/files/download?path="+url.QueryEscape(path), nil)
			req.Header.Set("Range", tt.header)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			require.Equal(t, tt.wantStatus, w.Code, "body: %q; Content-Range: %q", w.Body.String(), w.Header().Get("Content-Range"))
			require.Equal(t, tt.wantBody, w.Body.String())
			require.Equal(t, tt.wantRange, w.Header().Get("Content-Range"))
			require.Equal(t, tt.wantLength, w.Header().Get("Content-Length"))
			require.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
		})
	}
}

func mustHash(t *testing.T, raw string) [32]byte {
	t.Helper()
	digest, err := binding.ParseAccessTokenHash(binding.HashAccessToken(raw))
	require.NoError(t, err)
	return digest
}
