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

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter"
	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const maxJupyterRuntimeFileSize = 64 * 1024

var errJupyterNotConfigured = errors.New("language runtime server not configured, please check your image runtime")

type jupyterRuntimeServerInfo struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// runJupyter executes code through a Jupyter kernel.
func (c *Controller) runJupyter(ctx context.Context, request *ExecuteCodeRequest) error {
	if c.baseURL == "" {
		return errJupyterNotConfigured
	}
	if request.Context == "" {
		if c.getDefaultLanguageSession(request.Language) == "" {
			if err := c.createDefaultLanguageJupyterContext(ctx, request.Language); err != nil {
				return err
			}
		}
	}

	var targetSessionID string
	if request.Context == "" {
		targetSessionID = c.getDefaultLanguageSession(request.Language)
	} else {
		targetSessionID = request.Context
	}

	kernel := c.getJupyterKernel(targetSessionID)
	if kernel == nil {
		if c.jupyterToken() == "" {
			return errJupyterNotConfigured
		}
		return ErrContextNotFound
	}

	request.SetDefaultHooks()
	request.Hooks.OnExecuteInit(targetSessionID)

	return c.runJupyterCode(ctx, kernel, request)
}

// runJupyterCode streams execution results for a single kernel.
//
//nolint:gocognit // complex due to hook handling; refactor later
func (c *Controller) runJupyterCode(ctx context.Context, kernel *jupyterKernel, request *ExecuteCodeRequest) error {
	if !kernel.mu.TryLock() {
		return errors.New("session is busy")
	}
	defer kernel.mu.Unlock()

	err := kernel.client.ConnectToKernel(kernel.kernelID)
	if err != nil {
		return err
	}
	defer kernel.client.DisconnectFromKernel()

	results := make(chan *execute.ExecutionResult, 10)

	err = kernel.client.ExecuteCodeStream(kernel.kernelID, request.Code, results)
	if err != nil {
		return err
	}

	for {
		select {
		case result := <-results:
			if result == nil {
				return nil
			}
			dispatchExecutionResultHooks(request, result)

		case <-ctx.Done():
			log.Warn("jupyter: context cancelled; interrupting kernel")
			err = kernel.client.InterruptKernel(kernel.kernelID)
			if err != nil {
				log.Error("jupyter: interrupt kernel: %v", err)
			}

			request.Hooks.OnExecuteError(&execute.ErrorOutput{
				EName:  "ContextCancelled",
				EValue: "Interrupt kernel",
			})
			return errors.New("context cancelled, interrupt kernel")
		}
	}
}

func dispatchExecutionResultHooks(request *ExecuteCodeRequest, result *execute.ExecutionResult) {
	if result.ExecutionCount > 0 || len(result.ExecutionData) > 0 {
		request.Hooks.OnExecuteResult(result.ExecutionData, result.ExecutionCount)
	}

	if result.Status != "" {
		request.Hooks.OnExecuteStatus(result.Status)
	}

	if result.Error != nil {
		request.Hooks.OnExecuteError(result.Error)
	}

	// Treat completion as success-only terminal signal. For failed executions,
	// error should be the terminal event to avoid losing error delivery.
	if result.ExecutionTime > 0 && result.Error == nil {
		request.Hooks.OnExecuteComplete(result.ExecutionTime)
	}

	for _, stream := range result.Stream {
		switch stream.Name {
		case execute.StreamStdout:
			request.Hooks.OnExecuteStdout(stream.Text)
		case execute.StreamStderr:
			request.Hooks.OnExecuteStderr(stream.Text)
		}
	}
}

func (c *Controller) getJupyterKernel(sessionID string) *jupyterKernel {
	if v, ok := c.jupyterClientMap.Load(sessionID); ok {
		if kernel, ok := v.(*jupyterKernel); ok {
			return kernel
		}
	}
	return nil
}

