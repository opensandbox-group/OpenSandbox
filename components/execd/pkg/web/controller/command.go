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

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func (c *CodeInterpretingController) RunCommand() {
	var request model.RunCommandRequest
	if err := c.bindJSON(&request); err != nil {
		c.RespondError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidRequest,
			fmt.Sprintf("error parsing request, MAYBE invalid body format. %v", err),
		)
		return
	}

	err := request.Validate()
	if err != nil {
		c.RespondError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidRequest,
			fmt.Sprintf("invalid request, validation error %v", err),
		)
		return
	}

	ctx, cancel := context.WithCancel(c.ctx.Request.Context())
	tracker := newExecutionTracker(ctx, "run_command")

	runCodeRequest := c.buildExecuteCommandRequest(request)
	eventsHandler, stopSSE := c.setServerEventsHandler(ctx)

	eventsHandler.OnExecuteComplete = func(executionTime time.Duration) {
		var exitCode *int
		if !request.Background {
			// The foreground runtime invokes this hook only after a successful wait.
			code := 0
			exitCode = &code
		}
		c.writeExecutionComplete(executionTime, exitCode)
	}
	tracker.wrap(&eventsHandler)
	runCodeRequest.Hooks = eventsHandler

	// Cancel the context first (signals the ping goroutine to stop), then
	// wait for it to fully exit before the handler returns. This prevents
	// the ping goroutine from writing to the response after Go's net/http
	// closes the response writer.
	defer func() { cancel(); stopSSE() }()

	// SSE headers are committed lazily on the first event write
	// (see writeSingleEvent), so a synchronous error from Execute below can
	// still be surfaced as a structured JSON error response.
	err = codeRunner.Execute(runCodeRequest)
	if err != nil {
		tracker.record("failure")
		c.respondRunError(runCodeRequest.Hooks, tracker.done, fmt.Sprintf("error running commands %v", err))
		return
	}

	waitForExecutionComplete(ctx, tracker.done)

	// Keep the SSE connection alive briefly so clients can read all
	// buffered events and downstream components (e.g. egress sidecar)
	// have time to synchronise state changes that were triggered
	// during command execution.
	time.Sleep(flag.ApiGracefulShutdownTimeout)
}

func (c *CodeInterpretingController) InterruptCommand() {
	c.interrupt()
}

func (c *CodeInterpretingController) GetCommandStatus() {
	commandID := c.ctx.Param("id")
	if commandID == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "missing command execution id")
		return
	}

	status, err := codeRunner.GetCommandStatus(commandID)
	if err != nil {
		c.RespondError(http.StatusNotFound, model.ErrorCodeInvalidRequest, err.Error())
		return
	}

	resp := model.CommandStatusResponse{
		ID:       status.Session,
		Running:  status.Running,
		ExitCode: status.ExitCode,
		Error:    status.Error,
		Content:  status.Content,
	}
	if !status.StartedAt.IsZero() {
		resp.StartedAt = status.StartedAt
	}
	if status.FinishedAt != nil {
		resp.FinishedAt = status.FinishedAt
	}

	c.RespondSuccess(resp)
}

func (c *CodeInterpretingController) GetBackgroundCommandOutput() {
	id := c.ctx.Param("id")
	if id == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeMissingQuery, "missing command execution id")
		return
	}

	var cursor int64
	if raw := c.ctx.Query("cursor"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "cursor must be an integer")
			return
		}
		cursor = parsed
	}
	if cursor < 0 {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "cursor cannot be negative")
		return
	}

	output, lastCursor, err := codeRunner.SeekBackgroundCommandOutput(id, cursor)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, runtime.ErrCommandNotFound) {
			status = http.StatusNotFound
		}
		c.RespondError(status, model.ErrorCodeInvalidRequest, err.Error())
		return
	}

	c.ctx.Header("EXECD-COMMANDS-TAIL-CURSOR", strconv.FormatInt(lastCursor, 10))
	c.ctx.Header("Content-Type", "text/plain; charset=utf-8")
	c.ctx.String(http.StatusOK, "%s", output)
}

func (c *CodeInterpretingController) buildExecuteCommandRequest(request model.RunCommandRequest) *runtime.ExecuteCodeRequest {
	timeout := time.Duration(request.TimeoutMs) * time.Millisecond
	if request.Background {
		return &runtime.ExecuteCodeRequest{
			Language: runtime.BackgroundCommand,
			Code:     request.Command,
			Argv:     request.Argv,
			Cwd:      request.Cwd,
			Timeout:  timeout,
			Gid:      request.Gid,
			Uid:      request.Uid,
			Envs:     request.Envs,
		}
	} else {
		return &runtime.ExecuteCodeRequest{
			Language: runtime.Command,
			Code:     request.Command,
			Argv:     request.Argv,
			Cwd:      request.Cwd,
			Timeout:  timeout,
			Gid:      request.Gid,
			Uid:      request.Uid,
			Envs:     request.Envs,
		}
	}
}
