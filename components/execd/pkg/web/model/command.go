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
	"errors"
	"time"

	"github.com/go-playground/validator/v10"
)

const CommandResumeAfterEidQuery = "after_eid"

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
	return nil
}

// CommandStatusResponse represents command status for REST APIs.
type CommandStatusResponse struct {
	ID         string     `json:"id"`
	Content    string     `json:"content,omitempty"`
	Running    bool       `json:"running"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}
