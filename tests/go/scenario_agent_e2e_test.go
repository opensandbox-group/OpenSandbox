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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
)

func getLLMEndpoint() string {
	if v := os.Getenv("LLM_ENDPOINT"); v != "" {
		return v
	}
	domain := os.Getenv("OPENSANDBOX_TEST_DOMAIN")
	if domain == "" {
		return ""
	}
	protocol := os.Getenv("OPENSANDBOX_TEST_PROTOCOL")
	if protocol == "" {
		protocol = "https"
	}
	return fmt.Sprintf("%s://%s/v1", protocol, domain)
}

func getLLMModel() string {
	if v := os.Getenv("LLM_MODEL"); v != "" {
		return v
	}
	return "azure/gpt-4o-mini"
}

func chatCompletion(ctx context.Context, endpoint, model string, messages []map[string]string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   messages,
		"max_tokens": 1024,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("LLM returned %d: %s", resp.StatusCode, string(data))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parse LLM response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in LLM response")
	}
	return result.Choices[0].Message.Content, nil
}

func extractCode(text string) string {
	start := strings.Index(text, "```python")
	if start == -1 {
		start = strings.Index(text, "```")
		if start == -1 {
			return ""
		}
		start += 3
	} else {
		start += 9
	}
	if nl := strings.Index(text[start:], "\n"); nl != -1 {
		start += nl + 1
	}
	end := strings.Index(text[start:], "```")
	if end == -1 {
		return text[start:]
	}
	return strings.TrimSpace(text[start : start+end])
}

func TestScenario_SimpleAgentLoop(t *testing.T) {
	llmEndpoint := getLLMEndpoint()
	if llmEndpoint == "" {
		t.Skip("LLM_ENDPOINT or OPENSANDBOX_TEST_DOMAIN not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	config := getConnectionConfig(t)
	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image: getSandboxImage(),
		Env:   map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
	})
	require.NoError(t, err)
	defer sb.Kill(context.Background())
	t.Logf("Sandbox ready: %s", sb.ID())

	task := "Write Python code that calculates the first 10 Fibonacci numbers and prints them as a comma-separated list. Only output the code block, nothing else."
	t.Logf("Task: %s", task)

	llmResponse, err := chatCompletion(ctx, llmEndpoint, getLLMModel(), []map[string]string{
		{"role": "system", "content": "You are a coding assistant. Respond ONLY with a Python code block. No explanation."},
		{"role": "user", "content": task},
	})
	require.NoError(t, err)
	t.Logf("LLM response:\n%s", llmResponse)

	code := extractCode(llmResponse)
	if code == "" {
		code = strings.TrimSpace(llmResponse)
	}
	t.Logf("Extracted code:\n%s", code)

	writeCmd := fmt.Sprintf("cat > /tmp/agent_task.py << 'PYEOF'\n%s\nPYEOF", code)
	writeResult, writeErr := sb.RunCommand(ctx, writeCmd, nil)
	require.NoError(t, writeErr)
	if writeResult.ExitCode != nil {
		require.Equal(t, 0, *writeResult.ExitCode, "write code to sandbox: %s", writeResult.Text())
	}
	exec, err := sb.RunCommand(ctx, "python3 /tmp/agent_task.py", nil)
	require.NoError(t, err)

	output := exec.Text()
	t.Logf("Execution output: %s", output)

	if exec.ExitCode != nil {
		require.Equal(t, 0, *exec.ExitCode, "code execution exit code")
	}

	require.Contains(t, output, "34", "expected Fibonacci output")
	require.Contains(t, output, "8")
	require.True(t, strings.Contains(output, "13") || strings.Contains(output, "21") || strings.Contains(output, "5"),
		"expected mid-sequence Fibonacci digits (5/13/21), got: %q", output)
	t.Log("Agent loop completed successfully: task → LLM → code → execute → result")
}

func TestScenario_SandboxToolUse(t *testing.T) {
	llmEndpoint := getLLMEndpoint()
	if llmEndpoint == "" {
		t.Skip("LLM_ENDPOINT or OPENSANDBOX_TEST_DOMAIN not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	config := getConnectionConfig(t)
	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image: getSandboxImage(),
		Env:   map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
	})
	require.NoError(t, err)
	defer sb.Kill(context.Background())

	reply, err := chatCompletion(ctx, llmEndpoint, getLLMModel(), []map[string]string{
		{"role": "system", "content": "You have access to a Linux shell. Respond ONLY with the exact shell command to run. No explanation, no code blocks, just the raw command."},
		{"role": "user", "content": "What command shows the Linux kernel version, CPU count, and total memory in one line?"},
	})
	require.NoError(t, err)
	command := strings.TrimSpace(reply)
	command = strings.TrimPrefix(command, "```bash\n")
	command = strings.TrimPrefix(command, "```\n")
	command = strings.TrimSuffix(command, "\n```")
	command = strings.TrimPrefix(command, "```")
	command = strings.TrimSpace(command)
	t.Logf("LLM suggested command: %s", command)

	exec, err := sb.RunCommand(ctx, command, nil)
	require.NoError(t, err)
	shellOutput := exec.Text()
	t.Logf("Shell output: %s", shellOutput)

	interpretation, err := chatCompletion(ctx, llmEndpoint, getLLMModel(), []map[string]string{
		{"role": "system", "content": "Summarize the system information in one sentence."},
		{"role": "user", "content": fmt.Sprintf("Shell output:\n%s", shellOutput)},
	})
	require.NoError(t, err)
	t.Logf("LLM interpretation: %s", interpretation)

	require.NotEmpty(t, interpretation, "LLM produced no interpretation")
	t.Log("Tool-use agent completed: task → LLM → shell → LLM → answer")
}
