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

package mcpproxy

import (
	"time"

	"github.com/google/uuid"
)

// Session tracks a downstream MCP client connection.
type Session struct {
	ID          string
	CreatedAt   time.Time
	Initialized bool
}

func newSession() *Session {
	return &Session{
		ID:        uuid.New().String(),
		CreatedAt: time.Now(),
	}
}
