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
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/telemetry"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// RunCommand executes a shell command and streams the output via SSE.
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
	execStart := time.Now()
	var recordOnce sync.Once
	recordExecution := func(result string) {
		recordOnce.Do(func() {
			telemetry.RecordExecutionDuration(
				ctx,
				"run_command",
				result,
				float64(time.Since(execStart))/float64(time.Millisecond),
			)
		})
	}

	runCodeRequest := c.buildExecuteCommandRequest(request)
	eventsHandler, stopSSE := c.setServerEventsHandler(ctx)

	// completeCh is closed when OnExecuteComplete fires, meaning the final SSE
	// event has been written and flushed. We only wait for this callback as a
	// safety check and then return immediately to avoid fixed tail latency.
	completeCh := make(chan struct{})
	var completeOnce sync.Once
	signalComplete := func() {
		completeOnce.Do(func() {
			close(completeCh)
		})
	}
	origComplete := eventsHandler.OnExecuteComplete
	eventsHandler.OnExecuteComplete = func(executionTime time.Duration) {
		origComplete(executionTime)
		recordExecution("success")
		signalComplete()
	}
	origError := eventsHandler.OnExecuteError
	eventsHandler.OnExecuteError = func(err *execute.ErrorOutput) {
		origError(err)
		recordExecution("failure")
		signalComplete()
	}
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
		recordExecution("failure")
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error running commands %v", err),
		)
		return
	}

	waitForExecutionComplete(ctx, completeCh)

	// Keep the SSE connection alive briefly so clients can read all
	// buffered events and downstream components (e.g. egress sidecar)
	// have time to synchronise state changes that were triggered
	// during command execution.
	time.Sleep(flag.ApiGracefulShutdownTimeout)
}

// InterruptCommand stops a running shell command session.
func (c *CodeInterpretingController) InterruptCommand() {
	c.interrupt()
}

// GetCommandStatus returns command status by id.
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

// GetBackgroundCommandOutput returns accumulated stdout/stderr for a command session as plain text.
func (c *CodeInterpretingController) GetBackgroundCommandOutput() {
	id := c.ctx.Param("id")
	if id == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeMissingQuery, "missing command execution id")
		return
	}

	cursor := c.QueryInt64(c.ctx.Query("cursor"), 0)
	output, lastCursor, err := codeRunner.SeekBackgroundCommandOutput(id, cursor)
	if err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, err.Error())
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
			Cwd:      request.Cwd,
			Timeout:  timeout,
			Gid:      request.Gid,
			Uid:      request.Uid,
			Envs:     request.Envs,
		}
	}
}
