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

//go:build !windows
// +build !windows

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/alibaba/opensandbox/internal/safego"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
)

// getShell returns the preferred shell, falling back to sh if bash is not available.
// This is needed for Alpine-based Docker images that only have sh by default.
func getShell() string {
	if _, err := exec.LookPath("bash"); err == nil {
		return "bash"
	}
	return "sh"
}

func buildCredential(uid, gid *uint32) (*syscall.Credential, error) {
	if uid == nil && gid == nil {
		return nil, nil //nolint:nilnil
	}

	cred := &syscall.Credential{}
	if uid != nil {
		cred.Uid = *uid
		// Load user info to get primary GID and supplemental groups
		u, err := user.LookupId(strconv.FormatUint(uint64(*uid), 10))
		if err == nil {
			// Set primary GID if not explicitly provided
			if gid == nil {
				primaryGid, err := strconv.ParseUint(u.Gid, 10, 32)
				if err == nil {
					cred.Gid = uint32(primaryGid)
				}
			}

			// Load supplemental groups
			gids, err := u.GroupIds()
			if err == nil {
				for _, g := range gids {
					id, err := strconv.ParseUint(g, 10, 32)
					if err == nil {
						cred.Groups = append(cred.Groups, uint32(id))
					}
				}
			}
		}
	}

	// Override Gid if explicitly provided
	if gid != nil {
		cred.Gid = *gid
	}

	return cred, nil
}

// runCommand executes shell commands and streams their output.
func (c *Controller) runCommand(ctx context.Context, request *ExecuteCodeRequest) error {
	session := c.newContextID()

	signals := make(chan os.Signal, 1)
	defer close(signals)
	signal.Notify(signals)
	defer signal.Reset()

	stdout, stderr, err := c.stdLogDescriptor(session)
	if err != nil {
		return fmt.Errorf("failed to get stdlog descriptor: %w", err)
	}
	defer stdout.Close()
	defer stderr.Close()
	stdoutPath := c.stdoutFileName(session)
	stderrPath := c.stderrFileName(session)

	startAt := time.Now()
	log.Info("received command: %v", log.SanitizeCommand(request.Code))
	shell := getShell()
	cmd := exec.CommandContext(ctx, shell, "-c", request.Code)
	extraEnv := mergeExtraEnvs(loadExtraEnvFromFile(), request.Envs)
	cwd, err := pathutil.ExpandPathWithEnv(request.Cwd, extraEnv)
	if err != nil {
		return fmt.Errorf("resolve request cwd %s: %w", request.Cwd, err)
	}

	// Configure credentials and process group
	cred, err := buildCredential(request.Uid, request.Gid)
	if err != nil {
		return fmt.Errorf("failed to build credential: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Credential: cred,
	}

	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = mergeEnvs(os.Environ(), extraEnv)
	cmd.Dir = cwd

	done := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	safego.Go(func() {
		defer wg.Done()
		c.tailStdPipe(stdoutPath, request.wrapStdoutPipeHook(), done)
	})
	safego.Go(func() {
		defer wg.Done()
		c.tailStdPipe(stderrPath, request.wrapStderrPipeHook(), done)
	})

	err = cmd.Start()
	if err != nil {
		close(done)
		wg.Wait()
		request.Hooks.OnExecuteInit(session)
		request.Hooks.OnExecuteError(&execute.ErrorOutput{
			EName:     "CommandExecError",
			EValue:    err.Error(),
			Traceback: []string{err.Error()},
		})
		log.Error("CommandExecError: error starting commands: %v", err)
		return nil
	}

	kernel := &commandKernel{
		pid:          cmd.Process.Pid,
		stdoutPath:   stdoutPath,
		stderrPath:   stderrPath,
		startedAt:    startAt,
		running:      true,
		content:      request.Code,
		isBackground: false,
	}
	c.storeCommandKernel(session, kernel)
	request.Hooks.OnExecuteInit(session)

	safego.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-signals:
				if sig == nil {
					continue
				}
				// DO NOT forward syscall.SIGURG to children processes.
				if sig != syscall.SIGCHLD && sig != syscall.SIGURG {
					_ = syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
				}
			}
		}
	})

	err = cmd.Wait()
	close(done)
	wg.Wait()
	if err != nil {
		var eName, eValue string
		var eCode int
		var traceback []string

		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode := exitError.ExitCode()
			eName = "CommandExecError"
			eValue = strconv.Itoa(exitCode)
			eCode = exitCode
		} else {
			eName = "CommandExecError"
			eValue = err.Error()
			eCode = 1
		}
		traceback = []string{err.Error()}

		request.Hooks.OnExecuteError(&execute.ErrorOutput{
			EName:     eName,
			EValue:    eValue,
			Traceback: traceback,
		})

		log.Error("CommandExecError: error running commands: %v", err)
		c.markCommandFinished(session, eCode, err.Error())
		return nil
	}

	c.markCommandFinished(session, 0, "")
	request.Hooks.OnExecuteComplete(time.Since(startAt))
	return nil
}

