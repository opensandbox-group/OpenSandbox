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

package utils

import "os"

var (
	// ControllerImage is the controller manager image
	// Can be overridden via CONTROLLER_IMG env var
	ControllerImage = getEnv("CONTROLLER_IMG", "controller:dev")

	// TaskExecutorImage is the task-executor image
	// Can be overridden via TASK_EXECUTOR_IMG env var
	TaskExecutorImage = getEnv("TASK_EXECUTOR_IMG", "task-executor:dev")

	// ImageCommitterImage is the image-committer image
	// Can be overridden via IMAGE_COMMITTER_IMG env var
	ImageCommitterImage = getEnv("IMAGE_COMMITTER_IMG", "image-committer:dev")

	// SandboxImage is the image used for sandbox containers in tests
	// Always uses TaskExecutorImage to ensure the image is available in Kind
	SandboxImage = TaskExecutorImage
)

func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
