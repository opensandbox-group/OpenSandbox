// Copyright 2025 Alibaba Group Holding Ltd.
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

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// NewRouter builds a Gin engine with all execd routes.
func NewRouter(accessToken string) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(logMiddleware(), otelHTTPMetricsMiddleware(), accessTokenMiddleware(accessToken), ProxyMiddleware())

	r.GET("/ping", controller.PingHandler)

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
		isolated.GET("/session/:sessionId", withIsolated(func(c *controller.IsolatedSessionController) { c.Get() }))
		isolated.POST("/session/:sessionId/run", withIsolated(func(c *controller.IsolatedSessionController) { c.Run() }))
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

func accessTokenMiddleware(token string) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		if token == "" {
			ctx.Next()
			return
		}

		requestedToken := ctx.GetHeader(model.ApiAccessTokenHeader)
		if requestedToken == "" || requestedToken != token {
			ctx.AbortWithStatusJSON(http.StatusUnauthorized, map[string]any{
				"error": "Unauthorized: invalid or missing header " + model.ApiAccessTokenHeader,
			})
			return
		}

		ctx.Next()
	}
}

func logMiddleware() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		log.Info("Requested: %v - %v", ctx.Request.Method, ctx.Request.URL.String())
		ctx.Next()
	}
}
