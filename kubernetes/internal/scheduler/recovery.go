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
	"context"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"

	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

// recover reconstructs the task scheduler state from existing pods and their endpoints
// This function is used to restore the scheduler state after a restart
func (sch *defaultTaskScheduler) recover() error {
	var err error
	sch.once.Do(func() {
		sch.recoverTaskNodesStatus()
		sch.logger.Info("task scheduler recovered", "scheduler", sch.name, "task_nodes", len(sch.taskNodes), "all_pods", len(sch.allPods))
	})
	return err
}

func (sch *defaultTaskScheduler) recoverTaskNodesStatus() error {
	ips := make([]string, 0, len(sch.allPods)/2)
	pods := make([]*v1.Pod, 0, len(sch.allPods)/2)
	for i := range sch.allPods {
		pod := sch.allPods[i]
		if pod.Status.PodIP == "" {
			continue
		}
		ips = append(ips, pod.Status.PodIP)
		pods = append(pods, pod)
	}
	if len(ips) == 0 {
		return nil
	}
	// TODO: When the agent starts stopping a task, if a recovery occurs at this moment,
	// the recovery may complete after the agent has already finished stopping the task and returned an empty task list.
	// This could cause the scheduler to be unable to determine whether the task was never executed or has already completed.
	// It might lead to duplicate execution, but it ensures at-least-once delivery semantics.
	tasks := sch.taskStatusCollector.Collect(context.Background(), ips)
	for i := range ips {
		ip := ips[i]
		pod := pods[i]
		task := tasks[ip]
		if task == nil || pod == nil {
			continue
		}
		if tNode := sch.taskNodeByNameIndex[task.Name]; tNode != nil {
			recoverOneTaskNode(tNode, task, pod.Status.PodIP, pod.Name, sch.logger)
		}
	}
	return nil
}

func recoverOneTaskNode(tNode *taskNode, currentTask *api.Task, ip string, podName string, log logr.Logger) {
	tNode.Status = currentTask
	tNode.transTaskState(parseTaskState(currentTask), log)
	tNode.IP = ip
	tNode.PodName = podName
	if currentTask.DeletionTimestamp != nil {
		tNode.transSchState(stateReleasing, log)
	}
}
