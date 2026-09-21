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
	"errors"
	"fmt"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/config"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/types"
)

type containerExecutor struct {
	config *config.Config
}

// newContainerExecutor creates a new container-based task executor.
// This is a placeholder implementation - container mode is not yet supported.
func newContainerExecutor(cfg *config.Config) (Executor, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	return &containerExecutor{
		config: cfg,
	}, nil
}

// Start is not implemented for container mode yet.
func (e *containerExecutor) Start(ctx context.Context, task *types.Task) error {
	return errors.New("container mode is not implemented yet - use process mode instead")
}

// Inspect is not implemented for container mode yet.
func (e *containerExecutor) Inspect(ctx context.Context, task *types.Task) (*types.Status, error) {
	return nil, errors.New("container mode is not implemented yet - use process mode instead")
}

// Stop is not implemented for container mode yet.
func (e *containerExecutor) Stop(ctx context.Context, task *types.Task) error {
	return errors.New("container mode is not implemented yet - use process mode instead")
}
