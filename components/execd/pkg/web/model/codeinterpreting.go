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
	"errors"
	"fmt"
	"strings"

	"github.com/go-playground/validator/v10"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

// RunCodeRequest represents a code execution request.
type RunCodeRequest struct {
	Context CodeContext `json:"context,omitempty"`
	Code    string      `json:"code" validate:"required"`
}

func (r *RunCodeRequest) Validate() error {
	validate := validator.New()
	return validate.Struct(r)
}

// CodeContext tracks session metadata.
type CodeContext struct {
	ID                 string `json:"id,omitempty"`
	CodeContextRequest `json:",inline"`
}

type CodeContextRequest struct {
	Language string `json:"language,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
}

// RunCommandRequest represents a shell command execution request.
type RunCommandRequest struct {
	Command    string `json:"command" validate:"required"`
	Cwd        string `json:"cwd,omitempty"`
	Background bool   `json:"background,omitempty"`
	// TimeoutMs caps execution duration; 0 uses server default.
	TimeoutMs int64 `json:"timeout,omitempty" validate:"omitempty,gte=1"`

	Uid  *uint32           `json:"uid,omitempty"`
	Gid  *uint32           `json:"gid,omitempty"`
	Envs map[string]string `json:"envs,omitempty"`
}

func (r *RunCommandRequest) Validate() error {
	validate := validator.New()
	if err := validate.Struct(r); err != nil {
		return err
	}
	if r.Gid != nil && r.Uid == nil {
		return errors.New("uid is required when gid is provided")
	}
	return runtime.ValidateWorkingDir(r.Cwd)
}

type ServerStreamEventType string

const (
	StreamEventTypeInit     ServerStreamEventType = "init"
	StreamEventTypeStatus   ServerStreamEventType = "status"
	StreamEventTypeError    ServerStreamEventType = "error"
	StreamEventTypeStdout   ServerStreamEventType = "stdout"
	StreamEventTypeStderr   ServerStreamEventType = "stderr"
	StreamEventTypeResult   ServerStreamEventType = "result"
	StreamEventTypeComplete ServerStreamEventType = "execution_complete"
	StreamEventTypeCount    ServerStreamEventType = "execution_count"
	StreamEventTypePing     ServerStreamEventType = "ping"
)

// ServerStreamEvent is emitted to clients over SSE.
type ServerStreamEvent struct {
	Type           ServerStreamEventType `json:"type,omitempty"`
	Text           string                `json:"text,omitempty"`
	ExecutionCount int                   `json:"execution_count,omitempty"`
	ExecutionTime  int64                 `json:"execution_time,omitempty"`
	Timestamp      int64                 `json:"timestamp,omitempty"`
	Results        map[string]any        `json:"results,omitempty"`
	Error          *execute.ErrorOutput  `json:"error,omitempty"`
}

// ToJSON serializes the event for streaming.
func (s ServerStreamEvent) ToJSON() []byte {
	bytes, _ := json.Marshal(s)
	return bytes
}

// Summary renders a lightweight, log-friendly string without JSON.
func (s ServerStreamEvent) Summary() string {
	parts := []string{fmt.Sprintf("type=%s", s.Type)}
	if s.Text != "" {
		parts = append(parts, fmt.Sprintf("text=%s", truncateString(s.Text, 100)))
	}
	if s.ExecutionTime > 0 {
		parts = append(parts, fmt.Sprintf("elapsed_ms=%d", s.ExecutionTime))
	}
	if len(s.Results) > 0 {
		parts = append(parts, fmt.Sprintf("results=%d", len(s.Results)))
	}
	if s.Error != nil {
		errLabel := s.Error.EName
		if errLabel == "" {
			errLabel = "error"
		}
		parts = append(parts, fmt.Sprintf("error=%s: %s", errLabel, truncateString(s.Error.EValue, 80)))
	}
	return strings.Join(parts, " ")
}

func truncateString(value string, maxCount int) string {
	if maxCount <= 0 || len(value) <= maxCount {
		return value
	}
	return value[:maxCount] + "..."
}
