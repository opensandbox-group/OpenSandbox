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

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/stretchr/testify/require"
)

func TestRunCodeRequestValidate(t *testing.T) {
	req := RunCodeRequest{
		Code: "print('hi')",
	}
	require.NoError(t, req.Validate())

	req.Code = ""
	require.Error(t, req.Validate(), "expected validation error when code is empty")
}

func TestRunCommandRequestValidate(t *testing.T) {
	req := RunCommandRequest{Command: "ls"}
	require.NoError(t, req.Validate(), "expected command validation success")

	req.TimeoutMs = -100
	require.Error(t, req.Validate(), "expected validation error when timeout is negative")

	req.TimeoutMs = 0
	req.Command = "ls"
	require.NoError(t, req.Validate(), "expected success when timeout is omitted/zero")

	req.TimeoutMs = 10
	req.Command = ""
	require.Error(t, req.Validate(), "expected validation error when command is empty")
}

func TestRunCommandRequestValidateCwd(t *testing.T) {
	tmp := t.TempDir()
	req := RunCommandRequest{Command: "ls", Cwd: tmp}
	require.NoError(t, req.Validate())

	req.Cwd = filepath.Join(tmp, "missing-subdir")
	err := req.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "working directory")
}

func ptr32(v uint32) *uint32 { return &v }

func TestRunCommandRequestValidateUidGid(t *testing.T) {
	// uid-only: valid
	req := RunCommandRequest{Command: "id", Uid: ptr32(1000)}
	require.NoError(t, req.Validate(), "expected success with uid only")

	// uid + gid: valid
	req = RunCommandRequest{Command: "id", Uid: ptr32(1000), Gid: ptr32(1000)}
	require.NoError(t, req.Validate(), "expected success with uid and gid")

	// gid-only: must be rejected
	req = RunCommandRequest{Command: "id", Gid: ptr32(1000)}
	require.Error(t, req.Validate(), "expected validation error when gid is set without uid")
}

func TestServerStreamEventToJSON(t *testing.T) {
	event := ServerStreamEvent{
		Type:           StreamEventTypeStdout,
		Text:           "hello",
		ExecutionCount: 3,
	}

	data := event.ToJSON()
	var decoded ServerStreamEvent
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, event.Type, decoded.Type)
	require.Equal(t, event.Text, decoded.Text)
	require.Equal(t, event.ExecutionCount, decoded.ExecutionCount)
}

func TestServerStreamEventSummary(t *testing.T) {
	longText := strings.Repeat("a", 120)
	tests := []struct {
		name     string
		event    ServerStreamEvent
		contains []string
	}{
		{
			name: "basic stdout",
			event: ServerStreamEvent{
				Type:           StreamEventTypeStdout,
				Text:           "hello",
				ExecutionCount: 2,
			},
			contains: []string{"type=stdout", "text=hello"},
		},
		{
			name: "truncated text and error",
			event: ServerStreamEvent{
				Type:  StreamEventTypeError,
				Text:  longText,
				Error: &execute.ErrorOutput{EName: "ValueError", EValue: "boom"},
			},
			contains: []string{
				"type=error",
				"text=" + strings.Repeat("a", 100) + "...",
				"error=ValueError: boom",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary := tt.event.Summary()
			for _, want := range tt.contains {
				require.Containsf(t, summary, want, "summary missing %q", want)
			}
		})
	}
}
