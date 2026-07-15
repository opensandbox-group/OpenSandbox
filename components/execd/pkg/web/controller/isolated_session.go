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
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/telemetry"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// isolatedRunner is set by InitIsolatedRunner during startup.
var isolatedRunner *runtime.IsolatedRunner

// isolatedProbeResult stores the probe result for capabilities reporting.
var isolatedProbeResult *isolation.ProbeResult

// InitIsolatedRunner wires the isolated session runner.
func InitIsolatedRunner(r *runtime.IsolatedRunner) {
	isolatedRunner = r
}

// InitIsolatedProbe stores the probe result for the capabilities endpoint.
func InitIsolatedProbe(p *isolation.ProbeResult) {
	isolatedProbeResult = p
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
	if err := req.Validate(); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, err.Error())
		return
	}

	binds := make([]isolation.BindMount, 0, len(req.Binds))
	for _, b := range req.Binds {
		binds = append(binds, isolation.BindMount{
			Source:   b.Source,
			Dest:     b.Dest,
			ReadOnly: b.ReadOnly,
		})
	}

	opts := &runtime.IsolatedSessionOptions{
		Profile:            req.Profile,
		WorkspacePath:      req.Workspace.Path,
		WorkspaceMode:      req.Workspace.Mode,
		ExtraWritable:      req.ExtraWritable,
		Binds:              binds,
		ShareNet:           req.ShareNet,
		EnvPassthroughMode: req.EnvPassthrough.Mode,
		EnvPassthroughKeys: req.EnvPassthrough.Keys,
		Uid:                req.Uid,
		Gid:                req.Gid,
		UidMode:            req.UidMode,
		IdleTimeoutSeconds: req.IdleTimeoutSeconds,
	}

	sessionID, err := isolatedRunner.CreateIsolatedSession(opts)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not in allowlist") ||
			strings.Contains(err.Error(), "not allowed") ||
			strings.Contains(err.Error(), "unknown isolation profile") ||
			strings.Contains(err.Error(), "must be an existing path") ||
			strings.Contains(err.Error(), "must be an absolute path") ||
			strings.Contains(err.Error(), "source is required") {
			status = http.StatusBadRequest
		}
		c.RespondError(status, model.ErrorCodeRuntimeError, err.Error())
		return
	}

	c.ctx.JSON(http.StatusCreated, model.IsolatedCreateSessionResponse{
		SessionID: sessionID,
		CreatedAt: time.Now(),
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

// List handles GET /v1/isolated/sessions.
func (c *IsolatedSessionController) List() {
	if !c.probed() {
		c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeServiceUnavailable, "isolation unavailable")
		return
	}

	sessions := isolatedRunner.ListIsolatedSessions()
	items := make([]model.IsolatedSessionSummary, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, model.IsolatedSessionSummary{
			SessionID:            s.SessionID,
			Status:               s.Status,
			CreatedAt:            s.CreatedAt,
			LastRunAt:            s.LastRunAt,
			IdleRemainingSeconds: s.IdleRemainingSeconds,
		})
	}

	c.RespondSuccess(model.ListIsolatedSessionsResponse{Sessions: items})
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
	if err := req.Validate(); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, err.Error())
		return
	}

	var ctx context.Context
	var cancel context.CancelFunc
	if req.TimeoutSeconds > 0 {
		ctx, cancel = context.WithTimeout(c.ctx.Request.Context(), time.Duration(req.TimeoutSeconds)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(c.ctx.Request.Context())
	}
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

	startTime := time.Now()
	err := isolatedRunner.RunInIsolatedSession(ctx, sessionID, req.Code, req.Envs, onStdout)
	durationMs := float64(time.Since(startTime)) / float64(time.Millisecond)

	if err != nil {
		if errors.Is(err, runtime.ErrContextNotFound) {
			c.RespondError(http.StatusNotFound, model.ErrorCodeSessionNotFound, "session not found")
			return
		}
		telemetry.RecordIsolatedRun(ctx, "error", durationMs)
		ename := "RuntimeError"
		evalue := err.Error()
		if strings.HasPrefix(evalue, "command exited with code ") {
			ename = "ExitError"
			evalue = strings.TrimPrefix(evalue, "command exited with code ")
		}
		event := model.ServerStreamEvent{
			Type:      model.StreamEventTypeError,
			Text:      err.Error(),
			Timestamp: time.Now().UnixMilli(),
			Error: &execute.ErrorOutput{
				EName:  ename,
				EValue: evalue,
			},
		}
		c.writeSingleEvent("IsolatedError", event.ToJSON(), true, event.Summary())
		return
	}
	telemetry.RecordIsolatedRun(ctx, "success", durationMs)
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
		resp := model.CapabilitiesResponse{
			Available:       false,
			CommitSupported: false,
			DiffSupported:   false,
		}
		if isolatedProbeResult != nil {
			resp.Message = isolatedProbeResult.Message
		}
		c.RespondSuccess(resp)
		return
	}
	caps := isolatedRunner.Capabilities()
	resp := model.CapabilitiesResponse{
		Available:       caps.Available,
		Isolator:        caps.Isolator,
		Version:         caps.Version,
		CommitSupported: caps.CommitSupported,
		DiffSupported:   caps.DiffSupported,
	}
	// Probe results indicate overlay capability, not diff/commit implementation.
	// Diff and commit are Phase 2; do not advertise them as supported.
	resp.CommitSupported = false
	resp.DiffSupported = false
	c.RespondSuccess(resp)
}

// Filesystem proxy handlers are in isolated_session_files.go.
