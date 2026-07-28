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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/btree"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter"
)

var kernelWaitingBackoff = wait.Backoff{
	Steps:    60,
	Duration: 500 * time.Millisecond,
	Factor:   1.5,
	Jitter:   0.1,
}

// ErrInvalidCommandCursor reports an invalid command inventory cursor.
var ErrInvalidCommandCursor = errors.New("invalid cursor")

// CommandInventoryConfig controls retention of completed command summaries.
type CommandInventoryConfig struct {
	RecoveryTTL time.Duration
	MaxTerminal int
}

var defaultCommandInventoryConfig = CommandInventoryConfig{
	RecoveryTTL: time.Hour,
	MaxTerminal: 1000,
}

// ControllerOption configures a Controller.
type ControllerOption func(*Controller)

// WithCommandInventory configures command inventory retention.
func WithCommandInventory(config CommandInventoryConfig) ControllerOption {
	return func(c *Controller) {
		c.commandInventoryConfig = config
	}
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
	isolatedSessionMap      sync.Map // map[sessionID]*isolatedSession
	db                      *sql.DB
	dbOnce                  sync.Once

	inventoryMu            sync.Mutex
	inventoryProcessID     string
	bySession              map[string]*inventoryEntry
	runningByStartedAt     *btree.BTreeG[*inventoryEntry]
	terminalByStartedAt    *btree.BTreeG[*inventoryEntry]
	terminalByFinishedAt   *btree.BTreeG[*inventoryEntry]
	commandInventoryConfig CommandInventoryConfig
	terminalSequence       uint64
	cleanupOwner           atomic.Bool
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
func NewController(baseURL, token string, options ...ControllerOption) *Controller {
	c := &Controller{
		baseURL:                baseURL,
		token:                  token,
		inventoryProcessID:     uuid.NewString(),
		bySession:              make(map[string]*inventoryEntry),
		runningByStartedAt:     btree.NewBTreeGOptions(inventoryEntryStartedLess, btree.Options{NoLocks: true}),
		terminalByStartedAt:    btree.NewBTreeGOptions(inventoryEntryStartedLess, btree.Options{NoLocks: true}),
		terminalByFinishedAt:   btree.NewBTreeGOptions(inventoryEntryTerminalFinishedLess, btree.Options{NoLocks: true}),
		commandInventoryConfig: defaultCommandInventoryConfig,
	}
	for _, option := range options {
		option(c)
	}
	return c
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
