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

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/types"
)

// Executor defines the contract for running tasks across different modes.
type Executor interface {
	Start(ctx context.Context, task *types.Task) error
	// Inspect retrieves the current runtime state.
	Inspect(ctx context.Context, task *types.Task) (*types.Status, error)

	Stop(ctx context.Context, task *types.Task) error
}
