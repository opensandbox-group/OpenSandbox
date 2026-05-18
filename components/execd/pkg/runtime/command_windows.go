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

//go:build windows
// +build windows

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
	"github.com/alibaba/opensandbox/internal/safego"
)

// runCommand executes shell commands and streams their output on Windows.
func (c *Controller) runCommand(ctx context.Context, request *ExecuteCodeRequest) error {
	session := c.newContextID()
	request.Hooks.OnExecuteInit(session)

	stdout, stderr, err := c.stdLogDescriptor(session)
	if err != nil {
		return fmt.Errorf("failed to get stdlog descriptor: %w", err)
	}

	startAt := time.Now()
	log.Info("received command: %v", log.SanitizeCommand(request.Code))
	cmd := exec.CommandContext(ctx, "cmd", "/C", request.Code)
	extraEnv := mergeExtraEnvs(loadExtraEnvFromFile(), request.Envs)
	cwd, err := pathutil.ExpandPathWithEnv(request.Cwd, extraEnv)
	if err != nil {
		return fmt.Errorf("resolve cwd: %w", err)
	}

	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Dir = cwd
	cmd.Env = mergeEnvs(os.Environ(), extraEnv)

	done := make(chan struct{}, 1)
	safego.Go(func() {
		c.tailStdPipe(c.stdoutFileName(session), request.wrapStdoutPipeHook(), done)
	})
	safego.Go(func() {
		c.tailStdPipe(c.stderrFileName(session), request.wrapStderrPipeHook(), done)
	})

	err = cmd.Start()
	if err != nil {
		request.Hooks.OnExecuteError(&execute.ErrorOutput{EName: "CommandExecError", EValue: err.Error()})
		log.Error("CommandExecError: error starting commands: %v", err)
		return nil
	}

	kernel := &commandKernel{
		pid:          cmd.Process.Pid,
		content:      request.Code,
		isBackground: false,
	}
	c.storeCommandKernel(session, kernel)

	err = cmd.Wait()
	close(done)
	if err != nil {
		var eName, eValue string
		var traceback []string

		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode := exitError.ExitCode()
			eName = "CommandExecError"
			eValue = strconv.Itoa(exitCode)
		} else {
			eName = "CommandExecError"
			eValue = err.Error()
		}
		traceback = []string{err.Error()}

		request.Hooks.OnExecuteError(&execute.ErrorOutput{
			EName:     eName,
			EValue:    eValue,
			Traceback: traceback,
		})

		log.Error("CommandExecError: error running commands: %v", err)
		return nil
	}
	request.Hooks.OnExecuteComplete(time.Since(startAt))
	return nil
}

// runBackgroundCommand executes shell commands in detached mode on Windows.
func (c *Controller) runBackgroundCommand(ctx context.Context, cancel context.CancelFunc, request *ExecuteCodeRequest) error {
	session := c.newContextID()
	request.Hooks.OnExecuteInit(session)

	pipe, err := c.combinedOutputDescriptor(session)
	if err != nil {
		return fmt.Errorf("failed to get combined output descriptor: %w", err)
	}
	stdoutPath := c.combinedOutputFileName(session)
	stderrPath := c.combinedOutputFileName(session)

	startAt := time.Now()
	log.Info("received command: %v", log.SanitizeCommand(request.Code))
	cmd := exec.CommandContext(ctx, "cmd", "/C", request.Code)
	extraEnv := mergeExtraEnvs(loadExtraEnvFromFile(), request.Envs)
	cwd, err := pathutil.ExpandPathWithEnv(request.Cwd, extraEnv)
	if err != nil {
		return fmt.Errorf("resolve cwd: %w", err)
	}

	cmd.Dir = cwd
	cmd.Stdout = pipe
	cmd.Stderr = pipe
	cmd.Env = mergeEnvs(os.Environ(), extraEnv)

	devNull, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0) // best-effort, ignore error
	cmd.Stdin = devNull

	safego.Go(func() {
		err := cmd.Start()
		if err != nil {
			log.Error("CommandExecError: error starting commands: %v", err)
			pipe.Close() // best-effort
			cancel()
			return
		}

		kernel := &commandKernel{
			pid:          cmd.Process.Pid,
			content:      request.Code,
			stdoutPath:   stdoutPath,
			stderrPath:   stderrPath,
			startedAt:    startAt,
			running:      true,
			isBackground: true,
		}
		c.storeCommandKernel(session, kernel)

		safego.Go(func() {
			<-ctx.Done()
			if cmd.Process != nil {
				_ = cmd.Process.Kill() // best-effort
			}
		})

		err = cmd.Wait()
		cancel()
		pipe.Close()    // best-effort
		devNull.Close() // best-effort

		if err != nil {
			log.Error("CommandExecError: error running commands: %v", err)
			exitCode := 1
			var exitError *exec.ExitError
			if errors.As(err, &exitError) {
				exitCode = exitError.ExitCode()
			}
			c.markCommandFinished(session, exitCode, err.Error())
			return
		}
		c.markCommandFinished(session, 0, "")
	})

	request.Hooks.OnExecuteComplete(time.Since(startAt))
	return nil
}