// searchKernel finds a kernel spec name for the given language.
func (c *Controller) searchKernel(client *jupyter.Client, language Language) (string, error) {
	specs, err := client.GetKernelSpecs()
	if err != nil {
		return "", err
	}

	if len(specs.Kernelspecs) == 0 {
		return "", errors.New("no kernel specs found")
	}

	var kernelName string
	for name, spec := range specs.Kernelspecs {
		if name == "python3" {
			continue
		}

		if spec.Spec.Language == language.String() {
			kernelName = name
		}
	}
	if kernelName == "" {
		return "", errors.New("no kernel specs found")
	}

	return kernelName, nil
}

func (c *Controller) jupyterToken() string {
	tokens := c.jupyterTokens()
	if len(tokens) == 0 {
		return ""
	}
	return tokens[0]
}

func (c *Controller) jupyterTokens() []string {
	if c.token != "" {
		return []string{c.token}
	}
	return c.discoverJupyterTokens()
}

func (c *Controller) expectedJupyterRuntimeUID() int {
	if c.jupyterRuntimeUID != nil {
		return *c.jupyterRuntimeUID
	}
	return os.Geteuid()
}

type discoveredJupyterToken struct {
	token   string
	modTime time.Time
}

// discoverJupyterTokens reads candidate tokens written by a standalone
// code-interpreter entrypoint in /tmp/opensandbox-jupyter.*/jpserver-*.json
// when execd is launched without bootstrap.sh (for example via fast-sandbox
// infraComponents), ordered from newest to oldest.
func (c *Controller) discoverJupyterTokens() []string {
	targetPort := jupyterPortFromBaseURL(c.baseURL)
	if targetPort <= 0 {
		return nil
	}
	root := c.jupyterRuntimeRoot
	if root == "" {
		root = "/tmp"
	}
	matches, err := filepath.Glob(filepath.Join(root, "opensandbox-jupyter.*", "jpserver-*.json"))
	if err != nil {
		return nil
	}

	expectedUID := c.expectedJupyterRuntimeUID()
	candidates := make([]discoveredJupyterToken, 0, len(matches))
	for _, match := range matches {
		token, modTime, ok := readJupyterRuntimeToken(match, targetPort, expectedUID)
		if !ok {
			continue
		}
		candidates = append(candidates, discoveredJupyterToken{
			token:   token,
			modTime: modTime,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})

	tokens := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, exists := seen[candidate.token]; exists {
			continue
		}
		seen[candidate.token] = struct{}{}
		tokens = append(tokens, candidate.token)
	}
	return tokens
}

func readJupyterRuntimeToken(filePath string, targetPort int, expectedUID int) (string, time.Time, bool) {
	dirInfo, err := os.Lstat(filepath.Dir(filePath))
	if err != nil || !dirInfo.Mode().IsDir() || dirInfo.Mode().Perm()&0o077 != 0 {
		return "", time.Time{}, false
	}
	if uid, ok := runtimeFileOwnerUID(dirInfo); ok && uid != expectedUID {
		return "", time.Time{}, false
	}

	file, err := openRuntimeFileNoFollow(filePath)
	if err != nil {
		return "", time.Time{}, false
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode().Perm()&0o022 != 0 {
		return "", time.Time{}, false
	}
	if fileInfo.Size() <= 0 || fileInfo.Size() > maxJupyterRuntimeFileSize {
		return "", time.Time{}, false
	}
	if uid, ok := runtimeFileOwnerUID(fileInfo); ok && uid != expectedUID {
		return "", time.Time{}, false
	}

	data, err := io.ReadAll(io.LimitReader(file, maxJupyterRuntimeFileSize+1))
	if err != nil || int64(len(data)) > maxJupyterRuntimeFileSize {
		return "", time.Time{}, false
	}

	var info jupyterRuntimeServerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return "", time.Time{}, false
	}
	token := strings.TrimSpace(info.Token)
	if token == "" || info.Port != targetPort {
		return "", time.Time{}, false
	}
	return token, fileInfo.ModTime(), true
}

func jupyterPortFromBaseURL(baseURL string) int {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return 0
	}
	if portStr := parsed.Port(); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 {
			return 0
		}
		return port
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		return 80
	case "https":
		return 443
	default:
		return 0
	}
}
