// Copyright 2026 The OpenSandbox Authors
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
)

type CreateSessionRequest struct {
	Cwd string `json:"cwd,omitempty"`
}

type CreateSessionResponse struct {
	SessionID string `json:"session_id"`
}

type RunInSessionRequest struct {
	Command string `json:"command" validate:"required"`
	Cwd     string `json:"cwd,omitempty"`
	Timeout int64  `json:"timeout,omitempty" validate:"omitempty,gte=0"`
}

// Validate performs structural validation only. The cwd is validated against
// the target session's environment by the runtime (see
// Controller.ValidateBashSessionCwd), because it can reference EXECD_ENVS
// file variables and variables exported in earlier runs of the session.
func (r *RunInSessionRequest) Validate() error {
	validate := validator.New()
	if err := validate.Struct(r); err != nil {
		return err
	}
	return nil
}
