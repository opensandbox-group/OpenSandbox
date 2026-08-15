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

package jupyter

import (
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
)

// TestLiveServerIntegration tests SDK integration with a real Jupyter server
func TestLiveServerIntegration(t *testing.T) {
	jupyterURL := getEnv("JUPYTER_URL", "")
	jupyterToken := getEnv("JUPYTER_TOKEN", "")
	if jupyterURL == "" || jupyterToken == "" {
		t.Skip("JUPYTER_URL and JUPYTER_TOKEN environment variables must be set to run this test")
	}

	t.Logf("Connecting to Jupyter server: %s", jupyterURL)

	httpClient := &http.Client{
		Transport: &AuthTransport{
			Token: jupyterToken,
			Base:  http.DefaultTransport,
		},
	}

	client := NewClient(jupyterURL,
		WithToken(jupyterToken), // Keep Token setting to support ValidateAuth and WebSocket connections
		WithHTTPClient(httpClient))

	t.Run("Validate Authentication", func(t *testing.T) {
		status, err := client.ValidateAuth()
		if err != nil {
			t.Fatalf("Authentication validation failed: %v", err)
		}
		if status != "ok" {
			t.Errorf("Authentication status incorrect, expected 'ok', got '%s'", status)
		}
		t.Logf("Authentication validation successful! Status: %s", status)
	})

	var kernelName string
	t.Run("Get Kernel Specs", func(t *testing.T) {
		specs, err := client.GetKernelSpecs()
		if err != nil {
			t.Fatalf("Failed to get kernel specs: %v", err)
		}
		if specs.Default == "" {
			t.Errorf("No default kernel")
		}
		if len(specs.Kernelspecs) == 0 {
			t.Errorf("No available kernels")
		}

		// Use default kernel or Python kernel (if available)
		kernelName = specs.Default
		for name, spec := range specs.Kernelspecs {
			if spec.Spec.Language == "python" {
				kernelName = name
				break
			}
		}

		t.Logf("Get kernel specs successful! Default kernel: %s, Selected kernel: %s", specs.Default, kernelName)
		t.Logf("Available kernels: %v", specs.Kernelspecs)
	})

	t.Run("List Sessions", func(t *testing.T) {
		sessions, err := client.ListSessions()
		if err != nil {
			t.Fatalf("Failed to list sessions: %v", err)
		}
		t.Logf("List sessions successful! Number of existing sessions: %d", len(sessions))
		for i, s := range sessions {
			t.Logf("Session %d: ID=%s, Path=%s, Kernel=%s", i+1, s.ID, s.Path, s.Kernel.Name)
		}
	})

	var sessionID string
	t.Run("Create Session", func(t *testing.T) {
		sessionName := fmt.Sprintf("test-session-%d", time.Now().Unix())
		sessionPath := "/test-notebook.ipynb"

		session, err := client.CreateSession(sessionName, sessionPath, kernelName)
		if err != nil {
			t.Fatalf("Failed to create session: %v", err)
		}
		if session.ID == "" {
			t.Errorf("Created session has no ID")
		}
		if session.Kernel.ID == "" {
			t.Errorf("Created session has no kernel ID")
		}

		// Save session ID for subsequent tests
		sessionID = session.ID

		t.Logf("Create session successful! Session ID: %s, Kernel ID: %s", session.ID, session.Kernel.ID)
	})

	var kernelID string
	t.Run("Get Session", func(t *testing.T) {
		if sessionID == "" {
			t.Skip("No session ID, skipping test")
		}

		session, err := client.GetSession(sessionID)
		if err != nil {
			t.Fatalf("Failed to get session: %v", err)
		}
		if session.ID != sessionID {
			t.Errorf("Session ID mismatch, expected '%s', got '%s'", sessionID, session.ID)
		}

		// Save kernel ID for subsequent tests
		kernelID = session.Kernel.ID

		t.Logf("Get session successful! Session name: %s, Kernel name: %s", session.Name, session.Kernel.Name)
	})

	t.Run("List Kernels", func(t *testing.T) {
		kernels, err := client.ListKernels()
		if err != nil {
			t.Fatalf("Failed to list kernels: %v", err)
		}
		t.Logf("List kernels successful! Number of kernels: %d", len(kernels))
		for i, k := range kernels {
			t.Logf("Kernel %d: ID=%s, Name=%s, State=%s", i+1, k.ID, k.Name, k.ExecutionState)
		}

		if kernelID != "" {
			found := false
			for _, k := range kernels {
				if k.ID == kernelID {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Cannot find created kernel in kernel list ID=%s", kernelID)
			}
		}
	})

	t.Run("Execute Code", func(t *testing.T) {
		if kernelID == "" {
			t.Skip("No kernel ID, skipping test")
		}

		err := client.ConnectToKernel(kernelID)
		if err != nil {
			t.Fatalf("Failed to connect to kernel: %v", err)
		}
		defer client.DisconnectFromKernel()

		code := "print('Hello, Jupyter!')\nresult = 2 + 2\nresult"
		t.Logf("Executing code:\n%s", code)

		err = client.ExecuteCodeWithCallback(code, execute.CallbackHandler{})
		if err != nil {
			t.Fatalf("Failed to execute code: %v", err)
		}
	})

	t.Run("Execute Code", func(t *testing.T) {
		if kernelID == "" {
			t.Skip("No kernel ID, skipping test")
		}

		err := client.ConnectToKernel(kernelID)
		if err != nil {
			t.Fatalf("Failed to connect to kernel: %v", err)
		}
		defer client.DisconnectFromKernel()

		code := "print(f'2 + 2 = {result}')\nresult"
		t.Logf("Executing code:\n%s", code)

		err = client.ExecuteCodeWithCallback(code, execute.CallbackHandler{})
		if err != nil {
			t.Fatalf("Failed to execute code: %v", err)
		}
	})

	t.Run("Execute Complex Code", func(t *testing.T) {
		if kernelID == "" {
			t.Skip("No kernel ID, skipping test")
		}

		err := client.ConnectToKernel(kernelID)
		if err != nil {
			t.Fatalf("Failed to connect to kernel: %v", err)
		}
		defer client.DisconnectFromKernel()

		code := `
# Display table data
import pandas as pd
import numpy as np
try:
    df = pd.DataFrame({
        'A': np.random.rand(5),
        'B': np.random.rand(5)
    })
    display(df)
    print("DataFrame created successfully")
except Exception as e:
    print(f"Error creating DataFrame: {e}")

# Generate error
try:
    print(undefined_variable)
except Exception as e:
    print(f"Expected error: {e}")

# Return dictionary
{'hello': 'world', 'number': 42}
`

		t.Logf("Executing complex code...")

		err = client.ExecuteCodeWithCallback(code, execute.CallbackHandler{})
		if err != nil {
			t.Fatalf("Failed to execute complex code: %v", err)
		}
	})

	t.Run("Restart Kernel", func(t *testing.T) {
		if kernelID == "" {
			t.Skip("No kernel ID, skipping test")
		}

		restarted, err := client.RestartKernel(kernelID)
		if err != nil {
			t.Fatalf("Failed to restart kernel: %v", err)
		}

		time.Sleep(2 * time.Second)

		kernel, err := client.GetKernel(kernelID)
		if err != nil {
			t.Fatalf("Failed to get kernel: %v", err)
		}

		t.Logf("Restart kernel successful! Restart status: %v, Kernel state: %s", restarted, kernel.ExecutionState)
	})

	t.Run("Close Session", func(t *testing.T) {
		if sessionID == "" {
			t.Skip("No session ID, skipping test")
		}

		err := client.DeleteSession(sessionID)
		if err != nil {
			t.Fatalf("Failed to delete session: %v", err)
		}

		sessions, err := client.ListSessions()
		if err != nil {
			t.Fatalf("Failed to list sessions: %v", err)
		}

		for _, s := range sessions {
			if s.ID == sessionID {
				t.Errorf("Session still exists, not properly deleted ID=%s", sessionID)
				break
			}
		}

		t.Logf("Close session successful!")
	})
}

// Helper function: Get environment variable, use default value if not exists
func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}
