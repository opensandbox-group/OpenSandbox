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
	baseURL                       string
	token                         string
	mu                            sync.RWMutex
	jupyterClientMap              sync.Map // map[sessionID]*jupyterKernel
	defaultLanguageSessions       sync.Map // map[Language]string
	commandClientMap              sync.Map // map[sessionID]*commandKernel
	bashSessionClientMap          sync.Map // map[sessionID]*bashSession
	bashSessionEnvironmentMu      sync.Mutex
	managedBashSessionEnvironment map[string]managedEnvironmentUpdate
	ptySessionMap                 sync.Map // map[sessionID]*ptySession
	isolatedSessionMap            sync.Map // map[sessionID]*isolatedSession
	db                            *sql.DB
	dbOnce                        sync.Once
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

type managedEnvironmentUpdate struct {
	value           string
	managedFallback string
}

// UpdateManagedBashSessionEnvironment upgrades daemon-managed values in
// existing bash sessions without replacing user overrides.
func (c *Controller) UpdateManagedBashSessionEnvironment(name, value, managedFallback string) {
	c.bashSessionEnvironmentMu.Lock()
	defer c.bashSessionEnvironmentMu.Unlock()

	if c.managedBashSessionEnvironment == nil {
		c.managedBashSessionEnvironment = make(map[string]managedEnvironmentUpdate)
	}
	update := managedEnvironmentUpdate{value: value, managedFallback: managedFallback}
	c.managedBashSessionEnvironment[name] = update
	c.bashSessionClientMap.Range(func(_, sessionValue any) bool {
		if session, ok := sessionValue.(*bashSession); ok {
			session.updateManagedEnvironment(name, update)
		}
		return true
	})
}

func (s *bashSession) updateManagedEnvironment(name string, update managedEnvironmentUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.managedEnvironment == nil {
		s.managedEnvironment = make(map[string]managedEnvironmentUpdate)
	}
	s.managedEnvironment[name] = update
	if !s.clearedEnvironment[name] {
		applyManagedEnvironmentUpdate(s.env, name, update, true)
	}
}

func applyManagedEnvironmentUpdates(
	env map[string]string,
	updates map[string]managedEnvironmentUpdate,
	replaceEmpty bool,
) {
	for name, update := range updates {
		applyManagedEnvironmentUpdate(env, name, update, replaceEmpty)
	}
}

func applyManagedEnvironmentUpdate(
	env map[string]string,
	name string,
	update managedEnvironmentUpdate,
	replaceEmpty bool,
) {
	current, exists := env[name]
	if (!exists || current == "") && !replaceEmpty {
		return
	}
	if current == update.value || (current != "" && current != update.managedFallback) {
		return
	}
	env[name] = update.value
}

func updateClearedEnvironment(
	cleared map[string]bool,
	previous map[string]string,
	current map[string]string,
) map[string]bool {
	if cleared == nil {
		cleared = make(map[string]bool)
	}
	for name, previousValue := range previous {
		currentValue, exists := current[name]
		if !exists || (previousValue != "" && currentValue == "") {
			cleared[name] = true
		}
	}
	for name, currentValue := range current {
		previousValue, existed := previous[name]
		switch {
		case currentValue != "":
			delete(cleared, name)
		case !existed || previousValue != "":
			cleared[name] = true
		}
	}
	return cleared
}

func copyClearedEnvironment(cleared map[string]bool) map[string]bool {
	if len(cleared) == 0 {
		return nil
	}
	copy := make(map[string]bool, len(cleared))
	for name := range cleared {
		copy[name] = true
	}
	return copy
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
