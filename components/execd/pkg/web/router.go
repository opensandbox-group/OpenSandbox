// Copyright 2025 The OpenSandbox Authors
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
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/alibaba/opensandbox/execd/pkg/binding"
	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

const internalInitPath = "/internal/init"

// preInitPaths must pass the runtime-init gate before the RuntimeBinding is
// applied: liveness, readiness, and the internal runtime-init call.
//
// /internal/init is an internal control-plane protocol: it is not part of
// the public execd API surface and carries no external compatibility
// guarantee.
var preInitPaths = map[string]struct{}{
	"/ping":          {},
	"/ready":         {},
	internalInitPath: {},
}

// unauthenticatedPaths are always reachable without an API access token.
// /internal/init skips auth when --init-auth / EXECD_INIT_AUTH is false
// (the backward-compatible default), and requires the active token when
// enabled (remaining open only while no token is configured).
var unauthenticatedPaths = map[string]struct{}{
	"/ping":  {},
	"/ready": {},
}

func NewRouter(accessToken string) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	// The runtime-init gate runs before auth: pre-init business APIs answer
	// a uniform 503 regardless of credentials, and the access-token check
	// only sees requests that passed the gate.
	r.Use(logMiddleware(), otelHTTPMetricsMiddleware(), runtimeInitGate(), accessTokenMiddleware(accessToken), ProxyMiddleware())

	r.GET("/ping", controller.PingHandler)
	r.POST(internalInitPath, withInit(func(c *controller.InitController) { c.Init() }))
	r.GET("/ready", withInit(func(c *controller.InitController) { c.Ready() }))

	registerFilesystemRoutes(r.Group(""))
	registerFilesystemRoutes(r.Group("/v1/filesystem/:uid/:gid", filesystemIdentityMiddleware()))

	registerExecutionRoutes(r)
	return r
}

// Both entrypoints use the same handlers. Identity-scoped requests are
// intercepted before any filesystem access and dispatched to a child process.
func registerFilesystemRoutes(r *gin.RouterGroup) {
	files := r.Group("/files")
	{
		files.DELETE("", withFilesystem(func(c *controller.FilesystemController) { c.RemoveFiles() }))
		files.GET("/info", withFilesystem(func(c *controller.FilesystemController) { c.GetFilesInfo() }))
		files.POST("/mv", withFilesystem(func(c *controller.FilesystemController) { c.RenameFiles() }))
		files.POST("/permissions", withFilesystem(func(c *controller.FilesystemController) { c.ChmodFiles() }))
		files.GET("/search", withFilesystem(func(c *controller.FilesystemController) { c.SearchFiles() }))
		files.POST("/replace", withFilesystem(func(c *controller.FilesystemController) { c.ReplaceContent() }))
		files.POST("/upload", withFilesystem(func(c *controller.FilesystemController) { c.UploadFile() }))
		files.GET("/download", withFilesystem(func(c *controller.FilesystemController) { c.DownloadFile() }))
	}

	directories := r.Group("/directories")
	{
		directories.GET("/list", withFilesystem(func(c *controller.FilesystemController) { c.ListDirectory() }))
		directories.POST("", withFilesystem(func(c *controller.FilesystemController) { c.MakeDirs() }))
		directories.DELETE("", withFilesystem(func(c *controller.FilesystemController) { c.RemoveDirs() }))
	}

}

