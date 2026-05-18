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

package runtime

import (
	"context"
	"errors"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter"
	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/log"
)

// runJupyter executes code through a Jupyter kernel.
func (c *Controller) runJupyter(ctx context.Context, request *ExecuteCodeRequest) error {
	if c.baseURL == "" || c.token == "" {
		return errors.New("language runtime server not configured, please check your image runtime")
	}
	if request.Context == "" {
		if c.getDefaultLanguageSession(request.Language) == "" {
			if err := c.createDefaultLanguageJupyterContext(request.Language); err != nil {
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
	defer kernel.client.DisconnectFromKernel(kernel.kernelID)

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
			log.Warning("context cancelled, try to interrupt kernel")
			err = kernel.client.InterruptKernel(kernel.kernelID)
			if err != nil {
				log.Error("interrupt kernel failed: %v", err)
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
			if stream.Text != "" && request.Hooks.OnExecuteStdout != nil {
				eid := request.NextEventID()
				request.Hooks.OnExecuteStdout(eid, stream.Text)
			}
		case execute.StreamStderr:
			if stream.Text != "" && request.Hooks.OnExecuteStderr != nil {
				eid := request.NextEventID()
				request.Hooks.OnExecuteStderr(eid, stream.Text)
			}
		default:
		}
	}
}

// setWorkingDir configures the working directory for a kernel session.
func (c *Controller) setWorkingDir(_ *jupyterKernel, _ *CreateContextRequest) error {
	return nil
}

// getJupyterKernel retrieves a kernel connection from the session map.
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
