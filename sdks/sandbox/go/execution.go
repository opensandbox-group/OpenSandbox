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

package opensandbox

import (
	"encoding/json"
	"strconv"
	"strings"
)

// OutputMessage represents a single stdout or stderr line from command execution.
type OutputMessage struct {
	Text      string `json:"text"`
	Timestamp int64  `json:"timestamp"`
}

// ExecutionResult represents a result output from code execution.
// Results maps MIME types to their string representations (e.g. "text/plain" → "4").
type ExecutionResult struct {
	// Results holds MIME-type keyed outputs (e.g. "text/plain", "text/html").
	Results   map[string]string `json:"results,omitempty"`
	Timestamp int64             `json:"timestamp"`
}

// Text returns the text/plain result, or empty string if not present.
func (r ExecutionResult) Text() string {
	return r.Results["text/plain"]
}

// ExecutionError represents an error during code/command execution.
type ExecutionError struct {
	Name      string   `json:"name"`
	Value     string   `json:"value"`
	Timestamp int64    `json:"timestamp"`
	Traceback []string `json:"traceback"`
}

// ExecutionComplete represents the completion event from the server.
type ExecutionComplete struct {
	Timestamp     int64 `json:"timestamp"`
	ExecutionTime int64 `json:"execution_time"`
}

// ExecutionInit represents the initialization event from the server.
type ExecutionInit struct {
	ID        string `json:"text"`
	Timestamp int64  `json:"timestamp"`
}

// Execution is the structured result of a command or code execution.
type Execution struct {
	// ID is the execution/command identifier from the init event.
	ID string

	// Stdout contains all stdout messages in order.
	Stdout []OutputMessage

	// Stderr contains all stderr messages in order.
	Stderr []OutputMessage

	// Results contains execution results (for code interpreter).
	Results []ExecutionResult

	// Error is set if the execution failed.
	Error *ExecutionError

	// Complete is set when execution finishes.
	Complete *ExecutionComplete

	// ExitCode is the process exit code. Nil if not available.
	ExitCode *int

	// LastEid is the highest event ID seen so far, used for SSE resume.
	LastEid int64
}

// Text returns the combined stdout text.
func (e *Execution) Text() string {
	var b strings.Builder
	for i, m := range e.Stdout {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(m.Text)
	}
	return b.String()
}

// ExecutionHandlers provides optional callbacks invoked during streaming execution.
// Return a non-nil error from any handler to abort the stream.
type ExecutionHandlers struct {
	OnInit     func(ExecutionInit) error
	OnStdout   func(OutputMessage) error
	OnStderr   func(OutputMessage) error
	OnResult   func(ExecutionResult) error
	OnComplete func(ExecutionComplete) error
	OnError    func(ExecutionError) error
}

// sseErrorPayload is the nested error object in a ServerStreamEvent.
type sseErrorPayload struct {
	EName     string   `json:"ename,omitempty"`
	EValue    string   `json:"evalue,omitempty"`
	Traceback []string `json:"traceback,omitempty"`
}

// sseEvent is the raw JSON structure from execd SSE/NDJSON events.
// Supports both the spec format (nested error/results objects) and the
// legacy flat format (top-level ename/evalue/traceback) for backward compat.
type sseEvent struct {
	Type          string `json:"type"`
	Text          string `json:"text"`
	Timestamp     int64  `json:"timestamp"`
	Eid           int64  `json:"eid,omitempty"`
	ExitCode      *int   `json:"exit_code,omitempty"`
	ExecutionTime int64  `json:"execution_time,omitempty"`

	// Nested error object (spec: {"type":"error","error":{...}})
	Error *sseErrorPayload `json:"error,omitempty"`
	// Nested results object (spec: {"type":"result","results":{"text/plain":"..."}})
	Results map[string]string `json:"results,omitempty"`

	// Flat error fields kept for backward compatibility with older servers
	EName     string   `json:"ename,omitempty"`
	EValue    string   `json:"evalue,omitempty"`
	Traceback []string `json:"traceback,omitempty"`
}

// processStreamEvent parses a raw StreamEvent into the Execution accumulator
// and invokes the appropriate handler.
func processStreamEvent(exec *Execution, event StreamEvent, handlers *ExecutionHandlers) error {
	data := event.Data
	if data == "" {
		return nil
	}

	var ev sseEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		// Not JSON — treat as raw stdout
		msg := OutputMessage{Text: data}
		exec.Stdout = append(exec.Stdout, msg)
		if handlers != nil && handlers.OnStdout != nil {
			return handlers.OnStdout(msg)
		}
		return nil
	}

	if ev.Eid > exec.LastEid {
		exec.LastEid = ev.Eid
	}

	switch ev.Type {
	case "init":
		initEvent := ExecutionInit{ID: ev.Text, Timestamp: ev.Timestamp}
		exec.ID = ev.Text
		if handlers != nil && handlers.OnInit != nil {
			return handlers.OnInit(initEvent)
		}

	case "stdout":
		msg := OutputMessage{Text: ev.Text, Timestamp: ev.Timestamp}
		exec.Stdout = append(exec.Stdout, msg)
		if handlers != nil && handlers.OnStdout != nil {
			return handlers.OnStdout(msg)
		}

	case "stderr":
		msg := OutputMessage{Text: ev.Text, Timestamp: ev.Timestamp}
		exec.Stderr = append(exec.Stderr, msg)
		if handlers != nil && handlers.OnStderr != nil {
			return handlers.OnStderr(msg)
		}

	case "result":
		res := ExecutionResult{Timestamp: ev.Timestamp}
		if ev.Results != nil {
			// Spec format: MIME-keyed map under "results"
			res.Results = ev.Results
		} else if ev.Text != "" {
			// Legacy flat format: bare "text" field
			res.Results = map[string]string{"text/plain": ev.Text}
		}
		exec.Results = append(exec.Results, res)
		if handlers != nil && handlers.OnResult != nil {
			return handlers.OnResult(res)
		}

	case "error":
		var ename, evalue string
		var traceback []string
		// Prefer nested error object per spec; fall back to flat fields
		if ev.Error != nil {
			ename = ev.Error.EName
			evalue = ev.Error.EValue
			traceback = ev.Error.Traceback
		} else {
			ename = ev.EName
			evalue = ev.EValue
			traceback = ev.Traceback
		}
		execErr := ExecutionError{
			Name:      ename,
			Value:     evalue,
			Timestamp: ev.Timestamp,
			Traceback: traceback,
		}
		exec.Error = &execErr
		// Try to parse exit code from error value
		if code, err := strconv.Atoi(evalue); err == nil {
			exec.ExitCode = &code
		}
		if handlers != nil && handlers.OnError != nil {
			return handlers.OnError(execErr)
		}

	case "execution_complete":
		complete := ExecutionComplete{
			Timestamp:     ev.Timestamp,
			ExecutionTime: ev.ExecutionTime,
		}
		exec.Complete = &complete
		// Foreground command exit code: 0 if no error
		if exec.ExitCode == nil && exec.Error == nil {
			zero := 0
			exec.ExitCode = &zero
		}
		if handlers != nil && handlers.OnComplete != nil {
			return handlers.OnComplete(complete)
		}

	case "ping":
		// Ignore ping events

	default:
		// Unknown event type — treat as stdout
		if ev.Text != "" {
			msg := OutputMessage{Text: ev.Text, Timestamp: ev.Timestamp}
			exec.Stdout = append(exec.Stdout, msg)
			if handlers != nil && handlers.OnStdout != nil {
				return handlers.OnStdout(msg)
			}
		}
	}

	return nil
}