func registerExecutionRoutes(r *gin.Engine) {
	code := r.Group("/code")
	{
		code.POST("", withCode(func(c *controller.CodeInterpretingController) { c.RunCode() }))
		code.DELETE("", withCode(func(c *controller.CodeInterpretingController) { c.InterruptCode() }))
		code.POST("/context", withCode(func(c *controller.CodeInterpretingController) { c.CreateContext() }))
		code.GET("/contexts", withCode(func(c *controller.CodeInterpretingController) { c.ListContexts() }))
		code.DELETE("/contexts", withCode(func(c *controller.CodeInterpretingController) { c.DeleteContextsByLanguage() }))
		code.DELETE("/contexts/:contextId", withCode(func(c *controller.CodeInterpretingController) { c.DeleteContext() }))
		code.GET("/contexts/:contextId", withCode(func(c *controller.CodeInterpretingController) { c.GetContext() }))
	}

	session := r.Group("/session")
	{
		session.POST("", withCode(func(c *controller.CodeInterpretingController) { c.CreateSession() }))
		session.POST("/:sessionId/run", withCode(func(c *controller.CodeInterpretingController) { c.RunInSession() }))
		session.DELETE("/:sessionId", withCode(func(c *controller.CodeInterpretingController) { c.DeleteSession() }))
	}

	command := r.Group("/command")
	{
		command.POST("", withCode(func(c *controller.CodeInterpretingController) { c.RunCommand() }))
		command.DELETE("", withCode(func(c *controller.CodeInterpretingController) { c.InterruptCommand() }))
		command.GET("/status/:id", withCode(func(c *controller.CodeInterpretingController) { c.GetCommandStatus() }))
		command.GET("/:id/logs", withCode(func(c *controller.CodeInterpretingController) { c.GetBackgroundCommandOutput() }))
	}

	metric := r.Group("/metrics")
	{
		metric.GET("", withMetric(func(c *controller.MetricController) { c.GetMetrics() }))
		metric.GET("/watch", withMetric(func(c *controller.MetricController) { c.WatchMetrics() }))
	}

	pty := r.Group("/pty")
	{
		pty.POST("", withPTY(func(c *controller.PTYController) { c.CreatePTYSession() }))
		pty.GET("/:sessionId", withPTY(func(c *controller.PTYController) { c.GetPTYSessionStatus() }))
		pty.DELETE("/:sessionId", withPTY(func(c *controller.PTYController) { c.DeletePTYSession() }))
		pty.GET("/:sessionId/ws", controller.PTYSessionWebSocket)
	}

	isolated := r.Group("/v1/isolated")
	{
		isolated.POST("/session", withIsolated(func(c *controller.IsolatedSessionController) { c.Create() }))
		isolated.GET("/sessions", withIsolated(func(c *controller.IsolatedSessionController) { c.List() }))
		isolated.GET("/session/:sessionId", withIsolated(func(c *controller.IsolatedSessionController) { c.Get() }))
		isolated.POST("/session/:sessionId/run", withIsolated(func(c *controller.IsolatedSessionController) { c.Run() }))
		isolated.GET("/session/:sessionId/runs/:runId", withIsolated(func(c *controller.IsolatedSessionController) { c.GetRunStatus() }))
		isolated.GET("/session/:sessionId/runs/:runId/logs", withIsolated(func(c *controller.IsolatedSessionController) { c.GetRunLogs() }))
		isolated.DELETE("/session/:sessionId", withIsolated(func(c *controller.IsolatedSessionController) { c.Delete() }))
		isolated.GET("/session/:sessionId/diff", withIsolated(func(c *controller.IsolatedSessionController) { c.Diff() }))
		isolated.POST("/session/:sessionId/commit", withIsolated(func(c *controller.IsolatedSessionController) { c.Commit() }))
		isolated.GET("/session/:sessionId/files/info", withIsolated(func(c *controller.IsolatedSessionController) { c.GetFilesInfo() }))
		isolated.GET("/session/:sessionId/files/download", withIsolated(func(c *controller.IsolatedSessionController) { c.DownloadFile() }))
		isolated.POST("/session/:sessionId/files/upload", withIsolated(func(c *controller.IsolatedSessionController) { c.UploadFile() }))
		isolated.DELETE("/session/:sessionId/files", withIsolated(func(c *controller.IsolatedSessionController) { c.RemoveFiles() }))
		isolated.POST("/session/:sessionId/files/mv", withIsolated(func(c *controller.IsolatedSessionController) { c.RenameFiles() }))
		isolated.POST("/session/:sessionId/files/permissions", withIsolated(func(c *controller.IsolatedSessionController) { c.ChmodFiles() }))
		isolated.POST("/session/:sessionId/files/replace", withIsolated(func(c *controller.IsolatedSessionController) { c.ReplaceContent() }))
		isolated.GET("/session/:sessionId/files/search", withIsolated(func(c *controller.IsolatedSessionController) { c.SearchFiles() }))
		isolated.GET("/session/:sessionId/directories/list", withIsolated(func(c *controller.IsolatedSessionController) { c.ListDirectory() }))
		isolated.POST("/session/:sessionId/directories", withIsolated(func(c *controller.IsolatedSessionController) { c.MakeDirs() }))
		isolated.DELETE("/session/:sessionId/directories", withIsolated(func(c *controller.IsolatedSessionController) { c.RemoveDirs() }))
		isolated.GET("/capabilities", withIsolated(func(c *controller.IsolatedSessionController) { c.Capabilities() }))
	}

}

func withFilesystem(fn func(*controller.FilesystemController)) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		fn(controller.NewFilesystemController(ctx))
	}
}

func withCode(fn func(*controller.CodeInterpretingController)) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		fn(controller.NewCodeInterpretingController(ctx))
	}
}

func withMetric(fn func(*controller.MetricController)) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		fn(controller.NewMetricController(ctx))
	}
}

func withPTY(fn func(*controller.PTYController)) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		fn(controller.NewPTYController(ctx))
	}
}

func withIsolated(fn func(*controller.IsolatedSessionController)) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		fn(controller.NewIsolatedSessionController(ctx))
	}
}

func withInit(fn func(*controller.InitController)) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		fn(controller.NewInitController(ctx))
	}
}