// runBackgroundCommand executes shell commands in detached mode.
func (c *Controller) runBackgroundCommand(ctx context.Context, cancel context.CancelFunc, request *ExecuteCodeRequest) error {
	session := c.newContextID()
	request.Hooks.OnExecuteInit(session)

	pipe, err := c.combinedOutputDescriptor(session)
	if err != nil {
		cancel()
		return fmt.Errorf("failed to get combined output descriptor: %w", err)
	}
	stdoutPath := c.combinedOutputFileName(session)
	stderrPath := c.combinedOutputFileName(session)

	signals := make(chan os.Signal, 1)
	defer close(signals)
	signal.Notify(signals)
	defer signal.Reset()

	startAt := time.Now()
	log.Info("received command: %v", log.SanitizeCommand(request.Code))
	shell := getShell()
	cmd := exec.CommandContext(ctx, shell, "-c", request.Code)
	extraEnv := mergeExtraEnvs(loadExtraEnvFromFile(), request.Envs)
	cwd, err := pathutil.ExpandPathWithEnv(request.Cwd, extraEnv)
	if err != nil {
		cancel()
		return fmt.Errorf("resolve cwd: %w", err)
	}
	cmd.Dir = cwd
	// Configure credentials and process group
	cred, err := buildCredential(request.Uid, request.Gid)
	if err != nil {
		cancel()
		return fmt.Errorf("build credential: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Credential: cred,
	}

	cmd.Stdout = pipe
	cmd.Stderr = pipe
	cmd.Env = mergeEnvs(os.Environ(), extraEnv)

	// use DevNull as stdin so interactive programs exit immediately.
	devNull, err := os.Open(os.DevNull)
	if err == nil {
		cmd.Stdin = devNull
		defer devNull.Close()
	}

	err = cmd.Start()
	kernel := &commandKernel{
		pid:          -1,
		stdoutPath:   stdoutPath,
		stderrPath:   stderrPath,
		startedAt:    startAt,
		running:      true,
		content:      request.Code,
		isBackground: true,
	}
	if err != nil {
		cancel()
		log.Error("CommandExecError: error starting commands: %v", err)
		kernel.running = false
		c.storeCommandKernel(session, kernel)
		c.markCommandFinished(session, 255, err.Error())
		return fmt.Errorf("failed to start commands: %w", err)
	}

	safego.Go(func() {
		defer pipe.Close()

		kernel.running = true
		kernel.pid = cmd.Process.Pid
		c.storeCommandKernel(session, kernel)

		err = cmd.Wait()
		cancel()
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

	// ensure we kill the whole process group if the context is cancelled (e.g., timeout).
	safego.Go(func() {
		<-ctx.Done()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // best-effort
		}
	})

	request.Hooks.OnExecuteComplete(time.Since(startAt))
	return nil
}
