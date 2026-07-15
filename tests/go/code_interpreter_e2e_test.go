// Copyright 2026 Alibaba Group Holding Ltd.
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

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
)

func createCodeInterpreter(t *testing.T) (context.Context, *opensandbox.CodeInterpreter) {
	t.Helper()
	if os.Getenv("RUN_CODE_INTERPRETER_E2E") != "true" {
		t.Skip("Set RUN_CODE_INTERPRETER_E2E=true to run code interpreter e2e tests")
	}
	config := connectionConfigForStreaming(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	ci, err := opensandbox.CreateCodeInterpreter(ctx, config, opensandbox.CodeInterpreterCreateOptions{
		Metadata: map[string]string{
			"test": "go-e2e-code-interpreter",
		},
		ReadyTimeout:        60 * time.Second,
		HealthCheckInterval: 500 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { ci.Kill(context.Background()) })
	return ctx, ci
}

func TestCodeInterpreter_CreateAndPing(t *testing.T) {
	ctx, ci := createCodeInterpreter(t)

	require.True(t, ci.IsHealthy(ctx), "code interpreter should be healthy")

	metrics, err := ci.GetMetrics(ctx)
	require.NoError(t, err)
	t.Logf("Code interpreter metrics: cpu=%.0f, mem=%.0fMiB", metrics.CPUCount, metrics.MemTotalMB)
}

func TestCodeInterpreter_PythonExecution(t *testing.T) {
	ctx, ci := createCodeInterpreter(t)

	exec, err := ci.Execute(ctx, "python", `print("hello from python")`, nil)
	require.NoError(t, err)

	text := exec.Text()
	require.Contains(t, text, "hello from python")
	t.Logf("Python output: %s", text)
}

func TestCodeInterpreter_PythonContextPersistence(t *testing.T) {
	ctx, ci := createCodeInterpreter(t)

	codeCtx, err := ci.CreateContext(ctx, opensandbox.CreateContextRequest{Language: "python"})
	require.NoError(t, err)
	t.Logf("Created context: %s", codeCtx.ID)

	exec, err := ci.ExecuteInContext(ctx, codeCtx.ID, "python", `x = 42`, nil)
	require.NoError(t, err)
	_ = exec

	exec, err = ci.ExecuteInContext(ctx, codeCtx.ID, "python", `print(f"x is {x}")`, nil)
	require.NoError(t, err)

	text := exec.Text()
	require.Contains(t, text, "x is 42")
	t.Logf("Context persistence: %s", text)

	err = ci.DeleteContext(ctx, codeCtx.ID)
	if err != nil {
		t.Logf("DeleteContext: %v", err)
	}
}

func TestCodeInterpreter_ContextManagement(t *testing.T) {
	ctx, ci := createCodeInterpreter(t)

	codeCtx, err := ci.CreateContext(ctx, opensandbox.CreateContextRequest{Language: "python"})
	require.NoError(t, err)

	contexts, err := ci.ListContexts(ctx, "python")
	require.NoError(t, err)
	require.NotEmpty(t, contexts, "expected at least one context")
	t.Logf("Listed %d python contexts", len(contexts))

	err = ci.DeleteContext(ctx, codeCtx.ID)
	require.NoError(t, err)
	t.Log("Context management passed")
}

func TestCodeInterpreter_ContextIsolation(t *testing.T) {
	if os.Getenv("RUN_CODE_INTERPRETER_E2E") != "true" {
		t.Skip("Set RUN_CODE_INTERPRETER_E2E=true to run code interpreter e2e tests")
	}

	config := connectionConfigForStreaming(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ci, err := opensandbox.CreateCodeInterpreter(ctx, config, opensandbox.CodeInterpreterCreateOptions{
		ReadyTimeout:        60 * time.Second,
		HealthCheckInterval: 500 * time.Millisecond,
	})
	require.NoError(t, err)
	defer ci.Kill(context.Background())

	ctx1, err := ci.CreateContext(ctx, opensandbox.CreateContextRequest{Language: "python"})
	require.NoError(t, err)
	ctx2, err := ci.CreateContext(ctx, opensandbox.CreateContextRequest{Language: "python"})
	require.NoError(t, err)

	ci.ExecuteInContext(ctx, ctx1.ID, "python", `isolated_var = "ctx1_only"`, nil)

	exec, err := ci.ExecuteInContext(ctx, ctx2.ID, "python", `print("ISOLATED") if "isolated_var" not in dir() else print(isolated_var)`, nil)
	require.NoError(t, err)

	text := exec.Text()
	require.Contains(t, text, "ISOLATED")
	t.Log("Context isolation verified")

	ci.DeleteContext(ctx, ctx1.ID)
	ci.DeleteContext(ctx, ctx2.ID)
}

func TestCodeInterpreter_ExecutionWithHandlers(t *testing.T) {
	ctx, ci := createCodeInterpreter(t)

	var stdoutLines []string
	handlers := &opensandbox.ExecutionHandlers{
		OnStdout: func(msg opensandbox.OutputMessage) error {
			stdoutLines = append(stdoutLines, msg.Text)
			return nil
		},
	}

	_, err := ci.Execute(ctx, "python", `
for i in range(3):
    print(f"line {i}")
`, handlers)
	require.NoError(t, err)

	require.NotEmpty(t, stdoutLines, "expected handler to receive stdout")
	t.Logf("Handler received %d stdout events", len(stdoutLines))
}

func TestCodeInterpreter_GetContextAndDeleteByLanguage(t *testing.T) {
	ctx, ci := createCodeInterpreter(t)
	execd := newExecdClientForSandbox(t, ctx, ci.Sandbox)

	codeCtx, err := ci.CreateContext(ctx, opensandbox.CreateContextRequest{Language: "python"})
	require.NoError(t, err)

	got, err := execd.GetContext(ctx, codeCtx.ID)
	require.NoError(t, err)
	require.Equal(t, codeCtx.ID, got.ID)

	_, err = ci.CreateContext(ctx, opensandbox.CreateContextRequest{Language: "python"})
	require.NoError(t, err)

	require.NoError(t, execd.DeleteContextsByLanguage(ctx, "python"))

	contexts, err := ci.ListContexts(ctx, "python")
	require.NoError(t, err)
	require.Len(t, contexts, 0)
}
