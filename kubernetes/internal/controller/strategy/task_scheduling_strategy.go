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

package strategy

import (
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

// TaskSchedulingStrategy defines the strategy interface for task scheduling.
// Different implementations can provide custom logic for determining whether
// task scheduling is needed and how to generate task specifications.
type TaskSchedulingStrategy interface {
	// NeedTaskScheduling determines whether the BatchSandbox requires task scheduling.
	NeedTaskScheduling() bool

	// ValidateShardTaskPatches checks that all shardTaskPatches can be merged into
	// a TaskTemplateSpec without type errors. Returns nil when all patches are valid.
	ValidateShardTaskPatches() error

	// GenerateTaskSpecs generates the complete list of task specifications for the BatchSandbox.
	GenerateTaskSpecs() ([]*api.Task, error)
}
