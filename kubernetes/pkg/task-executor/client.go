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

package task_executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"k8s.io/klog/v2"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string) *Client {
	if baseURL == "" {
		klog.Warning("baseURL is empty, client may not work properly")
	}
	return &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{},
	}
}

// Set creates or updates a task on the remote server.
// If task is nil, it sends a delete request.
func (c *Client) Set(ctx context.Context, task *Task) (*Task, error) {
	if c == nil {
		return nil, fmt.Errorf("client is nil")
	}

	var req *http.Request
	var err error

	if task == nil {
		// Delete request - send nil to clear tasks
		req, err = http.NewRequestWithContext(ctx, "POST", c.baseURL+"/setTasks", bytes.NewReader([]byte("[]")))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
	} else {
		// Create/Update request
		data, err := json.Marshal([]Task{*task})
		if err != nil {
			return nil, fmt.Errorf("failed to marshal task: %w", err)
		}
		req, err = http.NewRequestWithContext(ctx, "POST", c.baseURL+"/setTasks", bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
	}

	req.Header.Set("Content-Type", "application/json")

	// Send request with retry
	var resp *http.Response
	resp, err = c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error after retries: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server error: status=%d, body=%s", resp.StatusCode, string(body))
	}

	// Parse response - expect array of tasks
	var tasks []Task
	if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if task != nil && len(tasks) > 0 {
		// Find the task we just set
		for i := range tasks {
			if tasks[i].Name == task.Name {
				return &tasks[i], nil
			}
		}
	}

	if task == nil {
		// Delete succeeded
		return nil, nil
	}

	return task, nil
}

// Get retrieves the current task list from the remote server.
func (c *Client) Get(ctx context.Context) (*Task, error) {
	if c == nil {
		return nil, fmt.Errorf("client is nil")
	}

	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/getTasks", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server error: status=%d, body=%s", resp.StatusCode, string(body))
	}

	// Parse response - expect array of tasks
	var tasks []Task
	if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Return the first task (single task mode)
	if len(tasks) > 0 {
		return &tasks[0], nil
	}

	// No tasks
	return nil, nil
}
