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

package main

import (
	"context"
	"fmt"
	"log"
	"time"

	taskexecutor "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

func main() {
	baseURL := "http://localhost:5758"
	client := taskexecutor.NewClient(baseURL)
	ctx := context.Background()

	fmt.Printf("Connecting to Task Executor at %s...\n", baseURL)

	taskName := "example-task"
	newTask := &taskexecutor.Task{
		Name: taskName,
		Process: &taskexecutor.Process{
			Command: []string{"sh", "-c"},
			Args:    []string{"echo 'Hello from SDK example!' && sleep 2 && echo 'Task done.'"},
		},
	}

	fmt.Printf("Submitting task '%s'...\n", taskName)
	createdTask, err := client.Set(ctx, newTask)
	if err != nil {
		log.Fatalf("Failed to set task: %v", err)
	}
	fmt.Printf("Task submitted successfully. Initial state: %v\n", getTaskState(createdTask))

	fmt.Println("Polling task status...")
	for i := 0; i < 10; i++ {
		currentTask, err := client.Get(ctx)
		if err != nil {
			log.Printf("Error getting task: %v", err)
			continue
		}

		if currentTask == nil {
			fmt.Println("No task found.")
			break
		}

		state := getTaskState(currentTask)
		fmt.Printf("Current state: %s\n", state)

		// Check if task is finished
		if currentTask.ProcessStatus.Terminated != nil {
			fmt.Printf("Task finished with exit code: %d\n", currentTask.ProcessStatus.Terminated.ExitCode)
			break
		}

		time.Sleep(500 * time.Millisecond)
	}

	// Clean up (pass nil to clear tasks)
	fmt.Println("Cleaning up...")
	_, err = client.Set(ctx, nil)
	if err != nil {
		log.Printf("Failed to clear tasks: %v", err)
	} else {
		fmt.Println("Tasks cleared.")
	}
}

// getTaskState returns a string representation of the task state
func getTaskState(task *taskexecutor.Task) string {
	if task == nil {
		return "Unknown"
	}
	if task.ProcessStatus.Running != nil {
		return "Running"
	}
	if task.ProcessStatus.Terminated != nil {
		return "Terminated"
	}
	if task.ProcessStatus.Waiting != nil {
		return fmt.Sprintf("Waiting (%s)", task.ProcessStatus.Waiting.Reason)
	}
	return "Pending"
}
