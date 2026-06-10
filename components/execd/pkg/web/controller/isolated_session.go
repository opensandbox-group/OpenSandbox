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

package controller

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// isolatedRunner is set by InitIsolatedRunner during startup.
var isolatedRunner *runtime.IsolatedRunner

// InitIsolatedRunner wires the isolated session runner.
func InitIsolatedRunner(r *runtime.IsolatedRunner) {
	isolatedRunner = r
}

// IsolatedSessionController handles /v1/isolated/* endpoints.
type IsolatedSessionController struct {
	*basicController
}

// NewIsolatedSessionController creates a controller bound to ctx.
func NewIsolatedSessionController(ctx *gin.Context) *IsolatedSessionController {
	return &IsolatedSessionController{
		basicController: newBasicController(ctx),
	}
}

func (c *IsolatedSessionController) probed() bool {
	return isolatedRunner != nil && isolatedRunner.Available()
}

// Create handles POST /v1/isolated/session.
func (c *IsolatedSessionController) Create() {
	if !c.probed() {
		c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeServiceUnavailable, "isolation unavailable")
		return
	}

	var req model.CreateIsolatedSessionRequest
	if err := c.bindJSON(&req); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, err.Error())
		return
	}

	opts := &runtime.IsolatedSessionOptions{
		Profile:            req.Isolation.Profile,
		WorkspacePath:      req.Isolation.Workspace.Path,
		WorkspaceMode:      req.Isolation.Workspace.Mode,
		ExtraWritable:      req.Isolation.ExtraWritable,
		ShareNet:           req.Isolation.ShareNet,
		EnvPassthroughMode: req.Isolation.EnvPassthrough.Mode,
		EnvPassthroughKeys: req.Isolation.EnvPassthrough.Keys,
		Uid:                req.Isolation.Uid,
		Gid:                req.Isolation.Gid,
		IdleTimeoutSeconds: req.Isolation.IdleTimeoutSeconds,
	}

	sessionID, err := isolatedRunner.CreateIsolatedSession(opts)
	if err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}

	c.RespondSuccess(model.IsolatedCreateSessionResponse{
		SessionID: sessionID,
		CreatedAt: time.Now(),
		Isolation: req.Isolation,
	})
}

// Get handles GET /v1/isolated/session/:sessionId.
func (c *IsolatedSessionController) Get() {
	if !c.probed() {
		c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeServiceUnavailable, "isolation unavailable")
		return
	}

	sessionID := c.ctx.Param("sessionId")
	state, err := isolatedRunner.GetIsolatedSession(sessionID)
	if err != nil {
		if errors.Is(err, runtime.ErrContextNotFound) {
			c.RespondError(http.StatusNotFound, model.ErrorCodeSessionNotFound, "session not found")
			return
		}
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}

	c.RespondSuccess(model.SessionState{
		Status:               state.Status,
		CreatedAt:            state.CreatedAt,
		LastRunAt:            state.LastRunAt,
		IdleRemainingSeconds: state.IdleRemainingSeconds,
	})
}

// Run handles POST /v1/isolated/session/:sessionId/run (SSE streaming).
func (c *IsolatedSessionController) Run() {
	if !c.probed() {
		c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeServiceUnavailable, "isolation unavailable")
		return
	}

	sessionID := c.ctx.Param("sessionId")

	var req model.IsolatedRunRequest
	if err := c.bindJSON(&req); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, err.Error())
		return
	}

	ctx, cancel := context.WithCancel(c.ctx.Request.Context())
	defer cancel()

	// SSE stdout callback.
	onStdout := func(line string) {
		if line == "" {
			return
		}
		event := model.ServerStreamEvent{
			Type:      model.StreamEventTypeStdout,
			Text:      line,
			Timestamp: time.Now().UnixMilli(),
		}
		c.writeSingleEvent("IsolatedStdout", event.ToJSON(), false, event.Summary())
	}

	err := isolatedRunner.RunInIsolatedSession(ctx, sessionID, req.Code, onStdout)
	if err != nil {
		event := model.ServerStreamEvent{
			Type:      model.StreamEventTypeError,
			Text:      err.Error(),
			Timestamp: time.Now().UnixMilli(),
		}
		c.writeSingleEvent("IsolatedError", event.ToJSON(), true, event.Summary())
		return
	}
	event := model.ServerStreamEvent{
		Type:      model.StreamEventTypeComplete,
		Timestamp: time.Now().UnixMilli(),
	}
	c.writeSingleEvent("IsolatedComplete", event.ToJSON(), true, event.Summary())
}

// Delete handles DELETE /v1/isolated/session/:sessionId.
func (c *IsolatedSessionController) Delete() {
	if !c.probed() {
		c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeServiceUnavailable, "isolation unavailable")
		return
	}

	sessionID := c.ctx.Param("sessionId")
	if err := isolatedRunner.DeleteIsolatedSession(sessionID); err != nil {
		if errors.Is(err, runtime.ErrContextNotFound) {
			c.RespondError(http.StatusNotFound, model.ErrorCodeSessionNotFound, "session not found")
			return
		}
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}

	c.RespondSuccess(nil)
}

// Diff handles GET /v1/isolated/session/:sessionId/diff.
func (c *IsolatedSessionController) Diff() {
	c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeNotSupported, "diff not implemented yet (phase 2)")
}

// Commit handles POST /v1/isolated/session/:sessionId/commit.
func (c *IsolatedSessionController) Commit() {
	c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeNotSupported, "commit not implemented yet (phase 2)")
}

// Capabilities handles GET /v1/isolated/capabilities.
func (c *IsolatedSessionController) Capabilities() {
	if isolatedRunner == nil {
		c.RespondSuccess(model.CapabilitiesResponse{
			Available:       false,
			CommitSupported: false,
			DiffSupported:   false,
		})
		return
	}
	caps := isolatedRunner.Capabilities()
	c.RespondSuccess(model.CapabilitiesResponse{
		Available:       caps.Available,
		Isolator:        caps.Isolator,
		Version:         caps.Version,
		CommitSupported: caps.CommitSupported,
		DiffSupported:   caps.DiffSupported,
	})
}

// Filesystem proxy handlers are in isolated_session_files.go.