// parseEnvAccessTokenVerifier parses EXECD_ACCESS_TOKEN when it uses the
// hashed verifier format "sha256:<kid>:<nonce>:<hex>" or "sha256:<hex>".
// Returns (digest, isHashed, valid). Malformed sha256: values fail closed
// (isHashed=true, valid=false) so requests are never silently allowed.
func parseEnvAccessTokenVerifier(raw string) ([32]byte, bool, bool) {
	var zero [32]byte
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, binding.AccessTokenHashPrefix) {
		return zero, false, true
	}
	parts := strings.Split(trimmed, ":")
	if len(parts) != 2 && (len(parts) != 4 || parts[1] == "" || parts[2] == "") {
		log.Error("execd auth: malformed sha256 EXECD_ACCESS_TOKEN format; failing closed")
		return zero, true, false
	}
	digest, err := binding.ParseAccessTokenHash(binding.AccessTokenHashPrefix + parts[len(parts)-1])
	if err != nil {
		log.Error("execd auth: invalid sha256 EXECD_ACCESS_TOKEN digest (%v); failing closed", err)
		return zero, true, false
	}
	return digest, true, true
}

// accessTokenMiddleware guards API entrypoints, and also guards
// POST /internal/init when --init-auth / EXECD_INIT_AUTH is enabled.
// Verification priority:
//  1. RuntimeBinding token hash (/internal/init is authoritative)
//  2. Hashed verifier in EXECD_ACCESS_TOKEN ("sha256:<kid>:<nonce>:<hex>" or "sha256:<hex>")
//  3. Deprecated plaintext EXECD_ACCESS_TOKEN (constant-time compare)
//
// When none of the above is set (cold-start bootstrap or legacy unauthenticated
// mode), requests pass without credentials so the initial /internal/init can
// bind a token hash (or stay unauthenticated for legacy compatibility).
// Only /ping and /ready are unconditionally reachable without credentials.
func accessTokenMiddleware(legacyToken string) gin.HandlerFunc {
	expectedHash, isHashed, hashValid := parseEnvAccessTokenVerifier(legacyToken)

	return func(ctx *gin.Context) {
		fullPath := ctx.FullPath()
		if _, public := unauthenticatedPaths[fullPath]; public {
			ctx.Next()
			return
		}
		if fullPath == internalInitPath && !flag.InitAuth {
			ctx.Next()
			return
		}

		presented, valid := requestAccessToken(ctx)
		if !valid {
			abortUnauthorized(ctx)
			return
		}

		currentBinding := binding.Current()
		if !verifyAccessToken(currentBinding, presented, legacyToken, expectedHash, isHashed, hashValid) {
			abortUnauthorized(ctx)
			return
		}
		if fullPath == internalInitPath {
			ctx.Set(controller.InitAuthBindingContextKey, currentBinding)
		}
		ctx.Next()
	}
}

func verifyAccessToken(currentBinding *binding.RuntimeBinding, presented, legacyToken string, expectedHash [32]byte, isHashed, hashValid bool) bool {
	if currentBinding != nil && currentBinding.HasAccessToken {
		return presented != "" && currentBinding.VerifyAccessToken(presented)
	}
	if isHashed {
		if !hashValid || presented == "" {
			return false
		}
		digest := sha256.Sum256([]byte(presented))
		return hmac.Equal(digest[:], expectedHash[:])
	}
	if legacyToken == "" {
		return true
	}
	return presented != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(legacyToken)) == 1
}

// requestAccessToken selects the existing API-token header or, for PTY
// WebSocket upgrades only, a browser credential from the subprotocol list.
func requestAccessToken(ctx *gin.Context) (string, bool) {
	r := ctx.Request
	token := ""
	if r.Method == http.MethodGet && ctx.FullPath() == "/pty/:sessionId/ws" && websocket.IsWebSocketUpgrade(r) {
		var valid bool
		token, valid = ptyWebSocketAccessToken(r)
		if !valid {
			return "", false
		}
	}
	// Presence, not a non-empty value, determines precedence. A bad explicit
	// header must not be rescued by a valid subprotocol credential.
	if _, present := r.Header[http.CanonicalHeaderKey(model.ApiAccessTokenHeader)]; present {
		return r.Header.Get(model.ApiAccessTokenHeader), true
	}
	return token, true
}

func abortUnauthorized(ctx *gin.Context) {
	ctx.AbortWithStatusJSON(http.StatusUnauthorized, map[string]any{
		"error": "Unauthorized: invalid or missing header " + model.ApiAccessTokenHeader,
	})
}

// runtimeInitGate serves only liveness, readiness, and /internal/init until
// runtime init completes (runtime-init mode). The gate checks the manager's
// ready state — not binding presence — because the binding is installed
// atomically mid-apply: a failed apply (500) keeps the binding but must
// stay gated since /ready reports 503 too.
func runtimeInitGate() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		if !flag.RuntimeInit {
			ctx.Next()
			return
		}
		if _, preInit := preInitPaths[ctx.FullPath()]; preInit {
			ctx.Next()
			return
		}
		if controller.GetRuntimeInitManager().Ready() {
			ctx.Next()
			return
		}
		ctx.AbortWithStatusJSON(http.StatusServiceUnavailable, map[string]any{
			"error": "execd is not initialized yet: POST /internal/init must be called first",
		})
	}
}

func logMiddleware() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		log.Info("http: %s %s", ctx.Request.Method, ctx.Request.URL.String())
		ctx.Next()
	}
}
