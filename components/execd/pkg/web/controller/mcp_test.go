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

package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/mcpproxy"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupMCPRouter() *gin.Engine {
	InitMCPProxy()
	r := gin.New()
	mcp := r.Group("/mcpproxy")
	{
		mcp.POST("", func(ctx *gin.Context) { NewMCPController(ctx).HandleRequest() })
		mcp.DELETE("", func(ctx *gin.Context) { NewMCPController(ctx).DeleteSession() })
		upstreams := mcp.Group("/upstreams")
		{
			upstreams.POST("", func(ctx *gin.Context) { NewMCPController(ctx).AddUpstream() })
			upstreams.GET("", func(ctx *gin.Context) { NewMCPController(ctx).ListUpstreams() })
			upstreams.GET("/:name", func(ctx *gin.Context) { NewMCPController(ctx).GetUpstream() })
			upstreams.DELETE("/:name", func(ctx *gin.Context) { NewMCPController(ctx).RemoveUpstream() })
		}
	}
	return r
}

func mcpPost(t *testing.T, router *gin.Engine, body any, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	ctx, rec := newTestContext(http.MethodPost, "/mcpproxy", data)
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("Accept", "application/json")
	if sessionID != "" {
		ctx.Request.Header.Set(model.McpSessionHeader, sessionID)
	}
	router.ServeHTTP(rec, ctx.Request)
	return rec
}

func TestMCPInitialize(t *testing.T) {
	router := setupMCPRouter()
	body := mcpproxy.Request{
		JSONRPC: "2.0",
		ID:      float64(1),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}`),
	}

	rec := mcpPost(t, router, body, "")
	require.Equal(t, http.StatusOK, rec.Code)

	sid := rec.Header().Get(model.McpSessionHeader)
	require.NotEmpty(t, sid)

	var resp mcpproxy.Response
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Nil(t, resp.Error)
}

func TestMCPMissingSessionHeader(t *testing.T) {
	router := setupMCPRouter()
	body := mcpproxy.Request{
		JSONRPC: "2.0",
		ID:      float64(1),
		Method:  "tools/list",
		Params:  json.RawMessage(`{}`),
	}

	rec := mcpPost(t, router, body, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMCPUnknownSession(t *testing.T) {
	router := setupMCPRouter()
	body := mcpproxy.Request{
		JSONRPC: "2.0",
		ID:      float64(1),
		Method:  "tools/list",
		Params:  json.RawMessage(`{}`),
	}

	rec := mcpPost(t, router, body, "bogus-session")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestMCPToolsListEmpty(t *testing.T) {
	router := setupMCPRouter()

	// Initialize first.
	initBody := mcpproxy.Request{
		JSONRPC: "2.0",
		ID:      float64(1),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}`),
	}
	initRec := mcpPost(t, router, initBody, "")
	sid := initRec.Header().Get(model.McpSessionHeader)
	require.NotEmpty(t, sid)

	// tools/list with no upstreams.
	body := mcpproxy.Request{
		JSONRPC: "2.0",
		ID:      float64(2),
		Method:  "tools/list",
		Params:  json.RawMessage(`{}`),
	}
	rec := mcpPost(t, router, body, sid)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp mcpproxy.Response
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Nil(t, resp.Error)

	var result struct {
		Tools []json.RawMessage `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &result))
	require.Len(t, result.Tools, 0)
}

func TestMCPDeleteSession(t *testing.T) {
	router := setupMCPRouter()

	// Initialize.
	initBody := mcpproxy.Request{
		JSONRPC: "2.0",
		ID:      float64(1),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}`),
	}
	initRec := mcpPost(t, router, initBody, "")
	sid := initRec.Header().Get(model.McpSessionHeader)

	// Delete session.
	ctx, rec := newTestContext(http.MethodDelete, "/mcpproxy", nil)
	ctx.Request.Header.Set(model.McpSessionHeader, sid)
	router.ServeHTTP(rec, ctx.Request)
	require.Equal(t, http.StatusOK, rec.Code)

	// Session should be gone.
	body := mcpproxy.Request{
		JSONRPC: "2.0",
		ID:      float64(2),
		Method:  "tools/list",
		Params:  json.RawMessage(`{}`),
	}
	rec2 := mcpPost(t, router, body, sid)
	require.Equal(t, http.StatusNotFound, rec2.Code)
}

func TestMCPDeleteSessionMissingHeader(t *testing.T) {
	router := setupMCPRouter()
	ctx, rec := newTestContext(http.MethodDelete, "/mcpproxy", nil)
	router.ServeHTTP(rec, ctx.Request)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMCPPing(t *testing.T) {
	router := setupMCPRouter()

	// Initialize.
	initBody := mcpproxy.Request{
		JSONRPC: "2.0",
		ID:      float64(1),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}`),
	}
	initRec := mcpPost(t, router, initBody, "")
	sid := initRec.Header().Get(model.McpSessionHeader)

	// Ping.
	body := mcpproxy.Request{JSONRPC: "2.0", ID: float64(2), Method: "ping"}
	rec := mcpPost(t, router, body, sid)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp mcpproxy.Response
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Nil(t, resp.Error)
}

func TestMCPInvalidJSON(t *testing.T) {
	router := setupMCPRouter()
	ctx, rec := newTestContext(http.MethodPost, "/mcpproxy", []byte(`{invalid`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, ctx.Request)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMCPAddUpstreamValidation(t *testing.T) {
	router := setupMCPRouter()

	t.Run("stdio missing command", func(t *testing.T) {
		body := model.AddUpstreamRequest{
			Name:      "test",
			Transport: "stdio",
		}
		data, _ := json.Marshal(body)
		ctx, rec := newTestContext(http.MethodPost, "/mcpproxy/upstreams", data)
		ctx.Request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, ctx.Request)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("http missing url", func(t *testing.T) {
		body := model.AddUpstreamRequest{
			Name:      "test",
			Transport: "http",
		}
		data, _ := json.Marshal(body)
		ctx, rec := newTestContext(http.MethodPost, "/mcpproxy/upstreams", data)
		ctx.Request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, ctx.Request)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

func TestMCPListUpstreamsEmpty(t *testing.T) {
	router := setupMCPRouter()
	ctx, rec := newTestContext(http.MethodGet, "/mcpproxy/upstreams", nil)
	router.ServeHTTP(rec, ctx.Request)
	require.Equal(t, http.StatusOK, rec.Code)

	var list []mcpproxy.UpstreamInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 0)
}

func TestMCPGetUpstreamNotFound(t *testing.T) {
	router := setupMCPRouter()
	ctx, rec := newTestContext(http.MethodGet, "/mcpproxy/upstreams/nonexistent", nil)
	router.ServeHTTP(rec, ctx.Request)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestMCPRemoveUpstreamNotFound(t *testing.T) {
	router := setupMCPRouter()
	ctx, rec := newTestContext(http.MethodDelete, "/mcpproxy/upstreams/nonexistent", nil)
	router.ServeHTTP(rec, ctx.Request)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestMCPNotificationReturnsAccepted(t *testing.T) {
	router := setupMCPRouter()

	// Initialize first.
	initBody := mcpproxy.Request{
		JSONRPC: "2.0",
		ID:      float64(1),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}`),
	}
	initRec := mcpPost(t, router, initBody, "")
	sid := initRec.Header().Get(model.McpSessionHeader)

	// Send notification.
	body := mcpproxy.Request{JSONRPC: "2.0", Method: "notifications/initialized"}
	rec := mcpPost(t, router, body, sid)
	require.Equal(t, http.StatusAccepted, rec.Code)
}
