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

//go:build !windows
// +build !windows

package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
)

const (
	envDumpStartMarker = "__ENV_DUMP_START__"
	envDumpEndMarker   = "__ENV_DUMP_END__"
	exitMarkerPrefix   = "__EXIT_CODE__:"
	pwdMarkerPrefix    = "__PWD__:"
)

func (c *Controller) createBashSession(req *CreateContextRequest) (string, error) {
	resolvedCwd, err := pathutil.ExpandPath(req.Cwd)
	if err != nil {
		return "", fmt.Errorf("resolve request cwd %s: %w", req.Cwd, err)
	}
	if resolvedCwd != "" {
		err := os.MkdirAll(resolvedCwd, os.ModePerm)
		if err != nil {
			return "", err
		}
	}

	session := newBashSession(resolvedCwd)
	if err := session.start(); err != nil {
		return "", fmt.Errorf("failed to start bash session: %w", err)
	}

	c.bashSessionClientMap.Store(session.config.Session, session)
	log.Info("created bash session %s", session.config.Session)
	return session.config.Session, nil
}

func (c *Controller) runBashSession(ctx context.Context, request *ExecuteCodeRequest) error {
	session := c.getBashSession(request.Context)
	if session == nil {
		return ErrContextNotFound
	}

	return session.run(ctx, request)
}

func (c *Controller) getBashSession(sessionId string) *bashSession {
	if v, ok := c.bashSessionClientMap.Load(sessionId); ok {
		if s, ok := v.(*bashSession); ok {
			return s
		}
	}
	return nil
}

func (c *Controller) closeBashSession(sessionId string) error {
	session := c.getBashSession(sessionId)
	if session == nil {
		return ErrContextNotFound
	}

	err := session.close()
	if err != nil {
		return err
	}

	c.bashSessionClientMap.Delete(sessionId)
	return nil
}

func (c *Controller) CreateBashSession(req *CreateContextRequest) (string, error) {
	return c.createBashSession(req)
}

func (c *Controller) RunInBashSession(ctx context.Context, req *ExecuteCodeRequest) error {
	return c.runBashSession(ctx, req)
}

func (c *Controller) DeleteBashSession(sessionID string) error {
	return c.closeBashSession(sessionID)
}

func newBashSession(cwd string) *bashSession {
	config := &bashSessionConfig{
		Session:        uuidString(),
		StartupTimeout: 5 * time.Second,
	}

	env := make(map[string]string)
	for _, kv := range os.Environ() {
		if k, v, ok := splitEnvPair(kv); ok {
			env[k] = v
		}
	}

	return &bashSession{
		config: config,
		env:    env,
		cwd:    cwd,
	}
}

func (s *bashSession) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return errors.New("session already started")
	}

	s.started = true
	return nil
}

func (s *bashSession) trackCurrentProcess(pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentProcessPid = pid
}

func (s *bashSession) untrackCurrentProcess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentProcessPid = 0
}

//nolint:gocognit
func (s *bashSession) run(ctx context.Context, request *ExecuteCodeRequest) error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return errors.New("session not started")
	}

	envSnapshot := copyEnvMap(s.env)

	cwd := s.cwd
	// override original cwd if specified
	if request.Cwd != "" {
		expandedCwd, err := pathutil.ExpandPath(request.Cwd)
		if err != nil {
			s.mu.Unlock()
			return fmt.Errorf("resolve cwd: %w", err)
		}
		cwd = expandedCwd
	}
	sessionID := s.config.Session
	s.mu.Unlock()

	startAt := time.Now()
	if request.Hooks.OnExecuteInit != nil {
		request.Hooks.OnExecuteInit(sessionID)
	}

	wait := request.Timeout
	if wait <= 0 {
		wait = 24 * 3600 * time.Second // max to 24 hours
	}

	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	script := buildWrappedScript(request.Code, envSnapshot, cwd)
	scriptFile, err := os.CreateTemp("", "execd_bash_*.sh")
	if err != nil {
		return fmt.Errorf("create script file: %w", err)
	}
	scriptPath := scriptFile.Name()
	if _, err := scriptFile.WriteString(script); err != nil {
		_ = scriptFile.Close()
		return fmt.Errorf("write script file: %w", err)
	}
	if err := scriptFile.Close(); err != nil {
		return fmt.Errorf("close script file: %w", err)
	}

	cmd := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Do not pass envSnapshot via cmd.Env to avoid "argument list too long" when session env is large.
	// Child inherits parent env (nil => default in Go). The script file already has "export K=V" for
	// all session vars at the top, so the session environment is applied when the script runs.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		log.Error("start bash session failed: %v (command: %q)", err, log.SanitizeCommand(request.Code))
		return fmt.Errorf("start bash: %w", err)
	}
	defer s.untrackCurrentProcess()
	s.trackCurrentProcess(cmd.Process.Pid)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var (
		envLines []string
		pwdLine  string
		exitCode *int
		inEnv    bool
	)

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == envDumpStartMarker:
			inEnv = true
		case line == envDumpEndMarker:
			inEnv = false
		case strings.HasPrefix(line, exitMarkerPrefix):
			if code, err := strconv.Atoi(strings.TrimPrefix(line, exitMarkerPrefix)); err == nil {
				exitCode = &code //nolint:ineffassign
			}
		case strings.HasPrefix(line, pwdMarkerPrefix):
			pwdLine = strings.TrimPrefix(line, pwdMarkerPrefix)
		default:
			if inEnv {
				envLines = append(envLines, line)
				continue
			}
			if request.Hooks.OnExecuteStdout != nil {
				request.Hooks.OnExecuteStdout(request.NextEventID(), line)
			}
		}
	}

	scanErr := scanner.Err()
	waitErr := cmd.Wait()

	if scanErr != nil {
		log.Error("read stdout failed: %v (command: %q)", scanErr, log.SanitizeCommand(request.Code))
		return fmt.Errorf("read stdout: %w", scanErr)
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		log.Error("timeout after %s while running command: %q", wait, log.SanitizeCommand(request.Code))
		return fmt.Errorf("timeout after %s", wait)
	}

	if exitCode == nil && cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode() //nolint:staticcheck
		exitCode = &code                    //nolint:ineffassign
	}

	updatedEnv := parseExportDump(envLines)
	s.mu.Lock()
	if len(updatedEnv) > 0 {
		s.env = updatedEnv
	}
	if pwdLine != "" {
		s.cwd = pwdLine
	}
	s.mu.Unlock()

	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		log.Error("command wait failed: %v (command: %q)", waitErr, log.SanitizeCommand(request.Code))
		return waitErr
	}

	userExitCode := 0
	if exitCode != nil {
		userExitCode = *exitCode
	}

	if userExitCode != 0 {
		errMsg := fmt.Sprintf("command exited with code %d", userExitCode)
		if waitErr != nil {
			errMsg = waitErr.Error()
		}
		if request.Hooks.OnExecuteError != nil {
			request.Hooks.OnExecuteError(&execute.ErrorOutput{
				EName:     "CommandExecError",
				EValue:    strconv.Itoa(userExitCode),
				Traceback: []string{errMsg},
			})
		}
		log.Error("CommandExecError: %s (command: %q)", errMsg, log.SanitizeCommand(request.Code))
		return nil
	}

	if request.Hooks.OnExecuteComplete != nil {
		request.Hooks.OnExecuteComplete(time.Since(startAt))
	}

	return nil
}

