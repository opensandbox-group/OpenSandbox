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
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/binding"
	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// Paths that must be reachable before the RuntimeBinding is applied (and
// without the API access token): liveness, readiness, and the internal
// runtime-init call.
//
// /internal/init is an internal control-plane protocol: it is not part of
// the public execd API surface and carries no external compatibility
// guarantee.
var preInitPaths = map[string]struct{}{
	"/ping":          {},
	"/ready":         {},
	"/internal/init": {},
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
	r.POST("/internal/init", withInit(func(c *controller.InitController) { c.Init() }))
	r.GET("/ready", withInit(func(c *controller.InitController) { c.Ready() }))

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

	return r
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

// accessTokenMiddleware guards API entrypoints. Once a RuntimeBinding with
// a token hash is applied (/internal/init is authoritative), request tokens
// are verified against the hash; before that, the legacy container-env
// token applies. /internal/init, /ready, and /ping are always reachable
// without the token.
func accessTokenMiddleware(legacyToken string) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		if _, preInit := preInitPaths[ctx.FullPath()]; preInit {
			ctx.Next()
			return
		}

		if b := binding.Current(); b != nil && b.HasAccessToken {
			presented := ctx.GetHeader(model.ApiAccessTokenHeader)
			if presented == "" || !b.VerifyAccessToken(presented) {
				abortUnauthorized(ctx)
				return
			}
			ctx.Next()
			return
		}

		// TODO(runtime-init): dynamic authentication is undecided. When the
		// binding carries no token hash (/internal/init omitted
		// accessTokenHash), auth falls back to the legacy container-env
		// token below. This is an internal-protocol compatibility fallback:
		// it must not be relied on long-term, and the control plane should
		// always deliver a token hash until a credential scheme replaces it.
		if legacyToken == "" {
			ctx.Next()
			return
		}

		requestedToken := ctx.GetHeader(model.ApiAccessTokenHeader)
		if requestedToken == "" || requestedToken != legacyToken {
			abortUnauthorized(ctx)
			return
		}

		ctx.Next()
	}
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
