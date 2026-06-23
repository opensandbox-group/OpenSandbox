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
	"fmt"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/mcpproxy"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

var validUpstreamName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

var mcpManager *mcpproxy.Manager

// InitMCPProxy initializes the MCP proxy manager singleton.
func InitMCPProxy() {
	mcpManager = mcpproxy.NewManager()
}

// MCPController handles /mcp endpoints.
type MCPController struct {
	*basicController
}

// NewMCPController creates a new MCPController from the current Gin context.
func NewMCPController(ctx *gin.Context) *MCPController {
	return &MCPController{basicController: newBasicController(ctx)}
}

// HandleRequest handles POST /mcp — the MCP streamable-http entry point.
// Accepts a JSON-RPC 2.0 request, routes it through the proxy manager,
// and returns the JSON-RPC response.
func (c *MCPController) HandleRequest() {
	var req mcpproxy.Request
	if err := c.bindJSON(&req); err != nil {
		c.ctx.JSON(http.StatusBadRequest, mcpproxy.Response{
			JSONRPC: "2.0",
			Error:   &mcpproxy.RPCError{Code: mcpproxy.CodeParseError, Message: "invalid JSON: " + err.Error()},
		})
		return
	}

	sessionID := c.ctx.GetHeader(model.McpSessionHeader)

	// For non-initialize requests, require a valid session.
	if req.Method != "initialize" && !req.IsNotification() {
		if sessionID == "" {
			c.ctx.JSON(http.StatusBadRequest, mcpproxy.Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &mcpproxy.RPCError{Code: mcpproxy.CodeInvalidRequest, Message: "missing " + model.McpSessionHeader + " header"},
			})
			return
		}
		if mcpManager.GetSession(sessionID) == nil {
			c.ctx.JSON(http.StatusNotFound, mcpproxy.Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &mcpproxy.RPCError{Code: mcpproxy.CodeInvalidRequest, Message: "unknown session"},
			})
			return
		}
	}

	resp, newSessionID, err := mcpManager.HandleRequest(c.ctx.Request.Context(), &req, sessionID)
	if err != nil {
		c.ctx.JSON(http.StatusInternalServerError, mcpproxy.Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &mcpproxy.RPCError{Code: mcpproxy.CodeInternalError, Message: err.Error()},
		})
		return
	}

	// Notifications produce no response.
	if resp == nil {
		c.ctx.Status(http.StatusAccepted)
		return
	}

	if newSessionID != "" {
		c.ctx.Header(model.McpSessionHeader, newSessionID)
	}

	c.ctx.JSON(http.StatusOK, resp)
}

// SSEStream handles GET /mcp — opens an SSE stream for server-initiated messages.
func (c *MCPController) SSEStream() {
	sessionID := c.ctx.GetHeader(model.McpSessionHeader)
	if sessionID == "" || mcpManager.GetSession(sessionID) == nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "missing or invalid "+model.McpSessionHeader)
		return
	}

	c.setupSSEResponse()
	c.ctx.Writer.Flush()

	// Keep the connection open. Currently we only send keepalive pings.
	// Upstream notifications will be forwarded here in a future iteration.
	ctx := c.ctx.Request.Context()
	<-ctx.Done()
}

// DeleteSession handles DELETE /mcp — terminates an MCP session.
func (c *MCPController) DeleteSession() {
	sessionID := c.ctx.GetHeader(model.McpSessionHeader)
	if sessionID == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeMissingQuery, "missing "+model.McpSessionHeader+" header")
		return
	}

	mcpManager.DeleteSession(sessionID)
	c.ctx.Status(http.StatusOK)
}

// AddUpstream handles POST /mcp/upstreams — registers a new upstream MCP server.
func (c *MCPController) AddUpstream() {
	var req model.AddUpstreamRequest
	if err := c.bindJSON(&req); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}

	if !validUpstreamName.MatchString(req.Name) {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "name must be 1-64 chars: alphanumeric, dot, hyphen, underscore")
		return
	}
	if req.Transport == "stdio" && req.Command == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "command is required for stdio transport")
		return
	}
	if req.Transport == "http" && req.URL == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "url is required for http transport")
		return
	}

	config := mcpproxy.UpstreamConfig{
		Name:      req.Name,
		Transport: req.Transport,
		Command:   req.Command,
		Args:      req.Args,
		Env:       req.Env,
		URL:       req.URL,
		Headers:   req.Headers,
	}

	info, err := mcpManager.AddUpstream(c.ctx.Request.Context(), config)
	if err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}

	c.ctx.JSON(http.StatusCreated, info)
}

// ListUpstreams handles GET /mcp/upstreams.
func (c *MCPController) ListUpstreams() {
	c.RespondSuccess(mcpManager.ListUpstreams())
}

// GetUpstream handles GET /mcp/upstreams/:name.
func (c *MCPController) GetUpstream() {
	name := c.ctx.Param("name")
	if name == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeMissingQuery, "missing upstream name")
		return
	}

	info, err := mcpManager.GetUpstream(name)
	if err != nil {
		c.RespondError(http.StatusNotFound, model.ErrorCodeContextNotFound, err.Error())
		return
	}

	c.RespondSuccess(info)
}

// RemoveUpstream handles DELETE /mcp/upstreams/:name.
func (c *MCPController) RemoveUpstream() {
	name := c.ctx.Param("name")
	if name == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeMissingQuery, "missing upstream name")
		return
	}

	if err := mcpManager.RemoveUpstream(name); err != nil {
		c.RespondError(http.StatusNotFound, model.ErrorCodeContextNotFound, err.Error())
		return
	}

	c.ctx.Status(http.StatusOK)
}
