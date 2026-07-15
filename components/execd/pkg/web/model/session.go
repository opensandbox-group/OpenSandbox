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

package model

import (
	"github.com/go-playground/validator/v10"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

// CreateSessionRequest is the request body for creating a bash session.
type CreateSessionRequest struct {
	Cwd string `json:"cwd,omitempty"`
}

// CreateSessionResponse is the response for create_session.
type CreateSessionResponse struct {
	SessionID string `json:"session_id"`
}

// RunInSessionRequest is the request body for running a command in an existing session.
type RunInSessionRequest struct {
	Command string `json:"command" validate:"required"`
	Cwd     string `json:"cwd,omitempty"`
	Timeout int64  `json:"timeout,omitempty" validate:"omitempty,gte=0"`
}

// Validate validates RunInSessionRequest.
func (r *RunInSessionRequest) Validate() error {
	validate := validator.New()
	if err := validate.Struct(r); err != nil {
		return err
	}
	return runtime.ValidateWorkingDir(r.Cwd)
}
