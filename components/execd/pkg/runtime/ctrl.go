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

package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter"
)

var kernelWaitingBackoff = wait.Backoff{
	Steps:    60,
	Duration: 500 * time.Millisecond,
	Factor:   1.5,
	Jitter:   0.1,
}

// Controller manages code execution across runtimes.
type Controller struct {
	baseURL                 string
	token                   string
	mu                      sync.RWMutex
	jupyterClientMap        sync.Map // map[sessionID]*jupyterKernel
	defaultLanguageSessions sync.Map // map[Language]string
	commandClientMap        sync.Map // map[sessionID]*commandKernel
	bashSessionClientMap    sync.Map // map[sessionID]*bashSession
	ptySessionMap           sync.Map // map[sessionID]*ptySession
	db                      *sql.DB
	dbOnce                  sync.Once
}

type jupyterKernel struct {
	mu       sync.Mutex
	kernelID string
	client   *jupyter.Client
	language Language
}

type commandKernel struct {
	pid          int
	stdoutPath   string
	stderrPath   string
	startedAt    time.Time
	finishedAt   *time.Time
	exitCode     *int
	errMsg       string
	running      bool
	isBackground bool
	content      string
}

// NewController creates a runtime controller.
func NewController(baseURL, token string) *Controller {
	return &Controller{
		baseURL: baseURL,
		token:   token,
	}
}

// Execute dispatches a request to the correct backend.
func (c *Controller) Execute(request *ExecuteCodeRequest) error {
	var cancel context.CancelFunc
	var ctx context.Context
	if request.Timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), request.Timeout)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}

	switch request.Language {
	case Command:
		defer cancel()
		return c.runCommand(ctx, request)
	case BackgroundCommand:
		return c.runBackgroundCommand(ctx, cancel, request)
	case Bash, Python, Java, JavaScript, TypeScript, Go:
		defer cancel()
		return c.runJupyter(ctx, request)
	case SQL:
		defer cancel()
		return c.runSQL(ctx, request)
	default:
		defer cancel()
		return fmt.Errorf("unknown language: %s", request.Language)
	}
}
