// Copyright 2025 The OpenSandbox Authors
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
	"fmt"

	"k8s.io/klog/v2"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/config"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/types"
)

func NewExecutor(cfg *config.Config) (Executor, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	procExec, err := NewProcessExecutor(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create process executor: %w", err)
	}
	klog.InfoS("process executor initialized", "enableSidecar", cfg.EnableSidecarMode, "mainContainer", cfg.MainContainerName)

	containerExec, err := newContainerExecutor(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create container executor: %w", err)
	}

	return &compositeExecutor{
		processExec:   procExec,
		containerExec: containerExec,
	}, nil
}

// compositeExecutor dispatches tasks to the appropriate underlying executor
type compositeExecutor struct {
	processExec   Executor
	containerExec Executor
}

func (e *compositeExecutor) getDelegate(task *types.Task) (Executor, error) {
	if task == nil {
		return nil, fmt.Errorf("task cannot be nil")
	}
	executor := e.processExec
	if task.Process == nil {
		executor = e.containerExec
	}
	if executor == nil {
		return nil, fmt.Errorf("no executor available for task: %s", task.Name)
	}
	return executor, nil
}

func (e *compositeExecutor) Start(ctx context.Context, task *types.Task) error {
	delegate, err := e.getDelegate(task)
	if err != nil {
		return err
	}
	return delegate.Start(ctx, task)
}

func (e *compositeExecutor) Inspect(ctx context.Context, task *types.Task) (*types.Status, error) {
	delegate, err := e.getDelegate(task)
	if err != nil {
		return nil, err
	}
	return delegate.Inspect(ctx, task)
}

func (e *compositeExecutor) Stop(ctx context.Context, task *types.Task) error {
	delegate, err := e.getDelegate(task)
	if err != nil {
		return err
	}
	return delegate.Stop(ctx, task)
}
