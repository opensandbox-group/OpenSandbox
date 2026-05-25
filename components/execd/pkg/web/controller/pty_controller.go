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
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// PTYController handles /pty/* REST endpoints.
type PTYController struct {
	*basicController
}

// NewPTYController creates a new PTYController from the current Gin context.
func NewPTYController(ctx *gin.Context) *PTYController {
	return &PTYController{basicController: newBasicController(ctx)}
}

// CreatePTYSession handles POST /pty.
// Creates a new PTY session and returns its session_id.
func (c *PTYController) CreatePTYSession() {
	if !runtime.IsPTYSessionSupported() {
		c.RespondError(
			http.StatusNotImplemented,
			model.ErrorCodeNotSupported,
			"pty sessions are not supported on this platform",
		)
		return
	}

	var req model.CreatePTYSessionRequest
	if err := c.bindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.RespondError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidRequest,
			fmt.Sprintf("error parsing request: %v", err),
		)
		return
	}

	id := runtime.NewPTYSessionID()
	_, err := codeRunner.CreatePTYSession(id, req.Cwd)
	if err != nil {
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error creating pty session: %v", err),
		)
	}
	c.ctx.JSON(http.StatusCreated, model.CreatePTYSessionResponse{SessionID: id})
}

// GetPTYSessionStatus handles GET /pty/:sessionId.
func (c *PTYController) GetPTYSessionStatus() {
	if !runtime.IsPTYSessionSupported() {
		c.RespondError(
			http.StatusNotImplemented,
			model.ErrorCodeNotSupported,
			"pty sessions are not supported on this platform",
		)
		return
	}

	id := c.ctx.Param("sessionId")
	if id == "" {
		c.RespondError(
			http.StatusBadRequest,
			model.ErrorCodeMissingQuery,
			"missing path parameter 'sessionId'",
		)
		return
	}

	running, offset, err := codeRunner.GetPTYSessionStatus(id)
	if err != nil {
		if errors.Is(err, runtime.ErrContextNotFound) {
			c.RespondError(
				http.StatusNotFound,
				model.ErrorCodeContextNotFound,
				fmt.Sprintf("pty session %s not found", id),
			)
			return
		}
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error getting pty session status: %v", err),
		)
		return
	}

	c.RespondSuccess(model.PTYSessionStatusResponse{
		SessionID:    id,
		Running:      running,
		OutputOffset: offset,
	})
}

// DeletePTYSession handles DELETE /pty/:sessionId.
func (c *PTYController) DeletePTYSession() {
	if !runtime.IsPTYSessionSupported() {
		c.RespondError(
			http.StatusNotImplemented,
			model.ErrorCodeNotSupported,
			"pty sessions are not supported on this platform",
		)
		return
	}

	id := c.ctx.Param("sessionId")
	if id == "" {
		c.RespondError(
			http.StatusBadRequest,
			model.ErrorCodeMissingQuery,
			"missing path parameter 'sessionId'",
		)
		return
	}

	if err := codeRunner.DeletePTYSession(id); err != nil {
		if errors.Is(err, runtime.ErrContextNotFound) {
			c.RespondError(
				http.StatusNotFound,
				model.ErrorCodeContextNotFound,
				fmt.Sprintf("pty session %s not found", id),
			)
			return
		}
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error deleting pty session: %v", err),
		)
		return
	}

	c.RespondSuccess(nil)
}
