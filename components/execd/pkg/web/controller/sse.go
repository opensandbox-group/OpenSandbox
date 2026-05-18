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
	"io"
	"net/http"
	"time"

	"github.com/alibaba/opensandbox/internal/safego"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

var sseHeaders = map[string]string{
	"Content-Type":      "text/event-stream",
	"Cache-Control":     "no-cache",
	"Connection":        "keep-alive",
	"X-Accel-Buffering": "no",
}

func (c *basicController) setupSSEResponse() {
	for key, value := range sseHeaders {
		c.ctx.Writer.Header().Set(key, value)
	}
	if flusher, ok := c.ctx.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

// setServerEventsHandler adapts runtime callbacks to SSE events.
func (c *CodeInterpretingController) setServerEventsHandler(ctx context.Context, req *runtime.ExecuteCodeRequest) runtime.ExecuteResultHook {
	return runtime.ExecuteResultHook{
		OnExecuteInit: func(session string) {
			if c.resumeEnabled.Load() {
				c.resumeStreamMu.Lock()
				c.resumeStreamID = session
				c.resumeStreamMu.Unlock()
				commandStreams.registerPrimary(session, c.ctx.Writer, c.ctx.Request.Context())
			}

			event := model.ServerStreamEvent{
				Type:      model.StreamEventTypeInit,
				Text:      session,
				Timestamp: time.Now().UnixMilli(),
			}
			payload := event.ToJSON()
			c.writeSingleEvent("OnExecuteInit", payload, true, event.Summary(), 0)

			safego.Go(func() { c.ping(ctx) })
		},
		OnExecuteResult: func(result map[string]any, count int) {
			var mutated map[string]any
			if len(result) > 0 {
				mutated = make(map[string]any)
				for k, v := range result {
					switch k {
					case "text/plain":
						mutated["text"] = v
					default:
						mutated[k] = v
					}
				}
			}

			if count > 0 {
				event := model.ServerStreamEvent{
					Type:           model.StreamEventTypeCount,
					ExecutionCount: count,
					Timestamp:      time.Now().UnixMilli(),
				}
				payload := event.ToJSON()
				c.writeSingleEvent("OnExecuteResult", payload, true, event.Summary(), 0)
			}
			if len(mutated) > 0 {
				event := model.ServerStreamEvent{
					Type:      model.StreamEventTypeResult,
					Results:   mutated,
					Timestamp: time.Now().UnixMilli(),
				}
				payload := event.ToJSON()
				c.writeSingleEvent("OnExecuteResult", payload, true, event.Summary(), 0)
			}
		},
		OnExecuteComplete: func(executionTime time.Duration) {
			eid := req.NextEventID()
			event := model.ServerStreamEvent{
				Eid:           eid,
				Type:          model.StreamEventTypeComplete,
				ExecutionTime: executionTime.Milliseconds(),
				Timestamp:     time.Now().UnixMilli(),
			}
			payload := event.ToJSON()
			c.writeSingleEvent("OnExecuteComplete", payload, true, event.Summary(), eid)
		},
		OnExecuteError: func(err *execute.ErrorOutput) {
			if err == nil {
				return
			}

			eid := req.NextEventID()
			event := model.ServerStreamEvent{
				Eid:       eid,
				Type:      model.StreamEventTypeError,
				Error:     err,
				Timestamp: time.Now().UnixMilli(),
			}
			payload := event.ToJSON()
			c.writeSingleEvent("OnExecuteError", payload, true, event.Summary(), eid)
		},
		OnExecuteStatus: func(status string) {
			event := model.ServerStreamEvent{
				Type:      model.StreamEventTypeStatus,
				Text:      status,
				Timestamp: time.Now().UnixMilli(),
			}
			payload := event.ToJSON()
			c.writeSingleEvent("OnExecuteStatus", payload, true, event.Summary(), 0)
		},
		OnExecuteStdout: func(eid int64, text string) {
			if text == "" {
				return
			}

			event := model.ServerStreamEvent{
				Eid:       eid,
				Type:      model.StreamEventTypeStdout,
				Text:      text,
				Timestamp: time.Now().UnixMilli(),
			}
			payload := event.ToJSON()
			c.writeSingleEvent("OnExecuteStdout", payload, true, event.Summary(), eid)
		},
		OnExecuteStderr: func(eid int64, text string) {
			if text == "" {
				return
			}

			event := model.ServerStreamEvent{
				Eid:       eid,
				Type:      model.StreamEventTypeStderr,
				Text:      text,
				Timestamp: time.Now().UnixMilli(),
			}
			payload := event.ToJSON()
			c.writeSingleEvent("OnExecuteStderr", payload, true, event.Summary(), eid)
		},
	}
}

// writeSingleEvent serializes one SSE frame. When resumeStreamID is set, writes go through commandStreams (live hub + buffer).
// bufEid is stdout/stderr event id for the resume buffer; 0 skips Append (control events, resume catch-up frames).
func (c *CodeInterpretingController) writeSingleEvent(handler string, data []byte, verbose bool, summary string, bufEid int64) {
	if c == nil || c.ctx == nil || c.ctx.Writer == nil {
		return
	}

	var streamID string
	if c.resumeEnabled.Load() {
		c.resumeStreamMu.Lock()
		streamID = c.resumeStreamID
		c.resumeStreamMu.Unlock()
	}
	if streamID != "" {
		commandStreams.writeSSE(streamID, data, bufEid, handler, summary)
		if verbose {
			log.Info("StreamEvent.%s write data %s", handler, summary)
		}
		return
	}

	select {
	case <-c.ctx.Request.Context().Done():
		log.Error("StreamEvent.%s: client disconnected", handler)
		return
	default:
	}

	c.chunkWriter.Lock()
	defer c.chunkWriter.Unlock()

	payload := append(data, '\n', '\n')
	n, err := c.ctx.Writer.Write(payload)
	if err == nil && n != len(payload) {
		err = io.ErrShortWrite
	}

	if err != nil {
		log.Error("StreamEvent.%s write data %s error: %v", handler, summary, err)
		return
	}

	if flusher, ok := c.ctx.Writer.(http.Flusher); ok {
		flusher.Flush()
	}

	if verbose {
		log.Info("StreamEvent.%s write data %s", handler, summary)
	}
}

// ping periodically keeps the SSE connection alive.
func (c *CodeInterpretingController) ping(ctx context.Context) {
	wait.Until(func() {
		if c.ctx.Writer == nil {
			return
		}
		event := model.ServerStreamEvent{
			Type:      model.StreamEventTypePing,
			Text:      "pong",
			Timestamp: time.Now().UnixMilli(),
		}
		payload := event.ToJSON()
		c.writeSingleEvent("Ping", payload, false, event.Summary(), 0)
	}, 3*time.Second, ctx.Done())
}
