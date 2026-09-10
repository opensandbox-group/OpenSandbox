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
	"errors"
	"strings"
	"testing"

	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

type testTaskStatusClient struct {
	task *api.Task
	err  error
}

func (c *testTaskStatusClient) Get(context.Context) (*api.Task, error) {
	return c.task, c.err
}

func (c *testTaskStatusClient) Set(context.Context, *api.Task) (*api.Task, error) {
	return nil, nil
}

func TestDefaultTaskStatusCollectorCollectDistinguishesEmptyAndError(t *testing.T) {
	queryErr := errors.New("executor unavailable")
	activeTask := &api.Task{Name: "task-1"}
	collector := &defaultTaskStatusCollector{
		creator: func(ip string) taskClient {
			switch ip {
			case "10.0.0.1":
				return &testTaskStatusClient{}
			case "10.0.0.2":
				return &testTaskStatusClient{task: activeTask}
			default:
				return &testTaskStatusClient{err: queryErr}
			}
		},
		logger: testLogger,
	}

	tasks, err := collector.Collect(context.Background(), []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"})
	if !errors.Is(err, queryErr) {
		t.Fatalf("Collect() error = %v, want wrapped query error", err)
	}
	if !strings.Contains(err.Error(), "10.0.0.3") {
		t.Fatalf("Collect() error = %v, want IP context", err)
	}
	if task, ok := tasks["10.0.0.1"]; !ok || task != nil {
		t.Fatalf("Collect() empty result = (%v, %t), want explicit nil entry", task, ok)
	}
	if task, ok := tasks["10.0.0.2"]; !ok || task != activeTask {
		t.Fatalf("Collect() active result = (%v, %t), want active task", task, ok)
	}
	if _, ok := tasks["10.0.0.3"]; ok {
		t.Fatal("Collect() should omit the result for a failed query")
	}
}

func TestDefaultTaskStatusCollectorCollectWithoutErrors(t *testing.T) {
	collector := &defaultTaskStatusCollector{
		creator: func(string) taskClient {
			return &testTaskStatusClient{}
		},
		logger: testLogger,
	}

	tasks, err := collector.Collect(context.Background(), []string{"10.0.0.1"})
	if err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}
	if task, ok := tasks["10.0.0.1"]; !ok || task != nil {
		t.Fatalf("Collect() empty result = (%v, %t), want explicit nil entry", task, ok)
	}
}