func buildWrappedScript(command string, env map[string]string, cwd string) string {
	var b strings.Builder

	keys := make([]string, 0, len(env))
	for k := range env {
		v := env[k]
		if isValidEnvKey(k) && !envKeysNotPersisted[k] && len(v) <= maxPersistedEnvValueSize {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("export ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(shellEscape(env[k]))
		b.WriteString("\n")
	}

	if cwd != "" {
		b.WriteString("cd ")
		b.WriteString(shellEscape(cwd))
		b.WriteString("\n")
	}

	b.WriteString(command)
	if !strings.HasSuffix(command, "\n") {
		b.WriteString("\n")
	}

	b.WriteString("__USER_EXIT_CODE__=$?\n")
	b.WriteString("printf \"\\n%s\\n\" \"" + envDumpStartMarker + "\"\n")
	b.WriteString("export -p\n")
	b.WriteString("printf \"%s\\n\" \"" + envDumpEndMarker + "\"\n")
	b.WriteString("printf \"" + pwdMarkerPrefix + "%s\\n\" \"$(pwd)\"\n")
	b.WriteString("printf \"" + exitMarkerPrefix + "%s\\n\" \"$__USER_EXIT_CODE__\"\n")
	b.WriteString("exit \"$__USER_EXIT_CODE__\"\n")

	return b.String()
}

// envKeysNotPersisted are not carried across runs (prompt/display vars).
var envKeysNotPersisted = map[string]bool{
	"PS1": true, "PS2": true, "PS3": true, "PS4": true,
	"PROMPT_COMMAND": true,
}

// maxPersistedEnvValueSize caps single env value length as a safeguard.
const maxPersistedEnvValueSize = 8 * 1024

func parseExportDump(lines []string) map[string]string {
	if len(lines) == 0 {
		return nil
	}
	env := make(map[string]string, len(lines))
	for _, line := range lines {
		k, v, ok := parseExportLine(line)
		if !ok || envKeysNotPersisted[k] || len(v) > maxPersistedEnvValueSize {
			continue
		}
		env[k] = v
	}
	return env
}

func parseExportLine(line string) (string, string, bool) {
	const prefix = "declare -x "
	if !strings.HasPrefix(line, prefix) {
		return "", "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if rest == "" {
		return "", "", false
	}
	name, value := rest, ""
	if eq := strings.Index(rest, "="); eq >= 0 {
		name = rest[:eq]
		raw := rest[eq+1:]
		if unquoted, err := strconv.Unquote(raw); err == nil {
			value = unquoted
		} else {
			value = strings.Trim(raw, `"`)
		}
	}
	if !isValidEnvKey(name) {
		return "", "", false
	}
	return name, value, true
}

func shellEscape(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func isValidEnvKey(key string) bool {
	if key == "" {
		return false
	}

	for i, r := range key {
		if i == 0 {
			if (r < 'A' || (r > 'Z' && r < 'a') || r > 'z') && r != '_' {
				return false
			}
			continue
		}
		if (r < 'A' || (r > 'Z' && r < 'a') || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}

	return true
}

func copyEnvMap(src map[string]string) map[string]string {
	if src == nil {
		return map[string]string{}
	}

	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func splitEnvPair(kv string) (string, string, bool) {
	parts := strings.SplitN(kv, "=", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	if !isValidEnvKey(parts[0]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (s *bashSession) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	pid := s.currentProcessPid
	s.currentProcessPid = 0
	s.started = false
	s.env = nil
	s.cwd = ""

	if pid != 0 {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
			log.Warning("kill session process group %d: %v (process may have already exited)", pid, err)
		}
	}
	return nil
}

func uuidString() string {
	return uuid.New().String()
}
