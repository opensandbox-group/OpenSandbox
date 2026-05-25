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

package scheduler

import (
	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	apis "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
)

type TaskScheduler interface {
	Schedule() error
	UpdatePods(pod []*corev1.Pod)
	ListTask() []Task
	StopTask() []Task
	// AddTasks registers task specs that are not yet tracked by the scheduler.
	// Tasks whose names are already tracked are silently skipped, making this
	// safe to call with the full task list during a scale-out reconciliation.
	AddTasks(tasks []*apis.Task) error
}

func NewTaskScheduler(name string, tasks []*apis.Task, pods []*corev1.Pod, resPolicyWhenTaskCompleted sandboxv1alpha1.TaskResourcePolicy, logger logr.Logger) (TaskScheduler, error) {
	return newTaskScheduler(name, tasks, pods, resPolicyWhenTaskCompleted, logger)
}
