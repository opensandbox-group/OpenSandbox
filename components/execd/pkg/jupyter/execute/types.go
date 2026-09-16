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

// Package execute provides functionality for executing Jupyter kernel code via WebSocket
package execute

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type MessageType string

const (
	MsgExecuteRequest MessageType = "execute_request"

	MsgExecuteInput MessageType = "execute_input"

	MsgExecuteResult MessageType = "execute_result"

	MsgDisplayData MessageType = "display_data"

	MsgStream MessageType = "stream"

	MsgError MessageType = "error"

	MsgStatus MessageType = "status"

	MsgClearOutput MessageType = "clear_output"

	MsgComm MessageType = "comm"

	MsgCommOpen MessageType = "comm_open"

	MsgCommClose MessageType = "comm_close"

	MsgCommMsg MessageType = "comm_msg"

	MsgKernelInfo MessageType = "kernel_info_request"

	MsgKernelInfoReply MessageType = "kernel_info_reply"

	MsgExecuteReply MessageType = "execute_reply"
)

type StreamType string

const (
	StreamStdout StreamType = "stdout"

	StreamStderr StreamType = "stderr"
)

type ExecutionState string

const (
	StateIdle ExecutionState = "idle"

	StateBusy ExecutionState = "busy"

	StateStarting ExecutionState = "starting"
)

type Header struct {
	MessageID string `json:"msg_id"`

	Username string `json:"username"`

	Session string `json:"session"`

	Date string `json:"date"`

	MessageType string `json:"msg_type"`

	Version string `json:"version"`
}

// Message defines the basic structure of Jupyter messages
type Message struct {
	Header Header `json:"header"`

	// ParentHeader is the parent message header, used to track requests and responses
	ParentHeader Header `json:"parent_header"`

	Metadata map[string]interface{} `json:"metadata"`

	Content json.RawMessage `json:"content"`

	Buffers [][]byte `json:"buffers"`

	Channel string `json:"channel"`
}

type ExecuteRequest struct {
	Code string `json:"code"`

	Silent bool `json:"silent"`

	StoreHistory bool `json:"store_history"`

	UserExpressions map[string]string `json:"user_expressions"`

	AllowStdin bool `json:"allow_stdin"`

	StopOnError bool `json:"stop_on_error"`
}

type StreamOutput struct {
	Name StreamType `json:"name"`

	Text string `json:"text"`
}

type ExecuteResult struct {
	ExecutionCount int `json:"execution_count"`

	Data map[string]interface{} `json:"data"`

	Metadata map[string]interface{} `json:"metadata"`
}

type ExecuteReply struct {
	ExecutionCount int `json:"execution_count"`

	Status string `json:"status"`

	ErrorOutput `json:",inline"`
}

type DisplayData struct {
	Data map[string]interface{} `json:"data"`

	Metadata map[string]interface{} `json:"metadata"`
}

type ErrorOutput struct {
	EName string `json:"ename"`

	EValue string `json:"evalue"`

	Traceback []string `json:"traceback"`
}

func (e *ErrorOutput) String() string {
	return fmt.Sprintf(`
Error: %s
Value: %s
Traceback: %s
`, e.EName, e.EValue, strings.Join(e.Traceback, "\n"))
}

type StatusUpdate struct {
	ExecutionState ExecutionState `json:"execution_state"`
}

type ExecutionResult struct {
	Status string `json:"status"`

	ExecutionCount int `json:"execution_count"`

	Stream []*StreamOutput `json:"stream"`

	Error *ErrorOutput `json:"error"`

	ExecutionTime time.Duration `json:"execution_time"`

	ExecutionData map[string]interface{} `json:"execution_data"`
}

type CallbackHandler struct {
	OnExecuteResult func(*ExecuteResult)

	OnStream func(...*StreamOutput)

	OnDisplayData func(*DisplayData)

	OnError func(*ErrorOutput)

	OnStatus func(*StatusUpdate)
}
