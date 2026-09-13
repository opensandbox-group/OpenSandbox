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

import "time"

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

// ListCommandsResponse represents a page of command inventory summaries.
type ListCommandsResponse struct {
	Commands   []any                      `json:"commands"`
	Pagination CommandInventoryPagination `json:"pagination"`
}

// CommandInventoryPagination describes command inventory page traversal.
type CommandInventoryPagination struct {
	Limit      int     `json:"limit"`
	NextCursor *string `json:"nextCursor,omitempty"`
}

// RunningCommandSummary represents a command that is still running.
type RunningCommandSummary struct {
	Session    string    `json:"session"`
	Running    bool      `json:"running"`
	Background bool      `json:"background"`
	StartedAt  time.Time `json:"started_at"`
}

// TerminalCommandSummary represents a command that has completed.
type TerminalCommandSummary struct {
	Session    string     `json:"session"`
	Running    bool       `json:"running"`
	Background bool       `json:"background"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	ExitCode   *int       `json:"exit_code"`
	Error      string     `json:"error,omitempty"`
}
