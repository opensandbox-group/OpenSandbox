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

package manager

import (
	"context"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/types"
)

// TaskManager defines the contract for managing tasks in memory.
type TaskManager interface {
	Create(ctx context.Context, task *types.Task) (*types.Task, error)
	// Sync synchronizes the current task list with the desired state.
	// It deletes tasks not in the desired list and creates new ones.
	// Returns the current task list after synchronization.
	Sync(ctx context.Context, desired []*types.Task) ([]*types.Task, error)

	Get(ctx context.Context, id string) (*types.Task, error)

	List(ctx context.Context) ([]*types.Task, error)

	Delete(ctx context.Context, id string) error

	Start(ctx context.Context)

	Stop()
}
