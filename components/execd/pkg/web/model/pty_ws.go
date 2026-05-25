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

package model

// ClientFrame is a JSON frame sent from the WebSocket client to the server.
type ClientFrame struct {
	Type   string `json:"type"`
	Data   string `json:"data,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Signal string `json:"signal,omitempty"`
}

// ServerFrame is a JSON frame sent from the server to the WebSocket client.
type ServerFrame struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Data      string `json:"data,omitempty"`
	Offset    int64  `json:"offset,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Error     string `json:"error,omitempty"`
	Code      string `json:"code,omitempty"`
	Timestamp int64  `json:"timestamp,omitempty"`
}

// Binary WebSocket frame type bytes — prefix byte for all binary frames.
const (
	BinStdin  byte = 0x00 // Client → Server: raw stdin bytes
	BinStdout byte = 0x01 // Server → Client: raw stdout bytes
	BinStderr byte = 0x02 // Server → Client: raw stderr bytes (pipe mode)
	BinReplay byte = 0x03 // Server → Client: [8 bytes int64 BE offset][raw bytes]
)

// WebSocket error codes sent in ServerFrame.Code.
const (
	WSErrCodeSessionGone      = "SESSION_GONE"
	WSErrCodeStartFailed      = "START_FAILED"
	WSErrCodeStdinWriteFailed = "STDIN_WRITE_FAILED"
	WSErrCodeInvalidFrame     = "INVALID_FRAME"
	WSErrCodeAlreadyConnected = "ALREADY_CONNECTED"
	WSErrCodeRuntimeError     = "RUNTIME_ERROR"
)
