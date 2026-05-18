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
	"strconv"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/log"
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
	defer cancel()
	c.resumeEnabled.Store(true)
	defer func() {
		deferResumeCleanup(c)
		c.resumeEnabled.Store(false)
	}()
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
	eventsHandler := c.setServerEventsHandler(ctx, runCodeRequest)
	origComplete := eventsHandler.OnExecuteComplete
	eventsHandler.OnExecuteComplete = func(executionTime time.Duration) {
		origComplete(executionTime)
		recordExecution("success")
	}
	origError := eventsHandler.OnExecuteError
	eventsHandler.OnExecuteError = func(err *execute.ErrorOutput) {
		origError(err)
		recordExecution("failure")
	}
	runCodeRequest.Hooks = eventsHandler

	c.setupSSEResponse()
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

// ResumeCommandStream sends buffered events after after_eid, then if the command is still running
// and no other client holds the live slot, streams further events until completion or client disconnect.
func (c *CodeInterpretingController) ResumeCommandStream() {
	commandID := c.ctx.Param("id")
	if commandID == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "missing command execution id")
		return
	}
	afterEid := c.QueryInt64(c.ctx.Query(model.CommandResumeAfterEidQuery), 0)

	hub := commandStreams.getHub(commandID)
	st, errSt := codeRunner.GetCommandStatus(commandID)
	if errSt != nil && hub == nil {
		c.RespondError(http.StatusNotFound, model.ErrorCodeInvalidRequest, errSt.Error())
		return
	}

	events, bufferOK := resumeBuffer.EventsAfter(commandID, afterEid)
	if !bufferOK && hub == nil {
		c.RespondError(http.StatusNotFound, model.ErrorCodeInvalidRequest, "command stream resume buffer not available")
		return
	}

	if st != nil && st.Running && hub != nil && hub.isHolderAlive() {
		c.RespondError(
			http.StatusConflict,
			model.ErrorCodeInvalidRequest,
			"primary SSE stream is still active; disconnect it before resuming",
		)
		return
	}

	c.setupSSEResponse()
	lastReplayMaxEid := afterEid
	for _, ev := range events {
		c.writeSingleEvent("ResumeBuffer", ev.Payload, false, fmt.Sprintf("buffer eid=%d", ev.EID), 0)
		if ev.EID > lastReplayMaxEid {
			lastReplayMaxEid = ev.EID
		}
	}

	st2, _ := codeRunner.GetCommandStatus(commandID)
	if st2 == nil || !st2.Running {
		if len(events) > 0 {
			log.Info("resume stream: command_id=%s after_eid=%d snapshot_events=%d (replay only)",
				commandID, afterEid, len(events))
		}
		return
	}

	hub = commandStreams.getHub(commandID)
	if hub == nil {
		return
	}

	h, err := commandStreams.tryAttachResume(commandID, c.ctx.Writer, c.ctx.Request.Context())
	if err != nil {
		if errors.Is(err, errLiveStreamPrimaryActive) {
			log.Error("ResumeCommandStream: attach conflict after buffered history (another client may have attached)")
		}
		return
	}

	// Catch up events appended while the snapshot slice was replayed (holder still nil); same mutex as writeFrame.
	tailN := h.flushResumeTail(commandID, lastReplayMaxEid)
	log.Info("resume stream: command_id=%s after_eid=%d snapshot_events=%d post_attach_tail=%d (live)",
		commandID, afterEid, len(events), tailN)

	select {
	case <-h.waitDone():
	case <-c.ctx.Request.Context().Done():
	}
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
