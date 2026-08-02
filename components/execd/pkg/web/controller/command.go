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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/telemetry"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

const (
	defaultCommandInventoryLimit = 50
	maxCommandInventoryLimit     = 100
)

var (
	errInvalidCommandInventoryQuery  = errors.New("invalid query parameter")
	errInvalidCommandInventoryCursor = errors.New("invalid cursor")
)

// ListCommands returns a page of command inventory summaries.
func (c *CodeInterpretingController) ListCommands() {
	c.ctx.Header("Cache-Control", "no-store")

	request, err := parseListCommandsQuery(c.ctx.Request.URL.RawQuery)
	if err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidQuery, err.Error())
		return
	}

	response, err := codeRunner.ListCommands(request)
	if err != nil {
		if errors.Is(err, runtime.ErrInvalidCommandCursor) {
			c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidQuery, "invalid cursor")
			return
		}
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error listing commands. %v", err),
		)
		return
	}

	commands := make([]any, 0, len(response.Commands))
	for _, command := range response.Commands {
		if command.Running {
			commands = append(commands, model.RunningCommandSummary{
				Session:    command.Session,
				Running:    true,
				Background: command.Background,
				StartedAt:  command.StartedAt,
			})
			continue
		}
		commands = append(commands, model.TerminalCommandSummary{
			Session:    command.Session,
			Running:    false,
			Background: command.Background,
			StartedAt:  command.StartedAt,
			FinishedAt: command.FinishedAt,
			ExitCode:   command.ExitCode,
			Error:      command.Error,
		})
	}

	c.RespondSuccess(model.ListCommandsResponse{
		Commands: commands,
		Pagination: model.CommandInventoryPagination{
			Limit:      response.Pagination.Limit,
			NextCursor: response.Pagination.NextCursor,
		},
	})
}

func parseListCommandsQuery(rawQuery string) (runtime.ListCommandsRequest, error) {
	request := runtime.ListCommandsRequest{Limit: defaultCommandInventoryLimit}
	if rawQuery == "" {
		return request, nil
	}

	components := strings.Split(rawQuery, "&")
	seen := make(map[string]bool, 3)
	invalidQuery := false
	emptyCursor := false
	for _, component := range components {
		if component == "" {
			invalidQuery = true
			continue
		}
		rawKey, rawValue, hasValue := strings.Cut(component, "=")
		key, keyErr := url.QueryUnescape(rawKey)
		value, valueErr := url.QueryUnescape(rawValue)
		if keyErr != nil || valueErr != nil {
			invalidQuery = true
			continue
		}
		if key == "cursor" && (!hasValue || strings.TrimSpace(value) == "") {
			emptyCursor = true
			continue
		}
		if key != "running" && key != "limit" && key != "cursor" || !hasValue || seen[key] {
			invalidQuery = true
			continue
		}
		seen[key] = true
		if strings.TrimSpace(value) == "" {
			invalidQuery = true
			continue
		}

		switch key {
		case "running":
			running, err := strconv.ParseBool(value)
			if err != nil || (value != "true" && value != "false") {
				invalidQuery = true
				continue
			}
			request.Running = &running
		case "limit":
			if !isDecimal(value) {
				invalidQuery = true
				continue
			}
			limit, err := strconv.Atoi(value)
			if err != nil || limit < 1 || limit > maxCommandInventoryLimit {
				invalidQuery = true
				continue
			}
			request.Limit = limit
		case "cursor":
			request.Cursor = value
		}
	}
	if emptyCursor {
		return runtime.ListCommandsRequest{}, errInvalidCommandInventoryCursor
	}
	if invalidQuery {
		return runtime.ListCommandsRequest{}, errInvalidCommandInventoryQuery
	}
	return request, nil
}

func isDecimal(value string) bool {
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return value != ""
}

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
